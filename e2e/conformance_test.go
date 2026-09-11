//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const casesDir = "../testdata/ffe-system-test-data/evaluation-cases"

// evaluationCase is one entry of a shared Datadog evaluation case file.
type evaluationCase struct {
	Description   string         `json:"description"`
	Flag          string         `json:"flag"`
	VariationType string         `json:"variationType"`
	DefaultValue  any            `json:"defaultValue"`
	TargetingKey  *string        `json:"targetingKey"`
	Attributes    map[string]any `json:"attributes"`
	Result        struct {
		Value     any    `json:"value"`
		Reason    string `json:"reason"`
		ErrorCode string `json:"errorCode"`
	} `json:"result"`
}

// TestConformance runs every case of Datadog's cross language evaluation
// fixtures through OFREP.
//
// It does not check the evaluation logic, which belongs to the official SDK. It
// checks that the request conversion and the OFREP mapping preserve the value
// and the reason of every case, including the ones the protocol expresses by
// omitting the value.
func TestConformance(t *testing.T) {
	requireSetup(t)

	files, err := filepath.Glob(filepath.Join(casesDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no evaluation cases in %s: run git submodule update --init", casesDir)
	}

	var total int
	for _, file := range files {
		t.Run(strings.TrimSuffix(filepath.Base(file), ".json"), func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var cases []evaluationCase
			if err := json.Unmarshal(raw, &cases); err != nil {
				t.Fatalf("decoding %s: %v", file, err)
			}

			for i, c := range cases {
				t.Run(caseName(i, c), func(t *testing.T) {
					runCase(t, c)
				})
			}
			total += len(cases)
		})
	}
	if total == 0 {
		t.Fatal("the evaluation case files contain no cases")
	}
	t.Logf("%d evaluation cases from %d files", total, len(files))
}

func runCase(t *testing.T, c evaluationCase) {
	t.Helper()

	evalContext := make(map[string]any, len(c.Attributes)+1)
	maps.Copy(evalContext, c.Attributes)
	// A null targeting key in the fixtures means the caller has none, so the
	// property is left out of the request rather than sent as null.
	if c.TargetingKey != nil {
		evalContext["targetingKey"] = *c.TargetingKey
	}

	status, body, err := bridge.Evaluate(t.Context(), c.Flag, evalContext)
	if err != nil {
		t.Fatalf("evaluating %s: %v", c.Flag, err)
	}

	switch c.Result.Reason {
	case "ERROR":
		wantStatus, wantCode := expectedError(c.Result.ErrorCode)
		if status != wantStatus {
			t.Fatalf("status: got %d, want %d (%v)", status, wantStatus, body)
		}
		if got := body["errorCode"]; got != wantCode {
			t.Errorf("errorCode: got %#v, want %s", got, wantCode)
		}
		if _, ok := body["value"]; ok {
			t.Errorf("a failure must not carry a value: %v", body)
		}

	case "DEFAULT", "DISABLED":
		// Both reasons mean "use the value from your own code". OFREP says that
		// by omitting the value, and has no DEFAULT reason, so DEFAULT is
		// reported as UNKNOWN.
		wantReason := "UNKNOWN"
		if c.Result.Reason == "DISABLED" {
			wantReason = "DISABLED"
		}
		if status != 200 {
			t.Fatalf("status: got %d, want 200 (%v)", status, body)
		}
		if got := body["reason"]; got != wantReason {
			t.Errorf("reason: got %#v, want %s", got, wantReason)
		}
		if _, ok := body["value"]; ok {
			t.Errorf("a code default must omit the value: %v", body)
		}
		if _, ok := body["variant"]; ok {
			t.Errorf("a code default must omit the variant: %v", body)
		}

	default:
		if status != 200 {
			t.Fatalf("status: got %d, want 200 (%v)", status, body)
		}
		if got := body["reason"]; got != c.Result.Reason {
			t.Errorf("reason: got %#v, want %s", got, c.Result.Reason)
		}
		got, ok := body["value"]
		if !ok {
			t.Fatalf("the response carries no value: %v", body)
		}
		want := normalize(c.Result.Value)
		if !reflect.DeepEqual(normalize(got), want) {
			t.Errorf("value: got %#v, want %#v", got, c.Result.Value)
		}
	}
}

// expectedError maps a fixture's expected error onto the HTTP status and OFREP
// error code ddflagd answers with.
//
// A fixture with reason ERROR and no error code is the missing targeting key
// case, which OFREP models as TARGETING_KEY_MISSING.
func expectedError(errorCode string) (int, string) {
	switch errorCode {
	case "FLAG_NOT_FOUND":
		return 404, "FLAG_NOT_FOUND"
	case "PARSE_ERROR":
		return 400, "PARSE_ERROR"
	case "":
		return 400, "TARGETING_KEY_MISSING"
	default:
		return 500, errorCode
	}
}

// normalize brings a decoded JSON value to a comparable shape. Both sides of
// the comparison come from JSON, so only the numeric kinds need aligning.
func normalize(v any) any {
	switch value := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for name, inner := range value {
			out[name] = normalize(inner)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, inner := range value {
			out[i] = normalize(inner)
		}
		return out
	case json.Number:
		f, err := value.Float64()
		if err != nil {
			return value.String()
		}
		return f
	case int:
		return float64(value)
	case int64:
		return float64(value)
	default:
		return v
	}
}

func caseName(i int, c evaluationCase) string {
	targetingKey := "none"
	if c.TargetingKey != nil {
		targetingKey = *c.TargetingKey
		if targetingKey == "" {
			targetingKey = "empty"
		}
	}
	return fmt.Sprintf("%d_%s_%s", i, c.Flag, targetingKey)
}
