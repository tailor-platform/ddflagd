// Package bridge starts the official Datadog OpenFeature provider, keeps the
// lifecycle state of the process, and evaluates flags through an OpenFeature
// client.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	ddopenfeature "github.com/DataDog/dd-trace-go/v2/openfeature"
	"github.com/open-feature/go-sdk/openfeature"
	"github.com/open-feature/go-sdk/openfeature/isolated"

	"github.com/tailor-platform/ddflagd/internal/metrics"
)

// State is the lifecycle state of the bridge.
type State string

// States of the bridge.
const (
	// StateStarting means the official provider has not received a flag
	// configuration yet.
	StateStarting State = "starting"
	// StateReady means the official provider holds a flag configuration.
	StateReady State = "ready"
	// StateShuttingDown means the process received SIGTERM.
	StateShuttingDown State = "shutting_down"
)

// ErrProviderNotDatadog is returned when the official constructor hands back
// something other than a Datadog provider, which is how it reports that the
// experimental flagging provider was not enabled.
var ErrProviderNotDatadog = errors.New("the official constructor did not return a Datadog provider")

// ProviderFunc creates the OpenFeature provider the bridge evaluates through.
type ProviderFunc func(context.Context) (openfeature.FeatureProvider, error)

// Options configures New.
type Options struct {
	Config  *Config
	Logger  *slog.Logger
	Metrics *metrics.Metrics

	// NewProvider creates the provider. When nil, the official Datadog
	// provider is created after starting the tracer.
	NewProvider ProviderFunc

	// Now is the clock used for the state timestamps. When nil, time.Now.
	Now func() time.Time
}

// Bridge owns the provider and the OpenFeature client used for evaluation.
type Bridge struct {
	cfg     *Config
	logger  *slog.Logger
	metrics *metrics.Metrics
	now     func() time.Time

	// api is an isolated OpenFeature API rather than the package level
	// singleton, so that a test (or a future multi provider layout) can run
	// several bridges in one process without them overwriting each other's
	// provider registration.
	api      *openfeature.EvaluationAPI
	client   *openfeature.Client
	provider openfeature.FeatureProvider

	startedAt time.Time

	mu      sync.RWMutex
	state   State
	readyAt time.Time

	stopTracer func()
}

// New creates the provider and the OpenFeature client. It does not wait for the
// first flag configuration; call Start for that.
func New(ctx context.Context, opts Options) (*Bridge, error) {
	if opts.Config == nil {
		return nil, errors.New("bridge: Config is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	m := opts.Metrics
	if m == nil {
		m = metrics.New("")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	newProvider := opts.NewProvider
	stopTracer := func() {}
	if newProvider == nil {
		newProvider = NewDatadogProvider
		stopTracer = StopTracer
	}

	provider, err := newProvider(ctx)
	if err != nil {
		stopTracer()
		return nil, err
	}
	if err := rejectNoopProvider(provider); err != nil {
		stopTracer()
		return nil, err
	}

	b := &Bridge{
		cfg:        opts.Config,
		logger:     logger,
		metrics:    m,
		now:        now,
		api:        isolated.NewAPI(),
		provider:   provider,
		startedAt:  now(),
		state:      StateStarting,
		stopTracer: stopTracer,
	}
	b.client = b.api.NewClient()
	return b, nil
}

// Start registers the provider and waits until it holds a flag configuration.
// It returns an error when the wait exceeds the configured init timeout.
func (b *Bridge) Start(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.InitTimeout)
	defer cancel()

	if err := b.api.SetProviderAndWait(ctx, b.provider); err != nil {
		b.metrics.RecordError(fmt.Sprintf("provider initialization failed: %v", err))
		return fmt.Errorf("waiting for the first flag configuration (%s=%s): %w", EnvInitTimeout, b.cfg.InitTimeout, err)
	}

	at := b.now()
	b.mu.Lock()
	// A SIGTERM during initialization wins: the process is on its way out and
	// must not advertise readiness.
	if b.state == StateStarting {
		b.state = StateReady
		b.readyAt = at
	}
	state := b.state
	b.mu.Unlock()

	if state == StateReady {
		b.metrics.SetProviderReady(at)
		b.logger.Info("provider is ready", slog.String("service", b.cfg.Service), slog.String("state", string(state)))
	}
	return nil
}

// Evaluate evaluates one flag through the OpenFeature client.
//
// The evaluation goes through the client, not through the provider, because the
// exposure and evaluation count hooks live in the client's evaluation pipeline.
// It asks for an object with a nil default so that the value's type never has to
// be known here; a reason of DEFAULT or DISABLED then surfaces as a nil value,
// which is exactly the OFREP code default response.
func (b *Bridge) Evaluate(ctx context.Context, key string, evalCtx openfeature.EvaluationContext) (openfeature.InterfaceEvaluationDetails, error) {
	return b.client.ObjectValueDetails(ctx, key, nil, evalCtx)
}

// State reports the lifecycle state.
func (b *Bridge) State() State {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.state
}

// Status is the state the operational listener reports.
type Status struct {
	State     State
	StartedAt time.Time
	ReadyAt   time.Time
	Config    *Config
}

// Status reports the current state for /readyz and /debug/status.
func (b *Bridge) Status() Status {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return Status{
		State:     b.state,
		StartedAt: b.startedAt,
		ReadyAt:   b.readyAt,
		Config:    b.cfg,
	}
}

// BeginShutdown moves the bridge into the shutting down state so that /readyz
// starts failing while evaluation keeps working during the drain delay.
func (b *Bridge) BeginShutdown() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = StateShuttingDown
}

// Shutdown flushes the provider and stops the tracer. The provider shutdown is
// what flushes the last exposure events and evaluation counts, so it must
// happen after the evaluation listener has drained.
func (b *Bridge) Shutdown(ctx context.Context) error {
	b.BeginShutdown()
	err := b.api.Shutdown(ctx)
	b.stopTracer()
	if err != nil {
		return fmt.Errorf("shutting down the provider: %w", err)
	}
	return nil
}

// NewDatadogProvider starts the tracer and creates the official Datadog
// provider.
//
// The tracer has to start first: the provider's own Remote Configuration client
// leaves the Agent URL unset, so only the tracer's configuration gives the
// Remote Configuration client a reachable endpoint. This is also the form
// Datadog supports.
func NewDatadogProvider(context.Context) (openfeature.FeatureProvider, error) {
	applyTracerDefaults()

	if err := startTracer(); err != nil {
		return nil, fmt.Errorf("starting the tracer: %w", err)
	}
	provider, err := ddopenfeature.NewDatadogProvider(ddopenfeature.ProviderConfig{})
	if err != nil {
		return nil, fmt.Errorf("creating the Datadog provider: %w", err)
	}
	if err := checkDatadogProvider(provider); err != nil {
		return nil, err
	}
	return provider, nil
}

// checkDatadogProvider verifies that the official constructor returned a real
// Datadog provider. It reports a NoopProvider as an error because that is how
// the constructor signals a missing
// DD_EXPERIMENTAL_FLAGGING_PROVIDER_ENABLED=true, and a bridge that silently
// evaluates nothing is worse than one that fails to start.
func checkDatadogProvider(provider openfeature.FeatureProvider) error {
	if _, ok := provider.(*ddopenfeature.DatadogProvider); ok {
		return nil
	}
	return fmt.Errorf("%w: got %T, check %s=true", ErrProviderNotDatadog, provider, EnvFlaggingProviderEnabled)
}

// rejectNoopProvider refuses a provider that evaluates nothing, whichever
// constructor produced it.
func rejectNoopProvider(provider openfeature.FeatureProvider) error {
	switch provider.(type) {
	case nil:
		return errors.New("bridge: the provider constructor returned nil")
	case openfeature.NoopProvider, *openfeature.NoopProvider:
		return fmt.Errorf("%w: got a NoopProvider, check %s=true", ErrProviderNotDatadog, EnvFlaggingProviderEnabled)
	}
	return nil
}

// applyTracerDefaults sets the tracer settings ddflagd wants without taking
// them away from an operator who set them explicitly.
//
// DD_APM_TRACING_ENABLED=false is the setting Datadog recommends when the
// tracer only carries another product's data: traces get tagged so the backend
// does not turn APM on, trace submission is rate limited, and runtime metrics
// are off. ddflagd creates no spans, so this is a guard rather than a
// behavioral change.
func applyTracerDefaults() {
	for name, value := range map[string]string{
		EnvAPMTracingEnabled: "false",
		EnvTraceStartupLogs:  "false",
	} {
		if _, ok := os.LookupEnv(name); ok {
			continue
		}
		if err := os.Setenv(name, value); err != nil {
			// The tracer reads the environment itself, so a failure here only
			// means the default was not applied.
			slog.Warn("could not set a tracer default", slog.String("name", name), slog.Any("error", err))
		}
	}
}
