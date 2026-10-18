package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// probeHeader marks probe requests so route leaves their model alone: the
// fallback probe model does not match keep-models and would otherwise be
// rewritten onto whichever channel is currently selected.
const probeHeader = "X-Alpha-Search-Model-Probe"

// lastAlphaSearch is the unix time of the latest client alpha search seen by
// route. It outlives runtime replacement, and starts at plugin load so the
// first probes run.
var lastAlphaSearch atomic.Int64

func init() {
	lastAlphaSearch.Store(time.Now().Unix())
}

// probeOwnerFile names the probe loop allowed to run in this CPA process.
// When CPA hot-replaces the plugin with a newer version it keeps the old
// instance loaded and never shuts it down (h.retired in
// internal/pluginhost/host.go), so the old loop would keep probing. Every loop
// writes its own token here on start and exits once another loop has
// replaced it.
var probeOwnerFile = filepath.Join(os.TempDir(), "cpa-alpha-search-model.probe-owner")

// runtime is the routing state for one configuration. Channel health starts
// healthy and only changes through probes.
type runtime struct {
	settings  settings
	unhealthy []atomic.Bool

	stopOnce   sync.Once
	cancel     context.CancelFunc
	done       chan struct{}
	ownerToken string
}

func newRuntime(cfg settings) *runtime {
	return &runtime{settings: cfg, unhealthy: make([]atomic.Bool, len(cfg.Channels))}
}

// probing reports whether channel health is tracked. With one channel, or
// without an access key for CPA, every channel counts as healthy.
func (rt *runtime) probing() bool {
	return len(rt.settings.Channels) > 1 && rt.settings.ProbeAPIKey != ""
}

// channelIndex returns the first channel that is neither forced unhealthy nor
// failing its probe, falling back to the first channel when none is usable.
func (rt *runtime) channelIndex() int {
	for i, ch := range rt.settings.Channels {
		if !ch.ForceUnhealthy && !rt.unhealthy[i].Load() {
			return i
		}
	}
	return 0
}

func (rt *runtime) start() {
	if !rt.probing() {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt.cancel = cancel
	rt.done = make(chan struct{})
	rt.ownerToken = strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.Itoa(os.Getpid())
	if errWrite := os.WriteFile(probeOwnerFile, []byte(rt.ownerToken), 0o600); errWrite != nil {
		hostLog("warn", "alpha-search-model: cannot record probe owner, a replaced plugin version may keep probing: "+errWrite.Error())
	}
	names := make([]string, 0, len(rt.settings.Channels))
	for _, ch := range rt.settings.Channels {
		names = append(names, ch.Name)
	}
	hostLog("info", fmt.Sprintf("alpha-search-model: probing channels %s every %ds with model %s",
		strings.Join(names, ","), rt.settings.ProbeIntervalSeconds, rt.settings.ProbeModel))
	go func() {
		defer close(rt.done)
		rt.probeChannels(ctx)
		ticker := time.NewTicker(time.Duration(rt.settings.ProbeIntervalSeconds) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if rt.replaced() {
					hostLog("info", "alpha-search-model: probes stopped, a newer plugin instance took over")
					return
				}
				if rt.idle() {
					continue
				}
				rt.probeChannels(ctx)
			}
		}
	}()
}

// idle reports whether probes are paused because no client alpha search
// arrived within probe-idle-seconds. Health then keeps its last value until
// the next round after traffic returns.
func (rt *runtime) idle() bool {
	idleSeconds := int64(rt.settings.ProbeIdleSeconds)
	return idleSeconds > 0 && time.Now().Unix()-lastAlphaSearch.Load() > idleSeconds
}

// replaced reports whether another probe loop has recorded itself as owner.
// An unreadable owner file keeps the loop running.
func (rt *runtime) replaced() bool {
	owner, errRead := os.ReadFile(probeOwnerFile)
	return errRead == nil && string(owner) != rt.ownerToken
}

// stop cancels the probe loop and waits for it, so a replaced or unloaded
// configuration never keeps sending upstream searches.
func (rt *runtime) stop() {
	rt.stopOnce.Do(func() {
		if rt.cancel == nil {
			return
		}
		rt.cancel()
		<-rt.done
	})
}

func (rt *runtime) probeChannels(ctx context.Context) {
	client := &http.Client{
		// The CPA container exports HTTP(S)_PROXY; probes target CPA itself.
		Transport: &http.Transport{Proxy: nil},
		Timeout:   time.Duration(rt.settings.ProbeTimeoutSeconds) * time.Second,
	}
	defer client.CloseIdleConnections()
	for i, ch := range rt.settings.Channels {
		if ch.ForceUnhealthy {
			continue
		}
		model := strings.ReplaceAll(ch.Model, modelPlaceholder, rt.settings.ProbeModel)
		errProbe := probeChannel(ctx, client, rt.settings, ch.Name, model)
		if ctx.Err() != nil {
			return
		}
		// Failing to reach CPA itself, as during startup before it listens,
		// says nothing about the channel.
		var errOp *net.OpError
		if errors.As(errProbe, &errOp) && errOp.Op == "dial" {
			hostLog("warn", "alpha-search-model: probe could not reach CPA, channel health unchanged: "+errProbe.Error())
		} else {
			wasUnhealthy := rt.unhealthy[i].Swap(errProbe != nil)
			switch {
			case errProbe != nil && !wasUnhealthy:
				hostLog("warn", fmt.Sprintf("alpha-search-model: channel %s unhealthy, probe %s failed: %v", ch.Name, model, errProbe))
			case errProbe == nil && wasUnhealthy:
				hostLog("info", fmt.Sprintf("alpha-search-model: channel %s recovered, probe %s succeeded", ch.Name, model))
			}
		}
		// Routing uses the first healthy channel, so channels after it do not
		// matter until it fails; skipping them keeps the fallback's searches
		// to outages.
		if !rt.unhealthy[i].Load() {
			return
		}
	}
}

// probeChannel sends one short search through CPA. Success means HTTP 200
// with a JSON body carrying a non-empty output, which also rejects HTML pages
// some upstreams return with status 200.
func probeChannel(ctx context.Context, client *http.Client, cfg settings, channelName, model string) error {
	body, errMarshal := json.Marshal(map[string]any{
		"id":    "alpha-search-model-probe-" + channelName,
		"model": model,
		"input": []map[string]any{{
			"type": "message", "role": "user",
			"content": []map[string]any{{"type": "input_text", "text": "What is the current UTC date?"}},
		}},
		"commands": map[string]any{
			"search_query":    []map[string]any{{"q": "current UTC date"}},
			"response_length": "short",
		},
		"max_output_tokens": 256,
	})
	if errMarshal != nil {
		return errMarshal
	}
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, cfg.ProbeURL, bytes.NewReader(body))
	if errRequest != nil {
		return errRequest
	}
	req.Header.Set("Authorization", "Bearer "+cfg.ProbeAPIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", cfg.ProbeUserAgent)
	req.Header.Set(probeHeader, "1")
	resp, errDo := client.Do(req)
	if errDo != nil {
		return errDo
	}
	defer resp.Body.Close()
	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return errRead
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, snippet(raw))
	}
	var result struct {
		Output string `json:"output"`
	}
	if errUnmarshal := json.Unmarshal(raw, &result); errUnmarshal != nil || strings.TrimSpace(result.Output) == "" {
		return fmt.Errorf("status 200 without search output: %s", snippet(raw))
	}
	return nil
}

func snippet(raw []byte) string {
	text := strings.Join(strings.Fields(string(raw)), " ")
	if len(text) > 200 {
		text = text[:200]
	}
	return text
}

func isProbe(headers map[string][]string) bool {
	for name, values := range headers {
		if strings.EqualFold(name, probeHeader) && len(values) > 0 {
			return true
		}
	}
	return false
}
