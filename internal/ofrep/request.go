package ofrep

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/open-feature/go-sdk/openfeature"

	"github.com/k1LoW/ddflagd/internal/metrics"
)

// errorCodeProviderNotReady is an OFREP extension. The protocol has no error
// code for "the flag management system is not ready yet", and PROVIDER_NOT_READY
// is the OpenFeature error code for exactly that state.
const errorCodeProviderNotReady = "PROVIDER_NOT_READY"

// evaluationRequest is the OFREP request body. The context is kept raw so that
// each attribute's JSON type can be checked before it is accepted.
type evaluationRequest struct {
	Context map[string]json.RawMessage `json:"context"`
}

type requestFailure struct {
	status  int
	body    any
	outcome metrics.Outcome
}

func invalidRequest(key, code, details string) *requestFailure {
	return &requestFailure{
		status:  http.StatusBadRequest,
		body:    failureResponse{Key: key, ErrorCode: code, ErrorDetails: details},
		outcome: metrics.OutcomeInvalidRequest,
	}
}

// decodeContext reads the request body and converts the OFREP context into an
// OpenFeature evaluation context.
//
// Datadog only supports flat primitive attributes, and an exposure event drops
// anything else without a word, so a nested or array valued attribute is
// rejected here to make that constraint visible to the caller.
func (h *Handler) decodeContext(r *http.Request, key string) (*openfeature.EvaluationContext, *requestFailure) {
	if key == "" {
		return nil, invalidRequest(key, string(openfeature.GeneralCode), "the flag key is empty")
	}
	if !utf8.ValidString(key) {
		return nil, invalidRequest(key, string(openfeature.ParseErrorCode), "the flag key is not a UTF-8 encoded string")
	}

	body := http.MaxBytesReader(nil, r.Body, h.maxBodyBytes)
	dec := json.NewDecoder(body)
	dec.UseNumber()

	var req evaluationRequest
	if err := dec.Decode(&req); err != nil {
		if maxBytes, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return nil, &requestFailure{
				status:  http.StatusRequestEntityTooLarge,
				body:    failureResponse{Key: key, ErrorCode: string(openfeature.ParseErrorCode), ErrorDetails: fmt.Sprintf("the request body exceeds %d bytes", maxBytes.Limit)},
				outcome: metrics.OutcomeInvalidRequest,
			}
		}
		return nil, invalidRequest(key, string(openfeature.ParseErrorCode), fmt.Sprintf("the request body is not valid JSON: %v", err))
	}

	if len(req.Context) > h.maxAttributes {
		return nil, invalidRequest(key, string(openfeature.InvalidContextCode),
			fmt.Sprintf("the evaluation context has %d properties, the limit is %d", len(req.Context), h.maxAttributes))
	}

	var targetingKey string
	attributes := make(map[string]any, len(req.Context))
	// The properties are walked in a fixed order so that a request with more
	// than one problem always names the same one.
	for _, name := range sortedKeys(req.Context) {
		raw := req.Context[name]
		if name == openfeature.TargetingKey {
			tk, ok, err := decodeTargetingKey(raw)
			if err != nil {
				return nil, invalidRequest(key, string(openfeature.InvalidContextCode), err.Error())
			}
			// The targeting key becomes the subject id of the exposure event,
			// so it is bounded like any other string the caller sends.
			if len(tk) > h.maxStringBytes {
				return nil, invalidRequest(key, string(openfeature.InvalidContextCode),
					fmt.Sprintf("%s is %d bytes, the limit is %d", openfeature.TargetingKey, len(tk), h.maxStringBytes))
			}
			if ok {
				targetingKey = tk
			}
			continue
		}
		value, err := h.decodeAttribute(name, raw)
		if err != nil {
			return nil, invalidRequest(key, string(openfeature.InvalidContextCode), err.Error())
		}
		attributes[name] = value
	}

	// A missing targeting key is passed through rather than rejected: whether
	// one is required depends on the flag's allocations, and the official
	// provider is the component that knows. It answers TARGETING_KEY_MISSING
	// when a shard evaluation needs one.
	evalCtx := openfeature.NewEvaluationContext(targetingKey, attributes)
	return &evalCtx, nil
}

// decodeTargetingKey accepts a string or null. A null targeting key means the
// caller has none, which the shared Datadog test fixtures use to cover the
// missing targeting key path.
func decodeTargetingKey(raw json.RawMessage) (string, bool, error) {
	if isJSONNull(raw) {
		return "", false, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false, fmt.Errorf("%s must be a string", openfeature.TargetingKey)
	}
	return s, true, nil
}

func (h *Handler) decodeAttribute(name string, raw json.RawMessage) (any, error) {
	trimmed := strings.TrimSpace(string(raw))
	switch {
	case isJSONNull(raw):
		// A null attribute is meaningful rather than malformed: the official
		// provider's IS_NULL operator treats a null and an absent attribute
		// alike, and Datadog's shared evaluation fixtures send nulls.
		return nil, nil
	case strings.HasPrefix(trimmed, "{"):
		return nil, fmt.Errorf("the property %q is an object, the evaluation context accepts only flat primitives because Datadog drops anything else from the exposure event", name)
	case strings.HasPrefix(trimmed, "["):
		return nil, fmt.Errorf("the property %q is an array, the evaluation context accepts only flat primitives because Datadog drops anything else from the exposure event", name)
	}

	var value any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return nil, fmt.Errorf("the property %q is not valid JSON: %w", name, err)
	}

	switch v := value.(type) {
	case string:
		if len(v) > h.maxStringBytes {
			return nil, fmt.Errorf("the property %q is %d bytes, the limit is %d", name, len(v), h.maxStringBytes)
		}
		return v, nil
	case bool:
		return v, nil
	case json.Number:
		return decodeNumber(name, v)
	default:
		return nil, fmt.Errorf("the property %q has an unsupported type, the evaluation context accepts strings, numbers and booleans", name)
	}
}

// decodeNumber keeps an integral literal an integer. The official provider
// compares an attribute against a condition by the attribute's Go type, and an
// integer stays an integer all the way into the exposure event this way.
func decodeNumber(name string, n json.Number) (any, error) {
	if !strings.ContainsAny(n.String(), ".eE") {
		if i, err := n.Int64(); err == nil {
			return i, nil
		}
	}
	f, err := n.Float64()
	if err != nil {
		return nil, fmt.Errorf("the property %q is not a representable number: %w", name, err)
	}
	return f, nil
}

func isJSONNull(raw json.RawMessage) bool {
	return len(raw) == 0 || strings.TrimSpace(string(raw)) == "null"
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
