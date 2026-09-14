package bridge

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	ddopenfeature "github.com/DataDog/dd-trace-go/v2/openfeature"
	"github.com/open-feature/go-sdk/openfeature"
	"github.com/open-feature/go-sdk/openfeature/memprovider"

	"github.com/tailor-platform/ddflagd/internal/metrics"
)

// TestCheckDatadogProvider states that a provider which is not the Datadog one
// fails startup. The official constructor returns a NoopProvider instead of an
// error when the experimental flagging provider is not enabled, and a bridge
// that silently evaluates nothing is worse than one that refuses to start.
func TestCheckDatadogProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		provider openfeature.FeatureProvider
		wantErr  bool
	}{
		{
			name:     "a NoopProvider is rejected",
			provider: &openfeature.NoopProvider{},
			wantErr:  true,
		},
		{
			name:     "a NoopProvider value is rejected",
			provider: openfeature.NoopProvider{},
			wantErr:  true,
		},
		{
			name:     "another provider is rejected",
			provider: memprovider.NewInMemoryProvider(nil),
			wantErr:  true,
		},
		{
			name:     "a Datadog provider is accepted",
			provider: &ddopenfeature.DatadogProvider{},
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := checkDatadogProvider(tt.provider)
			if tt.wantErr {
				if err == nil {
					t.Fatal("want an error")
				}
				if !errors.Is(err, ErrProviderNotDatadog) {
					t.Errorf("error: got %v, want it to wrap ErrProviderNotDatadog", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("want no error, got %v", err)
			}
		})
	}
}

// TestNewRejectsNoopProvider states that the check applies whichever
// constructor produced the provider.
func TestNewRejectsNoopProvider(t *testing.T) {
	t.Parallel()

	_, err := New(context.Background(), Options{
		Config:      testConfig(),
		NewProvider: func(context.Context) (openfeature.FeatureProvider, error) { return &openfeature.NoopProvider{}, nil },
	})
	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, ErrProviderNotDatadog) {
		t.Errorf("error: got %v, want it to wrap ErrProviderNotDatadog", err)
	}
}

// TestNewPropagatesProviderError states that a provider that cannot be created
// stops startup.
func TestNewPropagatesProviderError(t *testing.T) {
	t.Parallel()

	want := errors.New("no agent")
	_, err := New(context.Background(), Options{
		Config:      testConfig(),
		NewProvider: func(context.Context) (openfeature.FeatureProvider, error) { return nil, want },
	})
	if !errors.Is(err, want) {
		t.Fatalf("error: got %v, want %v", err, want)
	}
}

// TestNewRequiresConfig states that a bridge cannot be built without a
// configuration.
func TestNewRequiresConfig(t *testing.T) {
	t.Parallel()

	if _, err := New(context.Background(), Options{}); err == nil {
		t.Fatal("want an error when Config is nil")
	}
}

// TestStartBecomesReady states the state machine on a successful start.
func TestStartBecomesReady(t *testing.T) {
	t.Parallel()

	readyAt := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	m := metrics.New("test")
	b := newTestBridge(t, testConfig(), memprovider.NewInMemoryProvider(nil), m, func() time.Time { return readyAt })

	if got := b.State(); got != StateStarting {
		t.Fatalf("state before Start: got %s, want %s", got, StateStarting)
	}
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := b.State(); got != StateReady {
		t.Fatalf("state after Start: got %s, want %s", got, StateReady)
	}

	status := b.Status()
	if !status.ReadyAt.Equal(readyAt) {
		t.Errorf("ReadyAt: got %s, want %s", status.ReadyAt, readyAt)
	}
	snapshot := m.Snapshot()
	if !snapshot.ProviderReady || !snapshot.ProviderReadyAt.Equal(readyAt) {
		t.Errorf("metrics: got ready=%v at=%s, want ready at %s", snapshot.ProviderReady, snapshot.ProviderReadyAt, readyAt)
	}
}

// TestStartTimesOut states that a provider which never receives a flag
// configuration fails startup, so that Kubernetes restarts the container rather
// than leaving a bridge that answers nothing.
func TestStartTimesOut(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.InitTimeout = 50 * time.Millisecond

	m := metrics.New("test")
	b := newTestBridge(t, cfg, &blockingProvider{}, m, nil)

	err := b.Start(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error: got %v, want it to wrap context.DeadlineExceeded", err)
	}
	if got := b.State(); got != StateStarting {
		t.Errorf("state: got %s, want %s", got, StateStarting)
	}
	if m.Snapshot().LastError == nil {
		t.Error("the initialization failure was not recorded as the last error")
	}
}

// TestShutdownDuringStartupStaysUnready states that a termination signal during
// startup wins over a late readiness.
func TestShutdownDuringStartupStaysUnready(t *testing.T) {
	t.Parallel()

	b := newTestBridge(t, testConfig(), memprovider.NewInMemoryProvider(nil), nil, nil)
	b.BeginShutdown()

	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := b.State(); got != StateShuttingDown {
		t.Errorf("state: got %s, want %s", got, StateShuttingDown)
	}
	if !b.Status().ReadyAt.IsZero() {
		t.Error("ReadyAt must stay unset when the process is shutting down")
	}
}

// TestEvaluateGoesThroughTheClient states that an evaluation fires the
// provider's hooks, which is what carries exposure events and evaluation counts
// to Datadog.
func TestEvaluateGoesThroughTheClient(t *testing.T) {
	t.Parallel()

	provider := &hookedProvider{}
	b := newTestBridge(t, testConfig(), provider, nil, nil)
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	details, err := b.Evaluate(context.Background(), "flag", openfeature.NewEvaluationContext("alice", nil))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if details.Value != "value" {
		t.Errorf("value: got %#v, want value", details.Value)
	}
	if provider.hookCalls == 0 {
		t.Error("the provider hook did not fire")
	}
}

// TestShutdownStopsTheTracer states that the shutdown sequence stops the tracer
// after the provider has flushed.
func TestShutdownStopsTheTracer(t *testing.T) {
	t.Parallel()

	b := newTestBridge(t, testConfig(), memprovider.NewInMemoryProvider(nil), nil, nil)
	stopped := false
	b.stopTracer = func() { stopped = true }

	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := b.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !stopped {
		t.Error("the tracer was not stopped")
	}
	if got := b.State(); got != StateShuttingDown {
		t.Errorf("state: got %s, want %s", got, StateShuttingDown)
	}
}

// TestApplyTracerDefaults states that ddflagd sets the tracer settings it wants
// but never overrides an explicit choice.
func TestApplyTracerDefaults(t *testing.T) {
	// t.Setenv is used, so this test cannot run in parallel.
	t.Run("unset settings are defaulted", func(t *testing.T) {
		// t.Setenv registers the restore, then the variable is removed so
		// that the defaulting path is the one under test.
		t.Setenv(EnvAPMTracingEnabled, "")
		t.Setenv(EnvTraceStartupLogs, "")
		if err := os.Unsetenv(EnvAPMTracingEnabled); err != nil {
			t.Fatal(err)
		}
		if err := os.Unsetenv(EnvTraceStartupLogs); err != nil {
			t.Fatal(err)
		}

		applyTracerDefaults()

		if got := os.Getenv(EnvAPMTracingEnabled); got != "false" {
			t.Errorf("%s: got %q, want false", EnvAPMTracingEnabled, got)
		}
		if got := os.Getenv(EnvTraceStartupLogs); got != "false" {
			t.Errorf("%s: got %q, want false", EnvTraceStartupLogs, got)
		}
	})

	t.Run("an explicit setting is kept", func(t *testing.T) {
		t.Setenv(EnvAPMTracingEnabled, "true")
		t.Setenv(EnvTraceStartupLogs, "true")

		applyTracerDefaults()

		if got := os.Getenv(EnvAPMTracingEnabled); got != "true" {
			t.Errorf("%s: got %q, want true", EnvAPMTracingEnabled, got)
		}
	})
}

// helpers

func testConfig() *Config {
	return &Config{
		Service:         "rust-service",
		Env:             "test",
		ListenAddr:      DefaultListenAddr,
		AdminAddr:       DefaultAdminAddr,
		InitTimeout:     5 * time.Second,
		ShutdownTimeout: DefaultShutdownTimeout,
		HandlerTimeout:  DefaultHandlerTimeout,
	}
}

func newTestBridge(t *testing.T, cfg *Config, provider openfeature.FeatureProvider, m *metrics.Metrics, now func() time.Time) *Bridge {
	t.Helper()

	b, err := New(context.Background(), Options{
		Config:      cfg,
		Metrics:     m,
		Now:         now,
		NewProvider: func(context.Context) (openfeature.FeatureProvider, error) { return provider, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := b.api.Shutdown(context.Background()); err != nil {
			t.Errorf("shutting down the API: %v", err)
		}
	})
	return b
}

// blockingProvider never finishes initializing, which is what a bridge that
// cannot reach the Agent looks like.
type blockingProvider struct {
	openfeature.NoopProvider
}

func (p *blockingProvider) Metadata() openfeature.Metadata {
	return openfeature.Metadata{Name: "blocking"}
}

func (p *blockingProvider) Init(openfeature.EvaluationContext) error { return nil }

func (p *blockingProvider) Shutdown() {}

func (p *blockingProvider) InitWithContext(ctx context.Context, _ openfeature.EvaluationContext) error {
	<-ctx.Done()
	return ctx.Err()
}

func (p *blockingProvider) ShutdownWithContext(context.Context) error { return nil }

// hookedProvider records whether its hook fired.
type hookedProvider struct {
	openfeature.NoopProvider
	hookCalls int
}

func (p *hookedProvider) Metadata() openfeature.Metadata {
	return openfeature.Metadata{Name: "hooked"}
}

func (p *hookedProvider) ObjectEvaluation(_ context.Context, _ string, _ any, _ openfeature.FlattenedContext) openfeature.InterfaceResolutionDetail {
	return openfeature.InterfaceResolutionDetail{
		Value:  "value",
		Reason: openfeature.StaticReason,
	}
}

func (p *hookedProvider) Hooks() []openfeature.Hook {
	return []openfeature.Hook{&recordingHook{provider: p}}
}

type recordingHook struct {
	openfeature.UnimplementedHook
	provider *hookedProvider
}

func (h *recordingHook) After(context.Context, openfeature.HookContext, openfeature.InterfaceEvaluationDetails, openfeature.HookHints) error {
	h.provider.hookCalls++
	return nil
}
