// Package metrics collects the operational metrics of ddflagd and renders them
// in the Prometheus text exposition format.
package metrics

import (
	"fmt"
	"io"
	"maps"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Outcome classifies the result of an OFREP evaluation request. It is the label
// value of ddflagd_evaluations_total and ddflagd_evaluation_duration_seconds.
type Outcome string

// Outcomes of an OFREP evaluation request.
const (
	// OutcomeValue is a 200 response carrying a value.
	OutcomeValue Outcome = "value"
	// OutcomeCodeDefault is a 200 response without a value, which asks the
	// caller to fall back to its code default.
	OutcomeCodeDefault Outcome = "code_default"
	// OutcomeFlagNotFound is a 404 response.
	OutcomeFlagNotFound Outcome = "flag_not_found"
	// OutcomeInvalidRequest is a 400 response.
	OutcomeInvalidRequest Outcome = "invalid_request"
	// OutcomeNotReady is a 503 response caused by the provider not holding a
	// flag configuration yet, or by the bridge shutting down.
	OutcomeNotReady Outcome = "not_ready"
	// OutcomeTimeout is a 503 response caused by the handler timeout.
	OutcomeTimeout Outcome = "timeout"
	// OutcomeError is a 500 response.
	OutcomeError Outcome = "error"
)

// Outcomes lists every outcome in a stable order.
var Outcomes = []Outcome{
	OutcomeValue,
	OutcomeCodeDefault,
	OutcomeFlagNotFound,
	OutcomeInvalidRequest,
	OutcomeNotReady,
	OutcomeTimeout,
	OutcomeError,
}

// buckets are the upper bounds of ddflagd_evaluation_duration_seconds. They are
// dense below a millisecond because a sidecar evaluation is a loopback request
// plus a map lookup, and they reach past the default handler timeout (200ms) so
// that a timing out deployment is still visible in the histogram.
var buckets = []float64{
	0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1,
}

// LastError is the most recent error reported to the metrics, exposed through
// /debug/status.
type LastError struct {
	At      time.Time `json:"at"`
	Message string    `json:"message"`
}

// Metrics holds every value ddflagd observes about itself. It is safe for
// concurrent use.
type Metrics struct {
	version          string
	ddTraceGoVersion string

	mu          sync.Mutex
	evaluations map[Outcome]uint64
	durations   map[Outcome]*histogram
	providerRdy bool
	providerAt  time.Time
	lastError   *LastError
	nowFunc     func() time.Time
}

type histogram struct {
	counts []uint64
	sum    float64
	total  uint64
}

// New returns Metrics labeled with the given ddflagd version. The dd-trace-go
// version is read from the build info of the running binary.
func New(version string) *Metrics {
	return &Metrics{
		version:          version,
		ddTraceGoVersion: DDTraceGoVersion(),
		evaluations:      make(map[Outcome]uint64, len(Outcomes)),
		durations:        make(map[Outcome]*histogram, len(Outcomes)),
		nowFunc:          time.Now,
	}
}

// SetNowFunc replaces the clock used for timestamps. It exists for tests.
func (m *Metrics) SetNowFunc(f func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nowFunc = f
}

// DDTraceGoVersion reports the version of dd-trace-go this binary was built
// against, or "unknown" when the build info is unavailable. A test binary is
// one such case: its build info carries no dependency list.
func DDTraceGoVersion() string {
	const modulePath = "github.com/DataDog/dd-trace-go/v2"
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	if info.Main.Path == modulePath && info.Main.Version != "" {
		return info.Main.Version
	}
	for _, dep := range info.Deps {
		if dep.Path != modulePath {
			continue
		}
		if dep.Replace != nil && dep.Replace.Version != "" {
			return dep.Replace.Version
		}
		if dep.Version != "" {
			return dep.Version
		}
	}
	return "unknown"
}

// ObserveEvaluation records one OFREP evaluation request and how long it took.
func (m *Metrics) ObserveEvaluation(outcome Outcome, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.evaluations[outcome]++
	h, ok := m.durations[outcome]
	if !ok {
		h = &histogram{counts: make([]uint64, len(buckets))}
		m.durations[outcome] = h
	}
	seconds := d.Seconds()
	h.sum += seconds
	h.total++
	for i, ub := range buckets {
		if seconds <= ub {
			h.counts[i]++
		}
	}
}

// SetProviderReady marks the official provider as ready at the given time.
func (m *Metrics) SetProviderReady(at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.providerRdy = true
	m.providerAt = at
}

// RecordError stores msg as the most recent error.
func (m *Metrics) RecordError(msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastError = &LastError{At: m.nowFunc(), Message: msg}
}

// Snapshot is a point-in-time copy of the values /debug/status reports.
type Snapshot struct {
	Version          string
	DDTraceGoVersion string
	Evaluations      map[Outcome]uint64
	ProviderReady    bool
	ProviderReadyAt  time.Time
	LastError        *LastError
}

// Snapshot returns a copy of the current values.
func (m *Metrics) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Snapshot{
		Version:          m.version,
		DDTraceGoVersion: m.ddTraceGoVersion,
		Evaluations:      make(map[Outcome]uint64, len(m.evaluations)),
		ProviderReady:    m.providerRdy,
		ProviderReadyAt:  m.providerAt,
	}
	maps.Copy(s.Evaluations, m.evaluations)
	if m.lastError != nil {
		e := *m.lastError
		s.LastError = &e
	}
	return s
}

// Write renders the metrics in the Prometheus text exposition format.
func (m *Metrics) Write(w io.Writer) error {
	_, err := io.WriteString(w, m.render())
	return err
}

// render builds the exposition text. The write itself happens outside the lock,
// so a slow scraper cannot hold up an evaluation.
func (m *Metrics) render() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	var b strings.Builder

	b.WriteString("# HELP ddflagd_build_info Build information of the running ddflagd.\n")
	b.WriteString("# TYPE ddflagd_build_info gauge\n")
	fmt.Fprintf(&b, "ddflagd_build_info{version=%q,dd_trace_go_version=%q} 1\n", m.version, m.ddTraceGoVersion)

	b.WriteString("# HELP ddflagd_evaluations_total Total number of OFREP evaluation requests by outcome.\n")
	b.WriteString("# TYPE ddflagd_evaluations_total counter\n")
	for _, outcome := range m.knownOutcomes() {
		fmt.Fprintf(&b, "ddflagd_evaluations_total{outcome=%q} %d\n", outcome, m.evaluations[outcome])
	}

	b.WriteString("# HELP ddflagd_evaluation_duration_seconds Duration of OFREP evaluation requests by outcome.\n")
	b.WriteString("# TYPE ddflagd_evaluation_duration_seconds histogram\n")
	for _, outcome := range m.knownOutcomes() {
		h, ok := m.durations[outcome]
		if !ok {
			continue
		}
		// h.counts is already cumulative: an observation increments every
		// bucket whose upper bound it falls under.
		for i, ub := range buckets {
			fmt.Fprintf(&b, "ddflagd_evaluation_duration_seconds_bucket{outcome=%q,le=%q} %d\n",
				outcome, strconv.FormatFloat(ub, 'g', -1, 64), h.counts[i])
		}
		fmt.Fprintf(&b, "ddflagd_evaluation_duration_seconds_bucket{outcome=%q,le=\"+Inf\"} %d\n", outcome, h.total)
		fmt.Fprintf(&b, "ddflagd_evaluation_duration_seconds_sum{outcome=%q} %s\n",
			outcome, strconv.FormatFloat(h.sum, 'g', -1, 64))
		fmt.Fprintf(&b, "ddflagd_evaluation_duration_seconds_count{outcome=%q} %d\n", outcome, h.total)
	}

	b.WriteString("# HELP ddflagd_provider_ready Whether the official Datadog provider holds a flag configuration.\n")
	b.WriteString("# TYPE ddflagd_provider_ready gauge\n")
	ready := 0
	if m.providerRdy {
		ready = 1
	}
	fmt.Fprintf(&b, "ddflagd_provider_ready %d\n", ready)

	b.WriteString("# HELP ddflagd_provider_ready_timestamp_seconds Unix time at which the provider became ready.\n")
	b.WriteString("# TYPE ddflagd_provider_ready_timestamp_seconds gauge\n")
	var readyAt float64
	if !m.providerAt.IsZero() {
		readyAt = float64(m.providerAt.UnixNano()) / float64(time.Second)
	}
	fmt.Fprintf(&b, "ddflagd_provider_ready_timestamp_seconds %s\n", strconv.FormatFloat(readyAt, 'f', -1, 64))

	return b.String()
}

// knownOutcomes returns the full set of outcomes once anything has been
// observed, so that a scraper sees a zero series rather than a gap for an
// outcome that has not happened yet. Before the first evaluation it returns
// nothing, which keeps a counter that has never been touched out of the output.
func (m *Metrics) knownOutcomes() []Outcome {
	if len(m.evaluations) == 0 {
		return nil
	}
	return slices.Clone(Outcomes)
}
