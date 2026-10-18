package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// callRoute sends a model.route request shaped like CPA's rpcModelRouteRequest,
// including a body the plugin must ignore.
func callRoute(t *testing.T, sourceFormat, model string) modelRouteResponse {
	t.Helper()
	request, errMarshal := json.Marshal(map[string]any{
		"SourceFormat":     sourceFormat,
		"RequestedModel":   model,
		"Body":             []byte(`{"model":"` + model + `","input":[]}`),
		"Metadata":         map[string]any{"requested_model": model},
		"host_callback_id": "cb-1",
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	raw, errHandle := handleMethod("model.route", request)
	if errHandle != nil {
		t.Fatalf("model.route: %v", errHandle)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil || !env.OK {
		t.Fatalf("model.route envelope %s: %v", raw, errUnmarshal)
	}
	var resp modelRouteResponse
	if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	return resp
}

func configure(t *testing.T, method, configYAML string) envelope {
	t.Helper()
	request, errMarshal := json.Marshal(map[string]any{"config_yaml": []byte(configYAML), "schema_version": 6})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	raw, errHandle := handleMethod(method, request)
	if errHandle != nil {
		t.Fatalf("%s: %v", method, errHandle)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	return env
}

func TestRouteModel(t *testing.T) {
	rewritten := func(target string) modelRouteResponse {
		return modelRouteResponse{Handled: true, TargetKind: "provider", Target: "codex", TargetModel: target}
	}
	tests := []struct {
		name         string
		configYAML   string
		sourceFormat string
		model        string
		want         modelRouteResponse
	}{
		{"claude search uses default target", "enabled: true\n", "codex-alpha-search", "claude-sonnet-5.5", rewritten("gpt-6.1-sol")},
		{"gemini search", "enabled: true\n", "codex-alpha-search", "gemini-3.8", rewritten("gpt-6.1-sol")},
		{"grok search", "enabled: true\n", "codex-alpha-search", "grok-4.6", rewritten("gpt-6.1-sol")},
		{"gpt search keeps its model", "enabled: true\n", "codex-alpha-search", "gpt-6-luna", modelRouteResponse{}},
		{"keep match is case-insensitive", "enabled: true\n", "codex-alpha-search", "GPT-6-Sol", modelRouteResponse{}},
		{"empty model is left alone", "enabled: true\n", "codex-alpha-search", "", modelRouteResponse{}},
		{"claude chat is not routed", "enabled: true\n", "openai-response", "claude-sonnet-5.5", modelRouteResponse{}},
		{"claude messages are not routed", "enabled: true\n", "claude", "claude-sonnet-5.5", modelRouteResponse{}},
		{
			"custom target and keep list",
			"enabled: true\npriority: 3\ntarget-model: \" gpt-6-sol \"\nkeep-models: [\"gpt-*\", \"glm-*\"]\nunknown-panel-key: 1\n",
			"codex-alpha-search", "glm-5.3", modelRouteResponse{},
		},
		{
			"custom target applies to models outside the keep list",
			"target-model: gpt-6-sol\nkeep-models: [\"gpt-*\", \"glm-*\"]\n",
			"codex-alpha-search", "claude-opus-5.5", rewritten("gpt-6-sol"),
		},
		{"model equal to target is left alone", "target-model: claude-x\nkeep-models: []\n", "codex-alpha-search", "Claude-X", modelRouteResponse{}},
		{"empty keep list falls back to gpt-*", "keep-models: []\n", "codex-alpha-search", "gpt-6-astra", modelRouteResponse{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if env := configure(t, "plugin.register", tt.configYAML); !env.OK {
				t.Fatalf("register failed: %+v", env.Error)
			}
			got := callRoute(t, tt.sourceFormat, tt.model)
			got.Reason = ""
			if got != tt.want {
				t.Fatalf("route = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestRegistrationContract(t *testing.T) {
	env := configure(t, "plugin.register", "enabled: true\n")
	if !env.OK {
		t.Fatalf("register failed: %+v", env.Error)
	}
	var reg struct {
		SchemaVersion uint32 `json:"schema_version"`
		Metadata      struct {
			Name, Version, Author, GitHubRepository string
		} `json:"metadata"`
		Capabilities map[string]bool `json:"capabilities"`
	}
	if errUnmarshal := json.Unmarshal(env.Result, &reg); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if reg.SchemaVersion != 6 || reg.Metadata.Name != pluginID || reg.Metadata.Version == "" ||
		reg.Metadata.Author == "" || reg.Metadata.GitHubRepository == "" {
		t.Fatalf("registration = %+v", reg)
	}
	if len(reg.Capabilities) != 1 || !reg.Capabilities["model_router"] {
		t.Fatalf("capabilities = %v, want only model_router", reg.Capabilities)
	}
}

func TestInvalidReconfigureKeepsLastGoodConfig(t *testing.T) {
	if env := configure(t, "plugin.register", "target-model: gpt-6-sol\n"); !env.OK {
		t.Fatalf("register failed: %+v", env.Error)
	}
	for _, configYAML := range []string{"keep-models: [\"gpt-[\"]\n", "target-model: [\n"} {
		env := configure(t, "plugin.reconfigure", configYAML)
		if env.OK || env.Error == nil || env.Error.Code != "invalid_config" {
			t.Fatalf("reconfigure %q = %+v, want invalid_config", configYAML, env)
		}
	}
	got := callRoute(t, "codex-alpha-search", "grok-4.7")
	if !got.Handled || got.TargetModel != "gpt-6-sol" {
		t.Fatalf("route after invalid reconfigure = %+v, want gpt-6-sol", got)
	}
}

// fakeCPA stands in for CPA's /v1/alpha/search during probes. status maps a
// probe model to the response it gets; unknown models get 503.
type fakeCPA struct {
	t      *testing.T
	mu     sync.Mutex
	status map[string]int
	seen   map[string]int
	hits   chan string
}

func (f *fakeCPA) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model    string         `json:"model"`
		Commands map[string]any `json:"commands"`
	}
	if errDecode := json.NewDecoder(r.Body).Decode(&body); errDecode != nil {
		f.t.Errorf("decode probe body: %v", errDecode)
	}
	if r.Header.Get("Authorization") != "Bearer probe-key" || r.Header.Get(probeHeader) != "1" ||
		r.Header.Get("User-Agent") != defaultProbeUserAgent || body.Commands["response_length"] != "short" {
		f.t.Errorf("probe request headers %v commands %v", r.Header, body.Commands)
	}
	f.mu.Lock()
	status, ok := f.status[body.Model]
	if f.seen == nil {
		f.seen = map[string]int{}
	}
	f.seen[body.Model]++
	f.mu.Unlock()
	select {
	case f.hits <- body.Model:
	default:
	}
	switch {
	case !ok:
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"auth_not_found: no auth available"}`))
	case status == http.StatusOK:
		_, _ = w.Write([]byte(`{"output":"2026-10-18","results":[]}`))
	case status == http.StatusNoContent:
		// An upstream that answers 200 with an HTML page.
		_, _ = w.Write([]byte(`<html>home</html>`))
	default:
		w.WriteHeader(status)
	}
}

func (f *fakeCPA) set(model string, status int) {
	f.mu.Lock()
	f.status[model] = status
	f.mu.Unlock()
}

const channelsYAML = `target-model: gpt-6-astra
channels:
  - name: primary
    model: "{model}"
  - name: fallback
    model: "alphasearch-nb-{model}"
probe-api-key: probe-key
`

func TestChannelRouting(t *testing.T) {
	tests := []struct {
		name       string
		configYAML string
		model      string
		headers    map[string][]string
		want       string
	}{
		{"primary keeps gpt model", channelsYAML, "gpt-6-sol", nil, ""},
		{"primary maps claude to target", channelsYAML, "claude-sonnet-5.5", nil, "gpt-6-astra"},
		{"forced primary sends gpt to fallback", forcePrimary(channelsYAML), "gpt-6-sol", nil, "alphasearch-nb-gpt-6-sol"},
		{"forced primary sends claude to fallback target", forcePrimary(channelsYAML), "grok-4.6", nil, "alphasearch-nb-gpt-6-astra"},
		{"probe requests keep their model", forcePrimary(channelsYAML), "alphasearch-nb-gpt-6-astra", map[string][]string{probeHeader: {"1"}}, ""},
		{"all channels forced falls back to primary", forceAll(channelsYAML), "gemini-3.8", nil, "gpt-6-astra"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := mustParse(t, tt.configYAML)
			got := newRuntime(cfg).route(modelRouteRequest{SourceFormat: alphaSearchSourceFormat, RequestedModel: tt.model, Headers: tt.headers})
			if got.TargetModel != tt.want || got.Handled != (tt.want != "") {
				t.Fatalf("route(%s) = %+v, want target %q", tt.model, got, tt.want)
			}
		})
	}
}

func TestProbesSwitchChannels(t *testing.T) {
	cpa := &fakeCPA{t: t, status: map[string]int{"gpt-6-astra": 200, "alphasearch-nb-gpt-6-astra": 200}, hits: make(chan string, 8)}
	server := httptest.NewServer(cpa)
	defer server.Close()
	rt := newRuntime(mustParse(t, channelsYAML+"probe-url: "+server.URL+"\n"))
	search := func() string {
		return rt.route(modelRouteRequest{SourceFormat: alphaSearchSourceFormat, RequestedModel: "gpt-6-luna"}).TargetModel
	}

	rt.probeChannels(context.Background())
	if got := search(); got != "" {
		t.Fatalf("healthy primary routed gpt-6-luna to %q", got)
	}
	if cpa.seen["alphasearch-nb-gpt-6-astra"] != 0 {
		t.Fatalf("fallback probed while the primary was healthy: %v", cpa.seen)
	}
	for _, failure := range []int{http.StatusForbidden, http.StatusNoContent} {
		cpa.set("gpt-6-astra", failure)
		rt.probeChannels(context.Background())
		if got := search(); got != "alphasearch-nb-gpt-6-luna" {
			t.Fatalf("primary failing with %d routed to %q, want fallback", failure, got)
		}
	}
	cpa.set("alphasearch-nb-gpt-6-astra", http.StatusBadGateway)
	rt.probeChannels(context.Background())
	if got := search(); got != "" {
		t.Fatalf("all channels failing routed to %q, want primary", got)
	}
	cpa.set("gpt-6-astra", http.StatusOK)
	rt.probeChannels(context.Background())
	if got := search(); got != "" || rt.channelIndex() != 0 {
		t.Fatalf("recovered primary routed to %q", got)
	}
}

func TestProbeLoopLifecycle(t *testing.T) {
	cpa := &fakeCPA{t: t, status: map[string]int{"gpt-6-astra": 200}, hits: make(chan string, 8)}
	server := httptest.NewServer(cpa)
	defer server.Close()
	configYAML := channelsYAML + "probe-url: " + server.URL + "\n"
	previousOwnerFile := probeOwnerFile
	probeOwnerFile = filepath.Join(t.TempDir(), "probe-owner")
	t.Cleanup(func() { probeOwnerFile = previousOwnerFile })

	if env := configure(t, "plugin.register", configYAML); !env.OK {
		t.Fatalf("register failed: %+v", env.Error)
	}
	started := active.Load()
	select {
	case <-cpa.hits:
	case <-time.After(5 * time.Second):
		t.Fatal("probe loop did not probe on start")
	}
	if env := configure(t, "plugin.reconfigure", "enabled: true\npriority: 0\n"+configYAML); !env.OK || active.Load() != started {
		t.Fatalf("unchanged reconfigure replaced the runtime: %+v", env)
	}
	if env := configure(t, "plugin.reconfigure", configYAML+"probe-interval-seconds: 30\n"); !env.OK || active.Load() == started {
		t.Fatalf("changed reconfigure kept the runtime: %+v", env)
	}
	assertStopped(t, started)

	replaced := active.Load()
	// A loop in a retired plugin instance sees the newer loop's token.
	if !started.replaced() || replaced.replaced() {
		t.Fatalf("owner token: started replaced=%v, current replaced=%v", started.replaced(), replaced.replaced())
	}
	if _, errShutdown := handleMethod("plugin.shutdown", nil); errShutdown != nil {
		t.Fatal(errShutdown)
	}
	assertStopped(t, replaced)
}

func assertStopped(t *testing.T, rt *runtime) {
	t.Helper()
	select {
	case <-rt.done:
	default:
		t.Fatal("probe loop still running")
	}
}

func TestInvalidChannelConfig(t *testing.T) {
	for _, configYAML := range []string{
		"channels: [{name: a, model: '{model}'}, {name: a, model: 'x-{model}'}]\n",
		"channels: [{name: a}]\n",
		"probe-interval-seconds: 5\n",
		"probe-idle-seconds: -1\n",
	} {
		if _, errParse := parseSettings(lifecycleRequest(t, configYAML)); errParse == nil {
			t.Fatalf("parseSettings(%q) accepted an invalid config", configYAML)
		}
	}
}

func forcePrimary(configYAML string) string {
	return strings.Replace(configYAML, "model: \"{model}\"\n", "model: \"{model}\"\n    force-unhealthy: true\n", 1)
}

func forceAll(configYAML string) string {
	return strings.ReplaceAll(configYAML, "{model}\"\n", "{model}\"\n    force-unhealthy: true\n")
}

func mustParse(t *testing.T, configYAML string) settings {
	t.Helper()
	cfg, errParse := parseSettings(lifecycleRequest(t, configYAML))
	if errParse != nil {
		t.Fatal(errParse)
	}
	return cfg
}

func lifecycleRequest(t *testing.T, configYAML string) []byte {
	t.Helper()
	request, errMarshal := json.Marshal(map[string]any{"config_yaml": []byte(configYAML)})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	return request
}

func TestProbeKeepsHealthWhenCPAIsUnreachable(t *testing.T) {
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatal(errListen)
	}
	closedURL := "http://" + listener.Addr().String() + "/v1/alpha/search"
	_ = listener.Close()
	rt := newRuntime(mustParse(t, channelsYAML+"probe-url: "+closedURL+"\n"))
	rt.probeChannels(context.Background())
	for i := range rt.unhealthy {
		if rt.unhealthy[i].Load() {
			t.Fatalf("channel %d marked unhealthy after a dial error", i)
		}
	}
}

func TestIdleProbesPause(t *testing.T) {
	rt := newRuntime(mustParse(t, channelsYAML+"probe-idle-seconds: 600\n"))
	previousSearch := lastAlphaSearch.Load()
	t.Cleanup(func() { lastAlphaSearch.Store(previousSearch) })
	lastAlphaSearch.Store(time.Now().Add(-time.Hour).Unix())
	if !rt.idle() {
		t.Fatal("probes not idle after an hour without searches")
	}
	rt.route(modelRouteRequest{SourceFormat: alphaSearchSourceFormat, RequestedModel: "gpt-6-sol", Headers: map[string][]string{probeHeader: {"1"}}})
	rt.route(modelRouteRequest{SourceFormat: "openai-response", RequestedModel: "gpt-6-sol"})
	if !rt.idle() {
		t.Fatal("probe or chat requests counted as search traffic")
	}
	rt.route(modelRouteRequest{SourceFormat: alphaSearchSourceFormat, RequestedModel: "gpt-6-sol"})
	if rt.idle() {
		t.Fatal("probes still idle after a client search")
	}
	if newRuntime(mustParse(t, channelsYAML)).idle() {
		t.Fatal("probe-idle-seconds 0 paused probes")
	}
}
