package main

// Supervision of the Flagsmith Edge Proxy child process, and the evaluation probe that makes
// POST /start honour its contract.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"
)

const (
	proxyPort     = 8000
	proxyConfig   = "/app/config.json"
	proxyCmdName  = "edge-proxy-serve"
	probeTimeout  = 30 * time.Second
	probeInterval = 50 * time.Millisecond
)

type edgeProxy struct {
	mu  sync.Mutex
	cmd *exec.Cmd

	serverKey string
	clientKey string
	// upstreamURL is the launchpad's own environment-document endpoint. The Edge Proxy polls it
	// exactly as it would poll the real Flagsmith API: GET {api_url}/environment-document/ with an
	// X-Environment-Key header. Verified against Flagsmith/edge-proxy@main
	// src/edge_proxy/environments.py _fetch_document.
	upstreamURL string
	pollSeconds int

	client *http.Client
}

func newEdgeProxy(serverKey, clientKey, upstreamURL string, pollSeconds int) *edgeProxy {
	return &edgeProxy{
		serverKey:   serverKey,
		clientKey:   clientKey,
		upstreamURL: upstreamURL,
		pollSeconds: pollSeconds,
		client:      &http.Client{Timeout: 5 * time.Second},
	}
}

// writeConfig renders the Edge Proxy's config.json.
//
// Two settings are deliberate and must not be "tidied up":
//
//   - endpoint_caches is pinned to use_cache:false for both flags and identities. It defaults to
//     null (no caching) today, but an LRU in front of evaluation is exactly the trap that made
//     flagd's RPC resolver return stale repeat evaluations, and the repeat-evaluation scenarios
//     would hit it. Pin it rather than inherit it.
//   - api_poll_frequency_seconds is 1. The default is 10, and nothing about POST /change would
//     complete in a sane time at that rate.
func (p *edgeProxy) writeConfig() error {
	cfg := map[string]any{
		"environment_key_pairs": []map[string]string{{
			"server_side_key": p.serverKey,
			"client_side_key": p.clientKey,
		}},
		"api_url":                    p.upstreamURL,
		"api_poll_frequency_seconds": p.pollSeconds,
		"api_poll_timeout_seconds":   2,
		"endpoint_caches": map[string]any{
			"flags":      map[string]any{"use_cache": false},
			"identities": map[string]any{"use_cache": false},
		},
		"logging":      map[string]any{"log_level": "INFO"},
		"server":       map[string]any{"host": "0.0.0.0", "port": proxyPort},
		"health_check": map[string]any{"environment_update_grace_period_seconds": 30},
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(proxyConfig, b, 0o644)
}

func (p *edgeProxy) running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cmd != nil && p.cmd.Process != nil
}

// start launches the proxy. It does NOT wait for readiness; callers use probe for that.
func (p *edgeProxy) start() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != nil {
		return nil
	}
	cmd := exec.Command(proxyCmdName)
	cmd.Env = append(os.Environ(), "CONFIG_PATH="+proxyConfig)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", proxyCmdName, err)
	}
	p.cmd = cmd
	go func() { _ = cmd.Wait() }() // reap; launchpad is PID 1
	return nil
}

// stop kills the proxy process. The container keeps running -- the control API is explicit that
// simulating an outage MUST NOT stop the container, because dynamically mapped host ports do not
// survive it.
func (p *edgeProxy) stop() error {
	p.mu.Lock()
	cmd := p.cmd
	p.cmd = nil
	p.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Kill(); err != nil {
		return err
	}
	// Wait for the port to actually be released, otherwise a fast /restart re-binds too early.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := p.evaluate("boolean-flag"); err != nil {
			return nil
		}
		time.Sleep(probeInterval)
	}
	return nil
}

type flagResponse struct {
	Feature struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"feature"`
	Enabled bool `json:"enabled"`
	Value   any  `json:"feature_state_value"`
}

// evaluate performs a real flag evaluation through the proxy, exactly as a provider would:
// GET /api/v1/flags/?feature=<key> with an X-Environment-Key header.
func (p *edgeProxy) evaluate(key string) (*flagResponse, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/api/v1/flags/?feature=%s", proxyPort, key)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Environment-Key", p.serverKey)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("evaluate %s: status %d: %s", key, resp.StatusCode, string(body))
	}
	var fr flagResponse
	if err := json.Unmarshal(body, &fr); err != nil {
		return nil, fmt.Errorf("evaluate %s: %w", key, err)
	}
	return &fr, nil
}

// probeServing blocks until the proxy actually serves `want` for `key`, or the deadline passes.
//
// This is the requirement POST /start exists to satisfy, and the one flagd-testbed got wrong
// (flagd-testbed#394): a 200 from /start is a promise that the very next evaluation resolves
// against the new baseline. A backend that answers a readiness probe while its flag store is
// still empty fails a stateless provider on essentially every scenario, which reads as a
// catastrophically broken provider rather than as a racing testbed.
//
// The probe key is derived from the document being served, not hardcoded, for the same reason
// flagd-testbed derives its probe keys from the config being started.
func (p *edgeProxy) probeServing(key string, want any, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		fr, err := p.evaluate(key)
		if err != nil {
			last = err
		} else if jsonEqual(fr.Value, want) {
			return nil
		} else {
			last = fmt.Errorf("evaluate %s: serving %v, want %v", key, fr.Value, want)
		}
		time.Sleep(probeInterval)
	}
	return fmt.Errorf("timed out after %s waiting for %s: %w", timeout, key, last)
}

func jsonEqual(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}
