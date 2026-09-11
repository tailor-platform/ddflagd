package ofrep

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/open-feature/go-sdk/openfeature"

	"github.com/k1LoW/ddflagd/internal/metrics"
)

// OFREP reasons. The protocol's reason enum is narrower than OpenFeature's, so
// a reason outside this set is reported as UNKNOWN.
const (
	reasonStatic         = "STATIC"
	reasonTargetingMatch = "TARGETING_MATCH"
	reasonSplit          = "SPLIT"
	reasonDisabled       = "DISABLED"
	reasonUnknown        = "UNKNOWN"
)

// successResponse is an OFREP evaluation success.
//
// Value is a pointer so that a nil value is omitted from the JSON while false,
// 0 and "" are still sent. An omitted value is the protocol's code default
// response, which is how the provider's DEFAULT and DISABLED reasons are
// reported without inventing a value here.
type successResponse struct {
	Key      string         `json:"key"`
	Reason   string         `json:"reason"`
	Variant  string         `json:"variant,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	Value    *any           `json:"value,omitempty"`
}

// failureResponse is an OFREP evaluation failure.
type failureResponse struct {
	Key          string         `json:"key"`
	ErrorCode    string         `json:"errorCode"`
	ErrorDetails string         `json:"errorDetails,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

// mapResult turns the result of an OpenFeature object evaluation into an OFREP
// response.
func mapResult(key string, details openfeature.InterfaceEvaluationDetails, err error) (int, any, metrics.Outcome) {
	metadata := ofrepMetadata(details.FlagMetadata)

	// The client short circuits a provider that is not ready before it reaches
	// the provider, and reports that through the error alone: the details carry
	// no error code in that case.
	if details.ErrorCode == "" && err != nil {
		status, code, outcome := mapShortCircuit(err)
		return status, failureResponse{
			Key:          key,
			ErrorCode:    code,
			ErrorDetails: err.Error(),
			Metadata:     metadata,
		}, outcome
	}

	if details.ErrorCode != "" {
		errorDetails := details.ErrorMessage
		if errorDetails == "" && err != nil {
			errorDetails = err.Error()
		}
		status, code, outcome := mapErrorCode(details.ErrorCode)
		return status, failureResponse{
			Key:          key,
			ErrorCode:    code,
			ErrorDetails: errorDetails,
			Metadata:     metadata,
		}, outcome
	}

	reason, hasValue := mapReason(details.Reason)
	// A nil value with a value bearing reason cannot be turned into a typed
	// value by the caller either, so it is reported as a code default.
	if details.Value == nil {
		hasValue = false
		if reason != reasonDisabled {
			reason = reasonUnknown
		}
	}

	res := successResponse{Key: key, Reason: reason, Metadata: metadata}
	outcome := metrics.OutcomeCodeDefault
	if hasValue {
		value := details.Value
		res.Value = &value
		res.Variant = details.Variant
		outcome = metrics.OutcomeValue
	}
	return http.StatusOK, res, outcome
}

// mapReason maps an OpenFeature reason onto the OFREP reason enum and reports
// whether the response carries a value.
//
// DEFAULT means no allocation matched, or that the flag's configuration was
// rejected, and the value is whatever default the caller passed. OFREP has no
// DEFAULT reason, so it becomes UNKNOWN with the value omitted.
func mapReason(reason openfeature.Reason) (string, bool) {
	switch reason {
	case openfeature.TargetingMatchReason:
		return reasonTargetingMatch, true
	case openfeature.SplitReason:
		return reasonSplit, true
	case openfeature.StaticReason:
		return reasonStatic, true
	case openfeature.DisabledReason:
		return reasonDisabled, false
	case openfeature.DefaultReason:
		return reasonUnknown, false
	default:
		return reasonUnknown, true
	}
}

// mapShortCircuit classifies an evaluation error that carries no error code.
func mapShortCircuit(err error) (int, string, metrics.Outcome) {
	switch {
	case errors.Is(err, openfeature.ProviderNotReadyError):
		return http.StatusServiceUnavailable, errorCodeProviderNotReady, metrics.OutcomeNotReady
	case errors.Is(err, openfeature.ProviderFatalError):
		return http.StatusInternalServerError, string(openfeature.ProviderFatalCode), metrics.OutcomeError
	default:
		return http.StatusInternalServerError, string(openfeature.GeneralCode), metrics.OutcomeError
	}
}

func mapErrorCode(code openfeature.ErrorCode) (int, string, metrics.Outcome) {
	switch code {
	case openfeature.FlagNotFoundCode:
		return http.StatusNotFound, string(openfeature.FlagNotFoundCode), metrics.OutcomeFlagNotFound
	case openfeature.TargetingKeyMissingCode:
		return http.StatusBadRequest, string(openfeature.TargetingKeyMissingCode), metrics.OutcomeInvalidRequest
	case openfeature.InvalidContextCode:
		return http.StatusBadRequest, string(openfeature.InvalidContextCode), metrics.OutcomeInvalidRequest
	case openfeature.ParseErrorCode:
		return http.StatusBadRequest, string(openfeature.ParseErrorCode), metrics.OutcomeInvalidRequest
	case openfeature.ProviderNotReadyCode:
		return http.StatusServiceUnavailable, errorCodeProviderNotReady, metrics.OutcomeNotReady
	default:
		return http.StatusInternalServerError, string(code), metrics.OutcomeError
	}
}

// ofrepMetadata keeps the flag metadata entries OFREP allows, which are
// booleans, strings and numbers. Anything else is dropped rather than coerced.
func ofrepMetadata(in openfeature.FlagMetadata) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for name, value := range in {
		switch v := value.(type) {
		case bool, string,
			int, int8, int16, int32, int64,
			uint, uint8, uint16, uint32, uint64,
			float32, float64, json.Number:
			out[name] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
