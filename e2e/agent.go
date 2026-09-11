//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

// EVP proxy endpoints the official provider writes to.
const (
	exposuresPath      = "/evp_proxy/v2/api/v2/exposures"
	flagEvaluationPath = "/evp_proxy/v2/api/v2/flagevaluation"
)

// TestAgent talks to a running dd-apm-test-agent.
type TestAgent struct {
	baseURL string
	client  *http.Client
}

// NewTestAgent returns a client for the test agent at baseURL.
func NewTestAgent(baseURL string) *TestAgent {
	return &TestAgent{baseURL: baseURL, client: &http.Client{Timeout: 10 * time.Second}}
}

// WaitReady blocks until the test agent answers /info.
func (a *TestAgent) WaitReady(ctx context.Context) error {
	return waitFor(ctx, time.Second, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+"/info", nil)
		if err != nil {
			return err
		}
		res, err := a.client.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("/info returned %d", res.StatusCode)
		}
		return nil
	})
}

// SetFlagConfiguration installs a Unified Flag Configuration as the FFE_FLAGS
// Remote Configuration product. The test agent builds the Remote Configuration
// envelope, so nothing here interprets the flag configuration itself.
func (a *TestAgent) SetFlagConfiguration(ctx context.Context, configID string, ufc json.RawMessage) error {
	payload, err := json.Marshal(map[string]any{
		"path": fmt.Sprintf("datadog/2/FFE_FLAGS/%s/config", configID),
		"msg":  ufc,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.baseURL+"/test/session/responses/config/path", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, _ := io.ReadAll(res.Body)
		return fmt.Errorf("installing the flag configuration returned %d: %s", res.StatusCode, body)
	}
	return nil
}

// ExposurePayload is the body the official provider posts to the exposure
// endpoint.
type ExposurePayload struct {
	Context struct {
		Service string `json:"service"`
		Env     string `json:"env"`
		Version string `json:"version"`
	} `json:"context"`
	Exposures []struct {
		Timestamp  int64 `json:"timestamp"`
		Allocation struct {
			Key string `json:"key"`
		} `json:"allocation"`
		Flag struct {
			Key string `json:"key"`
		} `json:"flag"`
		Variant struct {
			Key string `json:"key"`
		} `json:"variant"`
		Subject struct {
			ID         string         `json:"id"`
			Attributes map[string]any `json:"attributes"`
		} `json:"subject"`
	} `json:"exposures"`
}

// AgentProxy sits between the bridge and the test agent.
//
// The test agent accepts the exposure endpoint but drops the request, and does
// not route the flag evaluation endpoint at all, so neither can be inspected
// through its session API. Recording them here keeps Remote Configuration
// delivery with the test agent, where Datadog maintains it, while still letting
// the suite assert what the bridge actually sent.
type AgentProxy struct {
	server *httptest.Server

	mu              sync.Mutex
	exposures       []ExposurePayload
	flagEvaluations []json.RawMessage
	headers         []http.Header
}

// NewAgentProxy starts the proxy in front of the test agent at agentURL.
func NewAgentProxy(agentURL string) (*AgentProxy, error) {
	target, err := url.Parse(agentURL)
	if err != nil {
		return nil, err
	}

	p := &AgentProxy{}
	// The target is the fake Agent this suite starts itself, so there is no
	// untrusted input to this proxy.
	reverse := httputil.NewSingleHostReverseProxy(target) //nolint:gosec // G704

	mux := http.NewServeMux()
	mux.HandleFunc("POST "+exposuresPath, p.recordExposures)
	mux.HandleFunc("POST "+flagEvaluationPath, p.recordFlagEvaluations)
	mux.Handle("/", reverse)

	p.server = httptest.NewServer(mux)
	return p, nil
}

// URL is the address to point DD_TRACE_AGENT_URL at.
func (p *AgentProxy) URL() string { return p.server.URL }

// Close stops the proxy.
func (p *AgentProxy) Close() { p.server.Close() }

func (p *AgentProxy) recordExposures(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var payload ExposurePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	p.exposures = append(p.exposures, payload)
	p.headers = append(p.headers, r.Header.Clone())
	p.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (p *AgentProxy) recordFlagEvaluations(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	p.flagEvaluations = append(p.flagEvaluations, json.RawMessage(body))
	p.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// Exposures returns every exposure payload received so far.
func (p *AgentProxy) Exposures() []ExposurePayload {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ExposurePayload(nil), p.exposures...)
}

// ExposureHeaders returns the headers of every exposure request received so
// far.
func (p *AgentProxy) ExposureHeaders() []http.Header {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]http.Header(nil), p.headers...)
}

// FlagEvaluations returns every flag evaluation payload received so far.
func (p *AgentProxy) FlagEvaluations() []json.RawMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]json.RawMessage(nil), p.flagEvaluations...)
}

func waitFor(ctx context.Context, interval time.Duration, check func() error) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var last error
	for {
		if last = check(); last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last attempt: %w)", ctx.Err(), last)
		case <-ticker.C:
		}
	}
}
