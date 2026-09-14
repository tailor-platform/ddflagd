package ofrep

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-feature/go-sdk/openfeature"
	"github.com/open-feature/go-sdk/openfeature/isolated"
	"github.com/open-feature/go-sdk/openfeature/memprovider"

	"github.com/tailor-platform/ddflagd/internal/bridge"
	"github.com/tailor-platform/ddflagd/internal/metrics"
)

// ddMetadata is the flag metadata the official Datadog provider attaches to an
// evaluation result.
func ddMetadata() openfeature.FlagMetadata {
	return openfeature.FlagMetadata{
		"dd.allocation.key":     "allocation-1",
		"dd.doLog":              true,
		"dd.serialId":           int64(7),
		"dd.eval.timestamp_ms":  int64(1757500000000),
		"dd.unsupported.nested": map[string]any{"a": 1},
	}
}

// TestEvaluateMapping states the contract between a provider result and the
// OFREP response, row by row. The evaluation goes through a real OpenFeature
// client so that the response reflects what the client reports, not what a
// provider returns directly.
func TestEvaluateMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		resolution   openfeature.InterfaceResolutionDetail
		wantStatus   int
		wantBody     map[string]any
		wantAbsent   []string
		wantRetry    bool
		wantOutcome  metrics.Outcome
		wantHookFire bool
	}{
		{
			name: "targeting match carries the value, the variant and the metadata",
			resolution: openfeature.InterfaceResolutionDetail{
				Value: true,
				ProviderResolutionDetail: openfeature.ProviderResolutionDetail{
					Reason:       openfeature.TargetingMatchReason,
					Variant:      "on",
					FlagMetadata: ddMetadata(),
				},
			},
			wantStatus: http.StatusOK,
			wantBody: map[string]any{
				"key":     "flag",
				"value":   true,
				"reason":  "TARGETING_MATCH",
				"variant": "on",
				"metadata": map[string]any{
					"dd.allocation.key":    "allocation-1",
					"dd.doLog":             true,
					"dd.serialId":          float64(7),
					"dd.eval.timestamp_ms": float64(1757500000000),
				},
			},
			wantOutcome:  metrics.OutcomeValue,
			wantHookFire: true,
		},
		{
			name: "split carries the value",
			resolution: openfeature.InterfaceResolutionDetail{
				Value: map[string]any{"string": "one"},
				ProviderResolutionDetail: openfeature.ProviderResolutionDetail{
					Reason:  openfeature.SplitReason,
					Variant: "one",
				},
			},
			wantStatus: http.StatusOK,
			wantBody: map[string]any{
				"key":     "flag",
				"value":   map[string]any{"string": "one"},
				"reason":  "SPLIT",
				"variant": "one",
			},
			wantOutcome:  metrics.OutcomeValue,
			wantHookFire: true,
		},
		{
			name: "static carries a falsy value rather than omitting it",
			resolution: openfeature.InterfaceResolutionDetail{
				Value: false,
				ProviderResolutionDetail: openfeature.ProviderResolutionDetail{
					Reason:  openfeature.StaticReason,
					Variant: "off",
				},
			},
			wantStatus: http.StatusOK,
			wantBody: map[string]any{
				"key":     "flag",
				"value":   false,
				"reason":  "STATIC",
				"variant": "off",
			},
			wantOutcome:  metrics.OutcomeValue,
			wantHookFire: true,
		},
		{
			name: "default omits the value and the variant so the caller uses its code default",
			resolution: openfeature.InterfaceResolutionDetail{
				Value: nil,
				ProviderResolutionDetail: openfeature.ProviderResolutionDetail{
					Reason:       openfeature.DefaultReason,
					FlagMetadata: openfeature.FlagMetadata{"dd.eval.timestamp_ms": int64(1757500000000)},
				},
			},
			wantStatus: http.StatusOK,
			wantBody: map[string]any{
				"key":      "flag",
				"reason":   "UNKNOWN",
				"metadata": map[string]any{"dd.eval.timestamp_ms": float64(1757500000000)},
			},
			wantAbsent:   []string{"value", "variant"},
			wantOutcome:  metrics.OutcomeCodeDefault,
			wantHookFire: true,
		},
		{
			name: "disabled omits the value and keeps the reason",
			resolution: openfeature.InterfaceResolutionDetail{
				Value: nil,
				ProviderResolutionDetail: openfeature.ProviderResolutionDetail{
					Reason: openfeature.DisabledReason,
				},
			},
			wantStatus:   http.StatusOK,
			wantBody:     map[string]any{"key": "flag", "reason": "DISABLED"},
			wantAbsent:   []string{"value", "variant"},
			wantOutcome:  metrics.OutcomeCodeDefault,
			wantHookFire: true,
		},
		{
			name: "flag not found is 404",
			resolution: openfeature.InterfaceResolutionDetail{
				ProviderResolutionDetail: openfeature.ProviderResolutionDetail{
					ResolutionError: openfeature.NewFlagNotFoundResolutionError("flag not found"),
					Reason:          openfeature.ErrorReason,
				},
			},
			wantStatus:  http.StatusNotFound,
			wantBody:    map[string]any{"key": "flag", "errorCode": "FLAG_NOT_FOUND", "errorDetails": "flag not found"},
			wantOutcome: metrics.OutcomeFlagNotFound,
		},
		{
			name: "a missing targeting key is 400",
			resolution: openfeature.InterfaceResolutionDetail{
				ProviderResolutionDetail: openfeature.ProviderResolutionDetail{
					ResolutionError: openfeature.NewTargetingKeyMissingResolutionError("targeting key missing"),
					Reason:          openfeature.ErrorReason,
				},
			},
			wantStatus:  http.StatusBadRequest,
			wantBody:    map[string]any{"key": "flag", "errorCode": "TARGETING_KEY_MISSING"},
			wantOutcome: metrics.OutcomeInvalidRequest,
		},
		{
			name: "a parse error is 400",
			resolution: openfeature.InterfaceResolutionDetail{
				ProviderResolutionDetail: openfeature.ProviderResolutionDetail{
					ResolutionError: openfeature.NewParseErrorResolutionError("parse error"),
					Reason:          openfeature.ErrorReason,
				},
			},
			wantStatus:  http.StatusBadRequest,
			wantBody:    map[string]any{"key": "flag", "errorCode": "PARSE_ERROR"},
			wantOutcome: metrics.OutcomeInvalidRequest,
		},
		{
			name: "an invalid context is 400",
			resolution: openfeature.InterfaceResolutionDetail{
				ProviderResolutionDetail: openfeature.ProviderResolutionDetail{
					ResolutionError: openfeature.NewInvalidContextResolutionError("invalid context"),
					Reason:          openfeature.ErrorReason,
				},
			},
			wantStatus:  http.StatusBadRequest,
			wantBody:    map[string]any{"key": "flag", "errorCode": "INVALID_CONTEXT"},
			wantOutcome: metrics.OutcomeInvalidRequest,
		},
		{
			name: "a provider that holds no configuration is 503 with Retry-After",
			resolution: openfeature.InterfaceResolutionDetail{
				ProviderResolutionDetail: openfeature.ProviderResolutionDetail{
					ResolutionError: openfeature.NewProviderNotReadyResolutionError("no configuration loaded"),
					Reason:          openfeature.ErrorReason,
				},
			},
			wantStatus:  http.StatusServiceUnavailable,
			wantBody:    map[string]any{"key": "flag", "errorCode": "PROVIDER_NOT_READY"},
			wantRetry:   true,
			wantOutcome: metrics.OutcomeNotReady,
		},
		{
			name: "a general error is 500",
			resolution: openfeature.InterfaceResolutionDetail{
				ProviderResolutionDetail: openfeature.ProviderResolutionDetail{
					ResolutionError: openfeature.NewGeneralResolutionError("boom"),
					Reason:          openfeature.ErrorReason,
				},
			},
			wantStatus:  http.StatusInternalServerError,
			wantBody:    map[string]any{"key": "flag", "errorCode": "GENERAL", "errorDetails": "boom"},
			wantOutcome: metrics.OutcomeError,
		},
		{
			name: "a type mismatch is 500 even though an object evaluation should not produce one",
			resolution: openfeature.InterfaceResolutionDetail{
				ProviderResolutionDetail: openfeature.ProviderResolutionDetail{
					ResolutionError: openfeature.NewTypeMismatchResolutionError("type mismatch"),
					Reason:          openfeature.ErrorReason,
				},
			},
			wantStatus:  http.StatusInternalServerError,
			wantBody:    map[string]any{"key": "flag", "errorCode": "TYPE_MISMATCH"},
			wantOutcome: metrics.OutcomeError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			provider := &scriptedProvider{results: map[string]openfeature.InterfaceResolutionDetail{"flag": tt.resolution}}
			m := metrics.New("test")
			h := newTestHandler(t, provider, bridge.StateReady, m, 0)

			rec := post(t, h, "/ofrep/v1/evaluate/flags/flag", `{"context":{"targetingKey":"alice"}}`)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status: got %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type: got %q, want application/json", got)
			}
			retry := rec.Header().Get("Retry-After")
			if tt.wantRetry && retry != "1" {
				t.Errorf("Retry-After: got %q, want 1", retry)
			}
			if !tt.wantRetry && retry != "" {
				t.Errorf("Retry-After: got %q, want it absent", retry)
			}

			body := decodeBody(t, rec)
			for name, want := range tt.wantBody {
				got, ok := body[name]
				if !ok {
					t.Errorf("body is missing %q: %s", name, rec.Body.String())
					continue
				}
				if !equalJSON(got, want) {
					t.Errorf("body[%q]: got %#v, want %#v", name, got, want)
				}
			}
			for _, name := range tt.wantAbsent {
				if _, ok := body[name]; ok {
					t.Errorf("body should not carry %q: %s", name, rec.Body.String())
				}
			}
			if got := m.Snapshot().Evaluations[tt.wantOutcome]; got != 1 {
				t.Errorf("outcome %s: got %d, want 1 (all: %v)", tt.wantOutcome, got, m.Snapshot().Evaluations)
			}
			if got := provider.hookCalls.Load(); tt.wantHookFire && got == 0 {
				t.Error("the provider hook did not fire: exposure and evaluation counts depend on the client pipeline")
			}
		})
	}
}

// TestEvaluateWithMemProvider evaluates against the SDK's own reference
// provider, so that the handler is exercised without a purpose built double.
func TestEvaluateWithMemProvider(t *testing.T) {
	t.Parallel()

	provider := memprovider.NewInMemoryProvider(map[string]memprovider.InMemoryFlag{
		"string-flag": {
			Key:            "string-flag",
			State:          memprovider.Enabled,
			DefaultVariant: "green",
			Variants:       map[string]any{"green": "green-value"},
		},
	})

	h := newTestHandler(t, provider, bridge.StateReady, nil, 0)

	t.Run("a configured flag returns its value", func(t *testing.T) {
		rec := post(t, h, "/ofrep/v1/evaluate/flags/string-flag", `{"context":{"targetingKey":"alice"}}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status: got %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		body := decodeBody(t, rec)
		if body["value"] != "green-value" {
			t.Errorf("value: got %#v, want green-value", body["value"])
		}
		if body["reason"] != "STATIC" {
			t.Errorf("reason: got %#v, want STATIC", body["reason"])
		}
		if body["variant"] != "green" {
			t.Errorf("variant: got %#v, want green", body["variant"])
		}
	})

	t.Run("an unknown flag is 404", func(t *testing.T) {
		rec := post(t, h, "/ofrep/v1/evaluate/flags/absent", `{"context":{"targetingKey":"alice"}}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status: got %d, want 404 (%s)", rec.Code, rec.Body.String())
		}
		if body := decodeBody(t, rec); body["errorCode"] != "FLAG_NOT_FOUND" {
			t.Errorf("errorCode: got %#v, want FLAG_NOT_FOUND", body["errorCode"])
		}
	})
}

// TestRequestValidation states which evaluation contexts ddflagd accepts. The
// limits exist because Datadog only supports flat primitive attributes.
func TestRequestValidation(t *testing.T) {
	t.Parallel()

	oversized := strings.Repeat("a", bridge.MaxContextStringBytes+1)
	manyAttributes := make(map[string]any, bridge.MaxContextAttributes+1)
	for i := range bridge.MaxContextAttributes + 1 {
		manyAttributes[fmt.Sprintf("attr-%d", i)] = "v"
	}
	manyAttributesJSON, err := json.Marshal(map[string]any{"context": manyAttributes})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "a nested attribute is rejected",
			body:       `{"context":{"targetingKey":"alice","user":{"id":"1"}}}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "INVALID_CONTEXT",
		},
		{
			name:       "an array attribute is rejected",
			body:       `{"context":{"targetingKey":"alice","tags":["a"]}}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "INVALID_CONTEXT",
		},
		{
			name:       "a non string targeting key is rejected",
			body:       `{"context":{"targetingKey":42}}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "INVALID_CONTEXT",
		},
		{
			name:       "an over long targeting key is rejected",
			body:       `{"context":{"targetingKey":"` + oversized + `"}}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "INVALID_CONTEXT",
		},
		{
			name:       "an over long string attribute is rejected",
			body:       `{"context":{"targetingKey":"alice","region":"` + oversized + `"}}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "INVALID_CONTEXT",
		},
		{
			name:       "too many attributes are rejected",
			body:       string(manyAttributesJSON),
			wantStatus: http.StatusBadRequest,
			wantCode:   "INVALID_CONTEXT",
		},
		{
			name:       "a malformed body is rejected",
			body:       `{"context":`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "PARSE_ERROR",
		},
		{
			name:       "an over sized body is truncated and rejected",
			body:       `{"context":{"targetingKey":"` + strings.Repeat("b", int(bridge.MaxRequestBodyBytes)) + `"}}`,
			wantStatus: http.StatusRequestEntityTooLarge,
			wantCode:   "PARSE_ERROR",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			provider := &scriptedProvider{results: map[string]openfeature.InterfaceResolutionDetail{
				"flag": {Value: true, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.StaticReason}},
			}}
			m := metrics.New("test")
			h := newTestHandler(t, provider, bridge.StateReady, m, 0)

			rec := post(t, h, "/ofrep/v1/evaluate/flags/flag", tt.body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status: got %d, want %d (%s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if got := decodeBody(t, rec)["errorCode"]; got != tt.wantCode {
				t.Errorf("errorCode: got %#v, want %s", got, tt.wantCode)
			}
			if got := m.Snapshot().Evaluations[metrics.OutcomeInvalidRequest]; got != 1 {
				t.Errorf("invalid_request outcome: got %d, want 1", got)
			}
			if provider.calls.Load() != 0 {
				t.Error("a rejected request must not reach the provider")
			}
		})
	}
}

// TestContextConversion states how an OFREP context reaches the provider.
func TestContextConversion(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{results: map[string]openfeature.InterfaceResolutionDetail{
		"flag": {Value: true, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.StaticReason}},
	}}
	h := newTestHandler(t, provider, bridge.StateReady, nil, 0)

	rec := post(t, h, "/ofrep/v1/evaluate/flags/flag",
		`{"context":{"targetingKey":"alice","region":"jp","age":50,"score":3.5,"beta":true,"absent":null}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	got := provider.lastContext()
	want := openfeature.FlattenedContext{
		"targetingKey": "alice",
		"region":       "jp",
		"age":          int64(50),
		"score":        3.5,
		"beta":         true,
		"absent":       nil,
	}
	for name, wantValue := range want {
		gotValue, present := got[name]
		if !present {
			t.Errorf("context[%q] is missing: a null attribute is forwarded because the official provider treats null and absent alike for its IS_NULL operator", name)
			continue
		}
		if gotValue != wantValue {
			t.Errorf("context[%q]: got %#v (%T), want %#v (%T)", name, gotValue, gotValue, wantValue, wantValue)
		}
	}
}

// TestMissingTargetingKeyIsPassedThrough states that ddflagd does not reject a
// context without a targeting key: whether a flag needs one is the official
// provider's decision.
func TestMissingTargetingKeyIsPassedThrough(t *testing.T) {
	t.Parallel()

	for _, body := range []string{`{"context":{}}`, `{"context":{"targetingKey":null}}`} {
		provider := &scriptedProvider{results: map[string]openfeature.InterfaceResolutionDetail{
			"flag": {Value: "v", ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.StaticReason}},
		}}
		h := newTestHandler(t, provider, bridge.StateReady, nil, 0)

		rec := post(t, h, "/ofrep/v1/evaluate/flags/flag", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status got %d, want 200 (%s)", body, rec.Code, rec.Body.String())
		}
		if provider.calls.Load() != 1 {
			t.Errorf("%s: the evaluation did not reach the provider", body)
		}
	}
}

// TestStartingStateIsNotReady states that a bridge which has not received a
// flag configuration answers 503 without evaluating.
func TestStartingStateIsNotReady(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{results: map[string]openfeature.InterfaceResolutionDetail{
		"flag": {Value: true, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.StaticReason}},
	}}
	m := metrics.New("test")
	h := newTestHandler(t, provider, bridge.StateStarting, m, 0)

	rec := post(t, h, "/ofrep/v1/evaluate/flags/flag", `{"context":{"targetingKey":"alice"}}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503 (%s)", rec.Code, rec.Body.String())
	}
	if got := decodeBody(t, rec)["errorCode"]; got != "PROVIDER_NOT_READY" {
		t.Errorf("errorCode: got %#v, want PROVIDER_NOT_READY", got)
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After: got %q, want 1", got)
	}
	if provider.calls.Load() != 0 {
		t.Error("an unready bridge must not evaluate")
	}
	if got := m.Snapshot().Evaluations[metrics.OutcomeNotReady]; got != 1 {
		t.Errorf("not_ready outcome: got %d, want 1", got)
	}
}

// TestHandlerTimeout states that the response time is bounded by ddflagd, since
// the official Rust OFREP provider has no overall request timeout.
func TestHandlerTimeout(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	provider := &scriptedProvider{
		results: map[string]openfeature.InterfaceResolutionDetail{
			"flag": {Value: true, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.StaticReason}},
		},
		block: release,
	}
	m := metrics.New("test")
	h := newTestHandler(t, provider, bridge.StateReady, m, 20*time.Millisecond)

	rec := post(t, h, "/ofrep/v1/evaluate/flags/flag", `{"context":{"targetingKey":"alice"}}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503 (%s)", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["errorCode"] != "GENERAL" || body["errorDetails"] != "evaluation timed out" {
		t.Errorf("body: got %#v, want a GENERAL timeout failure", body)
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After: got %q, want 1", got)
	}
	if got := m.Snapshot().Evaluations[metrics.OutcomeTimeout]; got != 1 {
		t.Errorf("timeout outcome: got %d, want 1", got)
	}
}

// TestStopServing states the response once the drain delay has passed.
func TestStopServing(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{results: map[string]openfeature.InterfaceResolutionDetail{
		"flag": {Value: true, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.StaticReason}},
	}}
	m := metrics.New("test")
	h := newTestHandler(t, provider, bridge.StateReady, m, 0)
	h.StopServing()

	rec := post(t, h, "/ofrep/v1/evaluate/flags/flag", `{"context":{"targetingKey":"alice"}}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503 (%s)", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["errorDetails"] != "shutting down" {
		t.Errorf("errorDetails: got %#v, want shutting down", body["errorDetails"])
	}
	if provider.calls.Load() != 0 {
		t.Error("a draining bridge must not evaluate")
	}
}

// TestBulkEndpointIsNotImplemented states that the bulk endpoint answers
// explicitly instead of 404, so a client can tell it apart from a wrong path.
func TestBulkEndpointIsNotImplemented(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{}
	h := newTestHandler(t, provider, bridge.StateReady, nil, 0)

	rec := post(t, h, "/ofrep/v1/evaluate/flags", `{"context":{"targetingKey":"alice"}}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status: got %d, want 501 (%s)", rec.Code, rec.Body.String())
	}
}

// TestMethodNotAllowed states that a GET on the evaluation endpoint is rejected.
func TestMethodNotAllowed(t *testing.T) {
	t.Parallel()

	h := newTestHandler(t, &scriptedProvider{}, bridge.StateReady, nil, 0)

	req := httptest.NewRequest(http.MethodGet, "/ofrep/v1/evaluate/flags/flag", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d, want 405", rec.Code)
	}
}

// TestNewHandlerRequiresEvaluator states that the handler cannot be built
// without something to evaluate through.
func TestNewHandlerRequiresEvaluator(t *testing.T) {
	t.Parallel()

	if _, err := NewHandler(Options{}); err == nil {
		t.Fatal("want an error when Evaluator is nil")
	}
}

// helpers

func newTestHandler(t *testing.T, provider openfeature.FeatureProvider, state bridge.State, m *metrics.Metrics, timeout time.Duration) *Handler {
	t.Helper()

	api := isolated.NewAPI()
	if err := api.SetProviderAndWait(context.Background(), provider); err != nil {
		t.Fatalf("setting the provider: %v", err)
	}
	t.Cleanup(func() {
		if err := api.Shutdown(context.Background()); err != nil {
			t.Errorf("shutting down the API: %v", err)
		}
	})

	h, err := NewHandler(Options{
		Evaluator: &clientEvaluator{client: api.NewClient(), state: state},
		Metrics:   m,
		Timeout:   timeout,
	})
	if err != nil {
		t.Fatalf("building the handler: %v", err)
	}
	return h
}

func post(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	var body map[string]any
	if rec.Body.Len() == 0 {
		return body
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return body
}

func equalJSON(got, want any) bool {
	a, err := json.Marshal(got)
	if err != nil {
		return false
	}
	b, err := json.Marshal(want)
	if err != nil {
		return false
	}
	return bytes.Equal(a, b)
}

// clientEvaluator evaluates through an OpenFeature client, the same way the
// bridge does.
type clientEvaluator struct {
	client *openfeature.Client
	state  bridge.State
}

func (e *clientEvaluator) Evaluate(ctx context.Context, key string, evalCtx openfeature.EvaluationContext) (openfeature.InterfaceEvaluationDetails, error) {
	return e.client.ObjectValueDetails(ctx, key, nil, evalCtx)
}

func (e *clientEvaluator) State() bridge.State { return e.state }

// scriptedProvider returns a prepared resolution for a flag key and records
// what it was asked. Its hook stands in for the exposure and evaluation count
// hooks of the official provider, which only fire through a client.
type scriptedProvider struct {
	results map[string]openfeature.InterfaceResolutionDetail
	block   <-chan struct{}

	calls     atomic.Int64
	hookCalls atomic.Int64

	mu      sync.Mutex
	lastCtx openfeature.FlattenedContext
}

func (p *scriptedProvider) Metadata() openfeature.Metadata {
	return openfeature.Metadata{Name: "scripted"}
}

func (p *scriptedProvider) ObjectEvaluation(ctx context.Context, flag string, defaultValue any, flatCtx openfeature.FlattenedContext) openfeature.InterfaceResolutionDetail {
	p.calls.Add(1)
	p.mu.Lock()
	p.lastCtx = flatCtx
	p.mu.Unlock()

	if p.block != nil {
		select {
		case <-p.block:
		case <-ctx.Done():
		}
	}

	res, ok := p.results[flag]
	if !ok {
		return openfeature.InterfaceResolutionDetail{
			Value:           defaultValue,
			ResolutionError: openfeature.NewFlagNotFoundResolutionError("flag not found"),
			Reason:          openfeature.ErrorReason,
		}
	}
	return res
}

func (p *scriptedProvider) BooleanEvaluation(ctx context.Context, flag string, defaultValue bool, flatCtx openfeature.FlattenedContext) openfeature.BoolResolutionDetail {
	res := p.ObjectEvaluation(ctx, flag, defaultValue, flatCtx)
	v, _ := res.Value.(bool)
	return openfeature.BoolResolutionDetail{Value: v, ProviderResolutionDetail: res.ProviderResolutionDetail}
}

func (p *scriptedProvider) StringEvaluation(ctx context.Context, flag string, defaultValue string, flatCtx openfeature.FlattenedContext) openfeature.StringResolutionDetail {
	res := p.ObjectEvaluation(ctx, flag, defaultValue, flatCtx)
	v, _ := res.Value.(string)
	return openfeature.StringResolutionDetail{Value: v, ProviderResolutionDetail: res.ProviderResolutionDetail}
}

func (p *scriptedProvider) FloatEvaluation(ctx context.Context, flag string, defaultValue float64, flatCtx openfeature.FlattenedContext) openfeature.FloatResolutionDetail {
	res := p.ObjectEvaluation(ctx, flag, defaultValue, flatCtx)
	v, _ := res.Value.(float64)
	return openfeature.FloatResolutionDetail{Value: v, ProviderResolutionDetail: res.ProviderResolutionDetail}
}

func (p *scriptedProvider) IntEvaluation(ctx context.Context, flag string, defaultValue int64, flatCtx openfeature.FlattenedContext) openfeature.IntResolutionDetail {
	res := p.ObjectEvaluation(ctx, flag, defaultValue, flatCtx)
	v, _ := res.Value.(int64)
	return openfeature.IntResolutionDetail{Value: v, ProviderResolutionDetail: res.ProviderResolutionDetail}
}

func (p *scriptedProvider) Hooks() []openfeature.Hook {
	return []openfeature.Hook{&countingHook{calls: &p.hookCalls}}
}

func (p *scriptedProvider) lastContext() openfeature.FlattenedContext {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastCtx
}

type countingHook struct {
	openfeature.UnimplementedHook
	calls *atomic.Int64
}

func (h *countingHook) After(context.Context, openfeature.HookContext, openfeature.InterfaceEvaluationDetails, openfeature.HookHints) error {
	h.calls.Add(1)
	return nil
}
