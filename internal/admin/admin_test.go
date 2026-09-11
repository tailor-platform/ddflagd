package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/k1LoW/ddflagd/internal/bridge"
	"github.com/k1LoW/ddflagd/internal/metrics"
)

// TestHealthzIgnoresTheProviderState states that liveness reports only that the
// process serves requests. Tying it to the Agent or the provider would turn an
// Agent outage into a restart loop.
func TestHealthzIgnoresTheProviderState(t *testing.T) {
	t.Parallel()

	for _, state := range []bridge.State{bridge.StateStarting, bridge.StateReady, bridge.StateShuttingDown} {
		h := newTestHandler(t, &stubStatus{state: state}, nil, false)
		rec := fetch(t, h, "/healthz")
		if rec.Code != http.StatusOK {
			t.Errorf("state %s: got %d, want 200", state, rec.Code)
		}
	}
}

// TestReadyz states which states take the Pod out of its Service.
func TestReadyz(t *testing.T) {
	t.Parallel()

	readyAt := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		state      bridge.State
		wantStatus int
		wantState  string
	}{
		{state: bridge.StateStarting, wantStatus: http.StatusServiceUnavailable, wantState: "starting"},
		{state: bridge.StateReady, wantStatus: http.StatusOK, wantState: "ready"},
		{state: bridge.StateShuttingDown, wantStatus: http.StatusServiceUnavailable, wantState: "shutting_down"},
	}

	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			t.Parallel()

			h := newTestHandler(t, &stubStatus{state: tt.state, readyAt: readyAt}, nil, false)
			rec := fetch(t, h, "/readyz")
			if rec.Code != tt.wantStatus {
				t.Fatalf("status: got %d, want %d", rec.Code, tt.wantStatus)
			}
			body := decode(t, rec)
			if body["state"] != tt.wantState {
				t.Errorf("state: got %#v, want %s", body["state"], tt.wantState)
			}
			if tt.wantStatus == http.StatusServiceUnavailable {
				if _, ok := body["reason"]; !ok {
					t.Error("a 503 must explain itself")
				}
			}
		})
	}
}

// TestMetricsExposesThePrometheusFormat states that the operational listener
// renders the metrics a scraper needs.
func TestMetricsExposesThePrometheusFormat(t *testing.T) {
	t.Parallel()

	m := metrics.New("1.2.3")
	m.ObserveEvaluation(metrics.OutcomeValue, 500*time.Microsecond)
	m.ObserveEvaluation(metrics.OutcomeTimeout, 300*time.Millisecond)
	m.SetProviderReady(time.Unix(1757500000, 0))

	h := newTestHandler(t, &stubStatus{state: bridge.StateReady}, m, false)
	rec := fetch(t, h, "/metrics")

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type: got %q, want text/plain", got)
	}

	body := rec.Body.String()
	for _, want := range []string{
		`ddflagd_build_info{version="1.2.3"`,
		`ddflagd_evaluations_total{outcome="value"} 1`,
		`ddflagd_evaluations_total{outcome="timeout"} 1`,
		`ddflagd_evaluations_total{outcome="error"} 0`,
		`ddflagd_evaluation_duration_seconds_count{outcome="value"} 1`,
		`ddflagd_evaluation_duration_seconds_bucket{outcome="value",le="0.0005"} 1`,
		`ddflagd_evaluation_duration_seconds_bucket{outcome="value",le="0.00025"} 0`,
		"ddflagd_provider_ready 1",
		"ddflagd_provider_ready_timestamp_seconds 1757500000",
		"# TYPE ddflagd_evaluation_duration_seconds histogram",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the metrics are missing %q:\n%s", want, body)
		}
	}
}

// TestDebugStatus states what the operator facing status reports.
func TestDebugStatus(t *testing.T) {
	t.Parallel()

	m := metrics.New("1.2.3")
	m.ObserveEvaluation(metrics.OutcomeCodeDefault, time.Millisecond)
	m.RecordError("flag: boom")

	status := &stubStatus{
		state:     bridge.StateReady,
		startedAt: time.Date(2026, 9, 11, 11, 0, 0, 0, time.UTC),
		readyAt:   time.Date(2026, 9, 11, 11, 0, 5, 0, time.UTC),
		cfg:       &bridge.Config{Service: "rust-service", APIKey: "super-secret"},
	}
	h := newTestHandler(t, status, m, false)
	rec := fetch(t, h, "/debug/status")

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	body := decode(t, rec)

	if body["version"] != "1.2.3" {
		t.Errorf("version: got %#v", body["version"])
	}
	if body["revision"] != "abc1234" {
		t.Errorf("revision: got %#v", body["revision"])
	}
	if body["state"] != "ready" {
		t.Errorf("state: got %#v", body["state"])
	}
	// A test binary carries no dependency list in its build info, so only the
	// presence of the label is checked here. The e2e suite asserts the real
	// version against the compiled binary.
	if body["dd_trace_go_version"] == "" || body["dd_trace_go_version"] == nil {
		t.Error("dd_trace_go_version is empty")
	}
	if body["started_at"] != "2026-09-11T11:00:00Z" {
		t.Errorf("started_at: got %#v", body["started_at"])
	}
	if body["ready_at"] != "2026-09-11T11:00:05Z" {
		t.Errorf("ready_at: got %#v", body["ready_at"])
	}

	evaluations, ok := body["evaluations"].(map[string]any)
	if !ok {
		t.Fatalf("evaluations: got %#v", body["evaluations"])
	}
	if evaluations["code_default"] != float64(1) {
		t.Errorf("evaluations[code_default]: got %#v, want 1", evaluations["code_default"])
	}

	lastError, ok := body["last_error"].(map[string]any)
	if !ok {
		t.Fatalf("last_error: got %#v", body["last_error"])
	}
	if lastError["message"] != "flag: boom" {
		t.Errorf("last_error[message]: got %#v", lastError["message"])
	}

	if strings.Contains(rec.Body.String(), "super-secret") {
		t.Error("the status leaks the shared secret")
	}
}

// TestDebugStatusOmitsReadyAtBeforeReady states that a starting bridge reports
// no readiness timestamp.
func TestDebugStatusOmitsReadyAtBeforeReady(t *testing.T) {
	t.Parallel()

	h := newTestHandler(t, &stubStatus{state: bridge.StateStarting}, nil, false)
	body := decode(t, fetch(t, h, "/debug/status"))
	if _, ok := body["ready_at"]; ok {
		t.Error("ready_at must be absent before the provider is ready")
	}
	if _, ok := body["last_error"]; ok {
		t.Error("last_error must be absent when nothing failed")
	}
}

// TestPprofIsOptional states that the profiling endpoints are only reachable
// when they are switched on.
func TestPprofIsOptional(t *testing.T) {
	t.Parallel()

	off := newTestHandler(t, &stubStatus{state: bridge.StateReady}, nil, false)
	if got := fetch(t, off, "/debug/pprof/").Code; got != http.StatusNotFound {
		t.Errorf("pprof disabled: got %d, want 404", got)
	}

	on := newTestHandler(t, &stubStatus{state: bridge.StateReady}, nil, true)
	if got := fetch(t, on, "/debug/pprof/").Code; got != http.StatusOK {
		t.Errorf("pprof enabled: got %d, want 200", got)
	}
}

// TestNewHandlerRequiresStatus states that the operational handler cannot be
// built without something to report on.
func TestNewHandlerRequiresStatus(t *testing.T) {
	t.Parallel()

	if _, err := NewHandler(Options{}); err == nil {
		t.Fatal("want an error when Status is nil")
	}
}

// helpers

func newTestHandler(t *testing.T, status StatusReporter, m *metrics.Metrics, pprofEnabled bool) *Handler {
	t.Helper()

	h, err := NewHandler(Options{
		Status:       status,
		Metrics:      m,
		Revision:     "abc1234",
		PprofEnabled: pprofEnabled,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h
}

func fetch(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return body
}

type stubStatus struct {
	state     bridge.State
	startedAt time.Time
	readyAt   time.Time
	cfg       *bridge.Config
}

func (s *stubStatus) Status() bridge.Status {
	cfg := s.cfg
	if cfg == nil {
		cfg = &bridge.Config{Service: "rust-service"}
	}
	readyAt := s.readyAt
	if s.state != bridge.StateReady {
		readyAt = time.Time{}
	}
	return bridge.Status{
		State:     s.state,
		StartedAt: s.startedAt,
		ReadyAt:   readyAt,
		Config:    cfg,
	}
}
