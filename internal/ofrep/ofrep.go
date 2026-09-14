// Package ofrep serves the OpenFeature Remote Evaluation Protocol on top of an
// OpenFeature client.
package ofrep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/open-feature/go-sdk/openfeature"

	"github.com/tailor-platform/ddflagd/internal/bridge"
	"github.com/tailor-platform/ddflagd/internal/metrics"
)

// Paths of the OFREP endpoints.
const (
	PathEvaluateFlag  = "/ofrep/v1/evaluate/flags/{key}"
	PathEvaluateFlags = "/ofrep/v1/evaluate/flags"
)

// Evaluator is the part of the bridge the OFREP handler needs.
type Evaluator interface {
	Evaluate(ctx context.Context, key string, evalCtx openfeature.EvaluationContext) (openfeature.InterfaceEvaluationDetails, error)
	State() bridge.State
}

// Options configures NewHandler.
type Options struct {
	Evaluator Evaluator
	Metrics   *metrics.Metrics
	Logger    *slog.Logger

	// Timeout bounds one evaluation request. The official Rust OFREP provider
	// has no overall request timeout, so the response time is guaranteed here.
	Timeout time.Duration

	// MaxBodyBytes, MaxAttributes and MaxStringBytes bound the request. Zero
	// values fall back to the package defaults.
	MaxBodyBytes   int64
	MaxAttributes  int
	MaxStringBytes int
}

// Handler serves the OFREP endpoints.
type Handler struct {
	evaluator      Evaluator
	metrics        *metrics.Metrics
	logger         *slog.Logger
	timeout        time.Duration
	maxBodyBytes   int64
	maxAttributes  int
	maxStringBytes int

	stopped atomic.Bool
	mux     *http.ServeMux
}

// NewHandler builds the OFREP handler.
func NewHandler(opts Options) (*Handler, error) {
	if opts.Evaluator == nil {
		return nil, errors.New("ofrep: Evaluator is required")
	}
	h := &Handler{
		evaluator:      opts.Evaluator,
		metrics:        opts.Metrics,
		logger:         opts.Logger,
		timeout:        opts.Timeout,
		maxBodyBytes:   opts.MaxBodyBytes,
		maxAttributes:  opts.MaxAttributes,
		maxStringBytes: opts.MaxStringBytes,
	}
	if h.metrics == nil {
		h.metrics = metrics.New("")
	}
	if h.logger == nil {
		h.logger = slog.New(slog.DiscardHandler)
	}
	if h.timeout <= 0 {
		h.timeout = bridge.DefaultHandlerTimeout
	}
	if h.maxBodyBytes <= 0 {
		h.maxBodyBytes = bridge.MaxRequestBodyBytes
	}
	if h.maxAttributes <= 0 {
		h.maxAttributes = bridge.MaxContextAttributes
	}
	if h.maxStringBytes <= 0 {
		h.maxStringBytes = bridge.MaxContextStringBytes
	}

	h.mux = http.NewServeMux()
	h.mux.HandleFunc("POST "+PathEvaluateFlag, h.handleEvaluateFlag)
	h.mux.HandleFunc("POST "+PathEvaluateFlags, h.handleEvaluateFlags)
	return h, nil
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// StopServing makes every later evaluation request fail with 503. It is called
// once the drain delay has passed and the evaluation listener is closing, so
// that a request which arrives during the close is answered rather than reset.
func (h *Handler) StopServing() {
	h.stopped.Store(true)
}

// handleEvaluateFlags answers the bulk endpoint. Bulk evaluation carries a
// static context and targets client side SDKs, which ddflagd does not serve.
func (h *Handler) handleEvaluateFlags(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]any{
		"errorCode":    string(openfeature.GeneralCode),
		"errorDetails": "bulk evaluation is not implemented: ddflagd serves the single flag endpoint for dynamic context evaluation",
	}, nil)
}

func (h *Handler) handleEvaluateFlag(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	key := r.PathValue("key")

	status, body, outcome, header := h.evaluate(r, key)
	writeJSON(w, status, body, header)
	h.metrics.ObserveEvaluation(outcome, time.Since(started))
	if outcome == metrics.OutcomeError || outcome == metrics.OutcomeTimeout {
		if f, ok := body.(failureResponse); ok {
			h.metrics.RecordError(fmt.Sprintf("%s: %s", key, f.ErrorDetails))
			h.logger.Error("evaluation failed",
				slog.String("flag", key),
				slog.String("outcome", string(outcome)),
				slog.String("error", f.ErrorDetails))
		}
	}
}

func (h *Handler) evaluate(r *http.Request, key string) (int, any, metrics.Outcome, http.Header) {
	if h.stopped.Load() {
		return http.StatusServiceUnavailable,
			failureResponse{Key: key, ErrorCode: string(openfeature.GeneralCode), ErrorDetails: "shutting down"},
			metrics.OutcomeNotReady,
			retryAfter()
	}

	evalCtx, failure := h.decodeContext(r, key)
	if failure != nil {
		return failure.status, failure.body, failure.outcome, nil
	}

	// The provider holds no configuration before the first Remote Configuration
	// payload arrives. Answering here keeps the evaluation hooks from firing on
	// an evaluation that cannot produce a value.
	if state := h.evaluator.State(); state == bridge.StateStarting {
		return http.StatusServiceUnavailable,
			failureResponse{Key: key, ErrorCode: errorCodeProviderNotReady, ErrorDetails: "the provider has not received a flag configuration yet"},
			metrics.OutcomeNotReady,
			retryAfter()
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()

	type outcome struct {
		details openfeature.InterfaceEvaluationDetails
		err     error
	}
	// The evaluation runs in its own goroutine so that a provider which stalls
	// cannot hold the response past the timeout. http.TimeoutHandler is not
	// used because its 503 cannot carry Retry-After or feed the outcome metric.
	done := make(chan outcome, 1)
	go func() {
		details, err := h.evaluator.Evaluate(ctx, key, *evalCtx)
		done <- outcome{details: details, err: err}
	}()

	select {
	case res := <-done:
		status, body, o := mapResult(key, res.details, res.err)
		var header http.Header
		if status == http.StatusServiceUnavailable {
			header = retryAfter()
		}
		return status, body, o, header
	case <-ctx.Done():
		return http.StatusServiceUnavailable,
			failureResponse{Key: key, ErrorCode: string(openfeature.GeneralCode), ErrorDetails: "evaluation timed out"},
			metrics.OutcomeTimeout,
			retryAfter()
	}
}

// retryAfter returns the Retry-After header ddflagd puts on every 503.
//
// A 429 is never returned: the official Rust OFREP provider keeps a 429's
// Retry-After in a process wide static and fails every later evaluation in the
// process until it passes.
func retryAfter() http.Header {
	return http.Header{"Retry-After": []string{"1"}}
}

func writeJSON(w http.ResponseWriter, status int, body any, header http.Header) {
	for name, values := range header {
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	// The response is small and fully in memory, so an encoding failure can
	// only come from the writer, where nothing can be done about it.
	_ = json.NewEncoder(w).Encode(body) //nostyle:handlerrors
}
