package metrics

import (
	"strings"
	"testing"
	"time"
)

// TestObserveEvaluationCountsByOutcome states that the request results are
// counted separately, which is how the share of evaluations that fell back to a
// code default is monitored. The caller cannot tell those apart itself.
func TestObserveEvaluationCountsByOutcome(t *testing.T) {
	t.Parallel()

	m := New("1.2.3")
	m.ObserveEvaluation(OutcomeValue, time.Millisecond)
	m.ObserveEvaluation(OutcomeValue, 2*time.Millisecond)
	m.ObserveEvaluation(OutcomeCodeDefault, time.Millisecond)

	snapshot := m.Snapshot()
	if got := snapshot.Evaluations[OutcomeValue]; got != 2 {
		t.Errorf("value: got %d, want 2", got)
	}
	if got := snapshot.Evaluations[OutcomeCodeDefault]; got != 1 {
		t.Errorf("code_default: got %d, want 1", got)
	}
	if got := snapshot.Evaluations[OutcomeError]; got != 0 {
		t.Errorf("error: got %d, want 0", got)
	}
	if snapshot.Version != "1.2.3" {
		t.Errorf("version: got %q, want 1.2.3", snapshot.Version)
	}
}

// TestHistogramBucketsAreCumulative states the Prometheus histogram invariant:
// each bucket counts every observation at or below its upper bound.
func TestHistogramBucketsAreCumulative(t *testing.T) {
	t.Parallel()

	m := New("test")
	for _, d := range []time.Duration{100 * time.Microsecond, 3 * time.Millisecond, 2 * time.Second} {
		m.ObserveEvaluation(OutcomeValue, d)
	}

	var b strings.Builder
	if err := m.Write(&b); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := b.String()

	for _, want := range []string{
		`ddflagd_evaluation_duration_seconds_bucket{outcome="value",le="0.0001"} 1`,
		`ddflagd_evaluation_duration_seconds_bucket{outcome="value",le="0.005"} 2`,
		`ddflagd_evaluation_duration_seconds_bucket{outcome="value",le="1"} 2`,
		`ddflagd_evaluation_duration_seconds_bucket{outcome="value",le="+Inf"} 3`,
		`ddflagd_evaluation_duration_seconds_count{outcome="value"} 3`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// TestWriteWithoutObservations states that a freshly started bridge still
// exposes the provider state, so that a scrape before the first evaluation is
// not empty.
func TestWriteWithoutObservations(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	if err := New("test").Write(&b); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := b.String()

	if !strings.Contains(out, "ddflagd_provider_ready 0") {
		t.Errorf("missing the provider state:\n%s", out)
	}
	if !strings.Contains(out, "ddflagd_provider_ready_timestamp_seconds 0") {
		t.Errorf("missing the readiness timestamp:\n%s", out)
	}
	if strings.Contains(out, "ddflagd_evaluations_total{") {
		t.Errorf("an unobserved counter should not be reported:\n%s", out)
	}
}

// TestRecordErrorKeepsTheLatest states that /debug/status reports the most
// recent failure.
func TestRecordErrorKeepsTheLatest(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	m := New("test")
	m.SetNowFunc(func() time.Time { return at })

	if m.Snapshot().LastError != nil {
		t.Fatal("a fresh registry must report no error")
	}
	m.RecordError("first")
	m.RecordError("second")

	last := m.Snapshot().LastError
	if last == nil {
		t.Fatal("want a recorded error")
	}
	if last.Message != "second" {
		t.Errorf("message: got %q, want second", last.Message)
	}
	if !last.At.Equal(at) {
		t.Errorf("at: got %s, want %s", last.At, at)
	}
}

// TestSnapshotIsACopy states that a snapshot does not alias the registry.
func TestSnapshotIsACopy(t *testing.T) {
	t.Parallel()

	m := New("test")
	m.ObserveEvaluation(OutcomeValue, time.Millisecond)
	m.RecordError("boom")

	snapshot := m.Snapshot()
	snapshot.Evaluations[OutcomeValue] = 99
	snapshot.LastError.Message = "changed"

	fresh := m.Snapshot()
	if got := fresh.Evaluations[OutcomeValue]; got != 1 {
		t.Errorf("value: got %d, want 1", got)
	}
	if got := fresh.LastError.Message; got != "boom" {
		t.Errorf("message: got %q, want boom", got)
	}
}

// TestDDTraceGoVersionAlwaysAnswers states that the version lookup never
// returns an empty label, since it is a Prometheus label value. This package
// does not link dd-trace-go, so the concrete version is asserted where it is
// linked, in the operational listener's test.
func TestDDTraceGoVersionAlwaysAnswers(t *testing.T) {
	t.Parallel()

	if got := DDTraceGoVersion(); got == "" {
		t.Fatal("the dd-trace-go version label must never be empty")
	}
}
