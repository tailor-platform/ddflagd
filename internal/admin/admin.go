// Package admin serves the endpoints Kubernetes and operators talk to: the
// probes, the Prometheus metrics and the status and pprof endpoints.
package admin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/k1LoW/ddflagd/internal/bridge"
	"github.com/k1LoW/ddflagd/internal/metrics"
	"github.com/k1LoW/ddflagd/version"
)

// StatusReporter is the part of the bridge the operational listener reports on.
type StatusReporter interface {
	Status() bridge.Status
}

// Options configures NewHandler.
type Options struct {
	Status  StatusReporter
	Metrics *metrics.Metrics
	Logger  *slog.Logger

	// Revision is the commit the binary was built from.
	Revision string

	// PprofEnabled exposes /debug/pprof.
	PprofEnabled bool
}

// Handler serves the operational endpoints. It carries no authentication: in a
// sidecar it is the only listener the Pod network can reach, and a Deployment
// restricts it with a NetworkPolicy.
type Handler struct {
	status       StatusReporter
	metrics      *metrics.Metrics
	logger       *slog.Logger
	revision     string
	pprofEnabled bool
	mux          *http.ServeMux
}

// NewHandler builds the operational handler.
func NewHandler(opts Options) (*Handler, error) {
	if opts.Status == nil {
		return nil, errors.New("admin: Status is required")
	}
	h := &Handler{
		status:       opts.Status,
		metrics:      opts.Metrics,
		logger:       opts.Logger,
		revision:     opts.Revision,
		pprofEnabled: opts.PprofEnabled,
	}
	if h.metrics == nil {
		h.metrics = metrics.New(version.Version)
	}
	if h.logger == nil {
		h.logger = slog.New(slog.DiscardHandler)
	}

	h.mux = http.NewServeMux()
	h.mux.HandleFunc("GET /healthz", h.handleHealthz)
	h.mux.HandleFunc("GET /readyz", h.handleReadyz)
	h.mux.HandleFunc("GET /metrics", h.handleMetrics)
	h.mux.HandleFunc("GET /debug/status", h.handleStatus)
	if h.pprofEnabled {
		h.mux.HandleFunc("GET /debug/pprof/", pprof.Index)
		h.mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
		h.mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
		h.mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
		h.mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	}
	return h, nil
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// handleHealthz answers liveness. It reports only that the process can serve a
// request, and deliberately ignores the Agent and the provider: making liveness
// depend on either turns an Agent outage into a restart loop.
func (h *Handler) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// handleReadyz answers readiness and startup.
//
// Configuration freshness is left out on purpose. The official provider keeps
// its configuration private and emits no events, so freshness cannot be
// observed here, and failing readiness over a stale configuration would drop
// the Pod out of its Service and send every evaluation to the code default,
// which is worse for the caller than evaluating against an older configuration.
func (h *Handler) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	status := h.status.Status()
	if status.State == bridge.StateReady {
		writeJSON(w, http.StatusOK, map[string]any{
			"state":    string(bridge.StateReady),
			"ready_at": status.ReadyAt.UTC().Format(time.RFC3339Nano),
		})
		return
	}

	reason := "the provider has not received a flag configuration yet"
	if status.State == bridge.StateShuttingDown {
		reason = "the process is shutting down"
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"state":  string(status.State),
		"reason": reason,
	})
}

func (h *Handler) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if err := h.metrics.Write(w); err != nil {
		h.logger.Error("could not write metrics", slog.Any("error", err))
	}
}

func (h *Handler) handleStatus(w http.ResponseWriter, _ *http.Request) {
	status := h.status.Status()
	snapshot := h.metrics.Snapshot()

	evaluations := make(map[string]uint64, len(snapshot.Evaluations))
	for outcome, count := range snapshot.Evaluations {
		evaluations[string(outcome)] = count
	}

	body := map[string]any{
		"version":             snapshot.Version,
		"revision":            h.revision,
		"dd_trace_go_version": snapshot.DDTraceGoVersion,
		"started_at":          status.StartedAt.UTC().Format(time.RFC3339Nano),
		"state":               string(status.State),
		"evaluations":         evaluations,
		"config":              status.Config.Redacted(),
	}
	if !status.ReadyAt.IsZero() {
		body["ready_at"] = status.ReadyAt.UTC().Format(time.RFC3339Nano)
	}
	if snapshot.LastError != nil {
		body["last_error"] = map[string]any{
			"at":      snapshot.LastError.At.UTC().Format(time.RFC3339Nano),
			"message": snapshot.LastError.Message,
		}
	}
	writeJSON(w, http.StatusOK, body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The body is small and already in memory, so an encoding failure can only
	// mean the client went away.
	_ = json.NewEncoder(w).Encode(body) //nostyle:handlerrors
}
