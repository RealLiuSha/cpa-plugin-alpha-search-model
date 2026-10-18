// Package main implements a CLIProxyAPI plugin that chooses the model, and so
// the credential, for Codex web search (POST /v1/alpha/search).
//
// CPA serves alpha search only from codex credentials that enable
// alpha-search and register the requested model
// (sdk/cliproxy/auth/credential_policy.go). Before selecting the credential,
// CPA asks model routers for the selection model with SourceFormat
// "codex-alpha-search" and accepts only a provider route to codex
// (codexAlphaSearchSelectionModel in internal/api/server_routes.go). The
// plugin answers that call in two steps:
//
//  1. A model outside keep-models (a Claude, Gemini or Grok session) is
//     replaced by target-model, since no codex credential registers it.
//  2. The model is put through the template of the first healthy channel,
//     for example "{model}" for the primary upstream and
//     "alphasearch-nb-{model}" for a fallback whose credential registers only
//     those aliases. CPA resolves the alias back to the upstream name before
//     forwarding the search.
//
// CPA sends a search once and never reports its outcome to plugins, so
// channel health comes from active probes sent through CPA's own
// /v1/alpha/search (probe.go). Every other request is left unhandled, so chat,
// image and model-list traffic is untouched.
//
// The wire types mirror sdk/pluginapi and sdk/pluginabi of CLIProxyAPI v8.0.32.
// pluginapi structs carry no json tags, so route and metadata fields use their
// Go names; the lifecycle and registration envelopes use the tags from
// internal/pluginhost/rpc_schema.go.
//
// Configuration under plugins.configs.alpha-search-model:
//
//	alpha-search-model:
//	    enabled: true
//	    target-model: gpt-6.1-sol        # default
//	    keep-models: ["gpt-*"]           # default; path.Match patterns, case-insensitive
//	    channels:                        # default: one channel "{model}", never probed
//	        - name: primary
//	          model: "{model}"
//	        - name: fallback
//	          model: "alphasearch-nb-{model}"
//	          force-unhealthy: false     # true skips the channel, for failover tests
//	    probe-api-key: "<CPA access key>" # probing is off while empty
//	    probe-interval-seconds: 60       # default, minimum 10
//	    probe-timeout-seconds: 15        # default
//	    probe-model: ""                  # default target-model
//	    probe-url: http://127.0.0.1:8317/v1/alpha/search
//	    probe-user-agent: codex_cli_rs/0.130.0 (alpha-search-model probe)
//	    probe-idle-seconds: 0            # default; >0 pauses probes after that long without searches
//
// Probes run in channel order and stop at the first healthy channel, so a
// fallback is only probed while the channels before it fail.
package main

import (
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

// pluginID must match the library filename (alpha-search-model-v<version>.so),
// which CPA uses as the plugin ID and as the key under plugins.configs.
const (
	pluginID      = "alpha-search-model"
	pluginVersion = "0.2.0"
	// schemaVersion is pluginabi.SchemaVersion in CLIProxyAPI v8.0.32.
	schemaVersion = 6

	alphaSearchSourceFormat = "codex-alpha-search"
	modelPlaceholder        = "{model}"

	defaultTargetModel          = "gpt-6.1-sol"
	defaultProbeIntervalSeconds = 60
	minProbeIntervalSeconds     = 10
	defaultProbeTimeoutSeconds  = 15
	defaultProbeURL             = "http://127.0.0.1:8317/v1/alpha/search"
	// CPA forwards the client User-Agent upstream, and Cloudflare in front of
	// some upstreams rejects generic HTTP client agents with 403.
	defaultProbeUserAgent = "codex_cli_rs/0.130.0 (alpha-search-model probe)"
)

var defaultKeepModels = []string{"gpt-*"}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type registration struct {
	SchemaVersion uint32       `json:"schema_version"`
	Metadata      metadata     `json:"metadata"`
	Capabilities  capabilities `json:"capabilities"`
}

type metadata struct {
	Name             string
	Version          string
	Author           string
	GitHubRepository string
	ConfigFields     []configField
}

type configField struct {
	Name        string
	Type        string
	Default     any
	Description string
}

type capabilities struct {
	ModelRouter bool `json:"model_router"`
}

// modelRouteRequest decodes only the fields the decision needs. Leaving out
// Body avoids base64-decoding the multi-megabyte Codex payloads that every
// chat request sends through model.route.
type modelRouteRequest struct {
	SourceFormat   string
	RequestedModel string
	Headers        map[string][]string
}

type modelRouteResponse struct {
	Handled     bool
	TargetKind  string `json:",omitempty"`
	Target      string `json:",omitempty"`
	TargetModel string `json:",omitempty"`
	Reason      string `json:",omitempty"`
}

type settings struct {
	TargetModel          string    `yaml:"target-model"`
	KeepModels           []string  `yaml:"keep-models"`
	Channels             []channel `yaml:"channels"`
	ProbeAPIKey          string    `yaml:"probe-api-key"`
	ProbeIntervalSeconds int       `yaml:"probe-interval-seconds"`
	ProbeTimeoutSeconds  int       `yaml:"probe-timeout-seconds"`
	ProbeModel           string    `yaml:"probe-model"`
	ProbeURL             string    `yaml:"probe-url"`
	ProbeUserAgent       string    `yaml:"probe-user-agent"`
	ProbeIdleSeconds     int       `yaml:"probe-idle-seconds"`
}

type channel struct {
	Name           string `yaml:"name"`
	Model          string `yaml:"model"`
	ForceUnhealthy bool   `yaml:"force-unhealthy"`
}

// active holds the runtime built from the last valid configuration.
// model.route runs concurrently with plugin.reconfigure, so it is swapped
// atomically.
var active atomic.Pointer[runtime]

func init() {
	parsed, _ := parseSettings(nil)
	active.Store(newRuntime(parsed))
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		parsed, errParse := parseSettings(request)
		if errParse != nil {
			return errorEnvelope("invalid_config", errParse.Error()), nil
		}
		applySettings(parsed)
		return okEnvelope(pluginRegistration())
	case "plugin.quiesce":
		return okEnvelope(struct{}{})
	case "plugin.shutdown":
		active.Load().stop()
		return okEnvelope(struct{}{})
	case "model.route":
		var req modelRouteRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		return okEnvelope(active.Load().route(req))
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// applySettings swaps in a new runtime only when the configuration changed.
// CPA sends plugin.reconfigure on every config reload, including panel saves
// that do not touch this block, and those must keep channel health and the
// probe schedule.
func applySettings(parsed settings) {
	current := active.Load()
	if reflect.DeepEqual(current.settings, parsed) {
		return
	}
	next := newRuntime(parsed)
	current.stop()
	active.Store(next)
	next.start()
}

// parseSettings reads the plugin's YAML block. Host-owned keys such as enabled
// and priority are ignored; absent or empty fields fall back to the defaults.
func parseSettings(request []byte) (settings, error) {
	var lifecycle struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &lifecycle); errUnmarshal != nil {
			return settings{}, errUnmarshal
		}
	}
	var parsed settings
	if len(lifecycle.ConfigYAML) > 0 {
		if errYAML := yaml.Unmarshal(lifecycle.ConfigYAML, &parsed); errYAML != nil {
			return settings{}, errYAML
		}
	}

	parsed.TargetModel = strings.TrimSpace(parsed.TargetModel)
	if parsed.TargetModel == "" {
		parsed.TargetModel = defaultTargetModel
	}
	keepModels := make([]string, 0, len(parsed.KeepModels))
	for _, pattern := range parsed.KeepModels {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		if pattern == "" {
			continue
		}
		if _, errMatch := path.Match(pattern, ""); errMatch != nil {
			return settings{}, fmt.Errorf("keep-models pattern %q: %w", pattern, errMatch)
		}
		keepModels = append(keepModels, pattern)
	}
	if len(keepModels) == 0 {
		keepModels = defaultKeepModels
	}
	parsed.KeepModels = keepModels

	if len(parsed.Channels) == 0 {
		parsed.Channels = []channel{{Name: "default", Model: modelPlaceholder}}
	}
	names := make(map[string]bool, len(parsed.Channels))
	for i := range parsed.Channels {
		ch := &parsed.Channels[i]
		ch.Name = strings.TrimSpace(ch.Name)
		ch.Model = strings.TrimSpace(ch.Model)
		if ch.Name == "" || ch.Model == "" {
			return settings{}, fmt.Errorf("channels[%d] needs a name and a model template", i)
		}
		if names[ch.Name] {
			return settings{}, fmt.Errorf("channels[%d]: duplicate name %q", i, ch.Name)
		}
		names[ch.Name] = true
	}

	parsed.ProbeAPIKey = strings.TrimSpace(parsed.ProbeAPIKey)
	if parsed.ProbeIntervalSeconds == 0 {
		parsed.ProbeIntervalSeconds = defaultProbeIntervalSeconds
	}
	if parsed.ProbeIntervalSeconds < minProbeIntervalSeconds {
		return settings{}, fmt.Errorf("probe-interval-seconds must be at least %d", minProbeIntervalSeconds)
	}
	if parsed.ProbeTimeoutSeconds <= 0 {
		parsed.ProbeTimeoutSeconds = defaultProbeTimeoutSeconds
	}
	parsed.ProbeModel = strings.TrimSpace(parsed.ProbeModel)
	if parsed.ProbeModel == "" {
		parsed.ProbeModel = parsed.TargetModel
	}
	parsed.ProbeURL = strings.TrimSpace(parsed.ProbeURL)
	if parsed.ProbeURL == "" {
		parsed.ProbeURL = defaultProbeURL
	}
	if parsed.ProbeIdleSeconds < 0 {
		return settings{}, fmt.Errorf("probe-idle-seconds must not be negative")
	}
	parsed.ProbeUserAgent = strings.TrimSpace(parsed.ProbeUserAgent)
	if parsed.ProbeUserAgent == "" {
		parsed.ProbeUserAgent = defaultProbeUserAgent
	}
	return parsed, nil
}

// route handles alpha search requests only. A handled answer must be a
// provider route to codex with a model: CPA turns any other handled answer
// into an error for the client.
func (rt *runtime) route(req modelRouteRequest) modelRouteResponse {
	model := strings.TrimSpace(req.RequestedModel)
	if req.SourceFormat != alphaSearchSourceFormat || isProbe(req.Headers) {
		return modelRouteResponse{}
	}
	lastAlphaSearch.Store(time.Now().Unix())
	if model == "" {
		return modelRouteResponse{}
	}
	cfg := rt.settings
	mapped := model
	if !keepModel(cfg.KeepModels, model) {
		mapped = cfg.TargetModel
	}
	ch := cfg.Channels[rt.channelIndex()]
	selected := strings.ReplaceAll(ch.Model, modelPlaceholder, mapped)
	if strings.EqualFold(selected, model) {
		return modelRouteResponse{}
	}
	return modelRouteResponse{
		Handled:     true,
		TargetKind:  "provider",
		Target:      "codex",
		TargetModel: selected,
		Reason:      "alpha search model " + model + " routed to " + selected + " via channel " + ch.Name,
	}
}

func keepModel(patterns []string, model string) bool {
	lowerModel := strings.ToLower(model)
	for _, pattern := range patterns {
		if matched, _ := path.Match(pattern, lowerModel); matched {
			return true
		}
	}
	return false
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: schemaVersion,
		Metadata: metadata{
			// CPA registers a plugin only when Name, Version, Author and
			// GitHubRepository are all non-empty (validPlugin in
			// internal/pluginhost/host.go).
			Name:             pluginID,
			Version:          pluginVersion,
			Author:           "freedieliu",
			GitHubRepository: "local/cpa-plugin-alpha-search-model",
			ConfigFields: []configField{
				{Name: "target-model", Type: "string", Default: defaultTargetModel,
					Description: "Model used for alpha search requests from models outside keep-models."},
				{Name: "keep-models", Type: "array", Default: defaultKeepModels,
					Description: "Model patterns (path.Match, case-insensitive) whose alpha search requests keep their own model."},
				{Name: "channels", Type: "array",
					Description: "Ordered channels with name, model template ({model} is replaced) and force-unhealthy; the first healthy channel serves alpha search."},
				{Name: "probe-api-key", Type: "string",
					Description: "CPA access key for channel probes; probing is off while empty."},
				{Name: "probe-interval-seconds", Type: "integer", Default: defaultProbeIntervalSeconds,
					Description: "Seconds between probes of each channel, at least 10."},
				{Name: "probe-timeout-seconds", Type: "integer", Default: defaultProbeTimeoutSeconds,
					Description: "Seconds before a probe counts as failed."},
				{Name: "probe-model", Type: "string",
					Description: "Model put through each channel template for probes; defaults to target-model."},
				{Name: "probe-url", Type: "string", Default: defaultProbeURL,
					Description: "CPA alpha search endpoint used by probes."},
				{Name: "probe-user-agent", Type: "string", Default: defaultProbeUserAgent,
					Description: "User-Agent sent by probes; CPA forwards it upstream."},
				{Name: "probe-idle-seconds", Type: "integer", Default: 0,
					Description: "Pause probes when no alpha search arrived for this many seconds; 0 always probes."},
			},
		},
		Capabilities: capabilities{ModelRouter: true},
	}
}

func okEnvelope(result any) ([]byte, error) {
	raw, errMarshal := json.Marshal(result)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}
