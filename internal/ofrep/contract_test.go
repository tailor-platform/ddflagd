package ofrep

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/open-feature/go-sdk/openfeature"
	"github.com/pb33f/libopenapi"
	validator "github.com/pb33f/libopenapi-validator"

	"github.com/k1LoW/ddflagd/internal/bridge"
)

// specPath is the vendored OFREP OpenAPI document. It is vendored rather than
// fetched so that the contract test is reproducible offline; the version it was
// taken from is recorded in the README.
const specPath = "../../testdata/ofrep/openapi.yaml"

// The document's codeDefaultFlag schema is an unconstrained object, so it
// matches every typed flag response as well as the valueless one it describes.
// That makes the oneOf in evaluationSuccess ambiguous for any implementation
// that returns a value at all, including the flagd reference implementation.
// The vendored document is left untouched and this one constraint, which the
// schema's own description already states in prose, is added before validating.
const (
	codeDefaultSchema      = "      type: object\n      properties: {}\n"
	codeDefaultSchemaFixed = "      type: object\n      properties: {}\n      not:\n        required:\n          - value\n"
)

// TestResponsesSatisfyTheOFREPSchema states that ddflagd's responses validate
// against the protocol's own OpenAPI document, not merely against this
// project's reading of it.
//
// Only the statuses OFREP documents for the single flag endpoint are checked.
// The 503 ddflagd returns while the provider holds no configuration, and the 501
// on the bulk endpoint, are deliberate extensions that the document does not
// describe, so they are covered by the handler tests instead.
func TestResponsesSatisfyTheOFREPSchema(t *testing.T) {
	t.Parallel()

	v := newSpecValidator(t)

	tests := []struct {
		name       string
		resolution openfeature.InterfaceResolutionDetail
		body       string
		wantStatus int
	}{
		{
			name: "a boolean value",
			resolution: openfeature.InterfaceResolutionDetail{
				Value: true,
				ProviderResolutionDetail: openfeature.ProviderResolutionDetail{
					Reason:       openfeature.TargetingMatchReason,
					Variant:      "on",
					FlagMetadata: ddMetadata(),
				},
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "a string value",
			resolution: openfeature.InterfaceResolutionDetail{
				Value: "green",
				ProviderResolutionDetail: openfeature.ProviderResolutionDetail{
					Reason:  openfeature.StaticReason,
					Variant: "green",
				},
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "a number value",
			resolution: openfeature.InterfaceResolutionDetail{
				Value: 3.1415926,
				ProviderResolutionDetail: openfeature.ProviderResolutionDetail{
					Reason:  openfeature.StaticReason,
					Variant: "pi",
				},
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "an object value",
			resolution: openfeature.InterfaceResolutionDetail{
				Value: map[string]any{"integer": 1, "string": "one"},
				ProviderResolutionDetail: openfeature.ProviderResolutionDetail{
					Reason:  openfeature.SplitReason,
					Variant: "one",
				},
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "a code default, which the protocol models as a success without a value",
			resolution: openfeature.InterfaceResolutionDetail{
				ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.DefaultReason},
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "a disabled flag",
			resolution: openfeature.InterfaceResolutionDetail{
				ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.DisabledReason},
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "a flag that does not exist",
			resolution: openfeature.InterfaceResolutionDetail{
				ProviderResolutionDetail: openfeature.ProviderResolutionDetail{
					ResolutionError: openfeature.NewFlagNotFoundResolutionError("flag not found"),
					Reason:          openfeature.ErrorReason,
				},
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "a missing targeting key",
			resolution: openfeature.InterfaceResolutionDetail{
				ProviderResolutionDetail: openfeature.ProviderResolutionDetail{
					ResolutionError: openfeature.NewTargetingKeyMissingResolutionError("targeting key missing"),
					Reason:          openfeature.ErrorReason,
				},
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "a rejected evaluation context",
			resolution: openfeature.InterfaceResolutionDetail{
				Value:                    true,
				ProviderResolutionDetail: openfeature.ProviderResolutionDetail{Reason: openfeature.StaticReason},
			},
			body:       `{"context":{"targetingKey":"alice","user":{"id":"1"}}}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "an internal error",
			resolution: openfeature.InterfaceResolutionDetail{
				ProviderResolutionDetail: openfeature.ProviderResolutionDetail{
					ResolutionError: openfeature.NewGeneralResolutionError("boom"),
					Reason:          openfeature.ErrorReason,
				},
			},
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			provider := &scriptedProvider{results: map[string]openfeature.InterfaceResolutionDetail{"flag": tt.resolution}}
			srv := httptest.NewServer(newTestHandler(t, provider, bridge.StateReady, nil, 0))
			t.Cleanup(srv.Close)

			body := tt.body
			if body == "" {
				body = `{"context":{"targetingKey":"alice"}}`
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
				srv.URL+"/ofrep/v1/evaluate/flags/flag", bytes.NewBufferString(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")

			res, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := res.Body.Close(); err != nil {
					t.Error(err)
				}
			})

			if res.StatusCode != tt.wantStatus {
				t.Fatalf("status: got %d, want %d", res.StatusCode, tt.wantStatus)
			}

			ok, validationErrs := v.ValidateHttpResponse(req, res)
			if !ok {
				for _, e := range validationErrs {
					t.Errorf("%s: %s (spec line %d)", e.Message, e.Reason, e.SpecLine)
					for _, sv := range e.SchemaValidationErrors {
						t.Errorf("  %s", sv.Reason)
					}
				}
				t.Fatalf("the response does not satisfy the OFREP schema")
			}
		})
	}
}

func newSpecValidator(t *testing.T) validator.Validator {
	t.Helper()

	spec, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("reading the OFREP document: %v", err)
	}
	if got := bytes.Count(spec, []byte(codeDefaultSchema)); got != 1 {
		t.Fatalf("the codeDefaultFlag schema appears %d times in the OFREP document, want 1: the document changed and the constraint below needs revisiting", got)
	}
	spec = bytes.Replace(spec, []byte(codeDefaultSchema), []byte(codeDefaultSchemaFixed), 1)

	doc, err := libopenapi.NewDocument(spec)
	if err != nil {
		t.Fatalf("parsing the OFREP document: %v", err)
	}
	v, errs := validator.NewValidator(doc)
	if len(errs) > 0 {
		t.Fatalf("building the validator: %v", errs)
	}
	return v
}
