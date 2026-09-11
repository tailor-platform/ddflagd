/*
Copyright © 2026 Ken'ichiro Oyama <k1lowxb@gmail.com>

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
*/
package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/k1LoW/donegroup"
	"github.com/spf13/cobra"

	"github.com/k1LoW/ddflagd/internal/admin"
	"github.com/k1LoW/ddflagd/internal/auth"
	"github.com/k1LoW/ddflagd/internal/bridge"
	"github.com/k1LoW/ddflagd/internal/metrics"
	"github.com/k1LoW/ddflagd/internal/ofrep"
	"github.com/k1LoW/ddflagd/version"
)

// readHeaderTimeout bounds how long a client may take to send its request
// headers. It is short because every caller is either a sidecar on loopback or
// a Pod in the same cluster.
const readHeaderTimeout = 5 * time.Second

// newRootCmd builds the root command.
//
// A command is built per call rather than kept in a package variable, because
// cobra stores parsed flag values on the command itself: a second Execute on
// the same instance would inherit the first one's flags.
func newRootCmd() *cobra.Command {
	return &cobra.Command{
		Use:   version.Name,
		Short: "ddflagd serves Datadog Feature Flags over the OpenFeature Remote Evaluation Protocol",
		Long: `ddflagd serves Datadog Feature Flags over OFREP, so that a language without a
Datadog SDK can evaluate Datadog flags through the OpenFeature API.

It holds Datadog's official Go SDK: the flag configuration, the evaluation and
the telemetry all stay inside that SDK, and ddflagd converts between it and the
protocol. A caller uses its language's OFREP provider unchanged.

  ddflagd                    Run the bridge, configured by the environment

Two listeners come up. The evaluation listener answers
POST /ofrep/v1/evaluate/flags/{key} and is bound to loopback by default, so in
a sidecar nothing outside the Pod can evaluate flags. The operational listener
answers /healthz, /readyz, /metrics and /debug/status, and is bound to the Pod
IP, because that is where a kubelet probe arrives.

Configuration is environment only, and deliberately so: the DD_ variables are
read by the official SDK itself, and a flag alongside them would make the
effective configuration depend on which of the two won.

  DD_EXPERIMENTAL_FLAGGING_PROVIDER_ENABLED  Required, "true".
  DD_SERVICE                                 Required. The consuming service's
                                             name, which the exposure events and
                                             the evaluation metrics are recorded
                                             under.
  DD_ENV, DD_VERSION                         The consuming service's environment
                                             and version.
  DD_TRACE_AGENT_URL                         The Agent. A Unix socket looks like
                                             unix:///var/run/datadog/apm.socket.
  DDFLAGD_LISTEN_ADDR                        The evaluation listener.
  DDFLAGD_ADMIN_ADDR                         The operational listener.

The README documents the rest, along with what a caller is asked to do.`,
		Args:    cobra.NoArgs,
		RunE:    run,
		Version: version.Version,
		// A configuration or startup failure is not a usage error, and the
		// error is reported as a structured log line rather than by cobra.
		SilenceUsage:  true,
		SilenceErrors: true,
	}
}

// Execute runs the root command.
func Execute() {
	if err := newRootCmd().Execute(); err != nil {
		slog.Error("ddflagd exited", slog.Any("error", err))
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, _ []string) error {
	cfg, err := bridge.LoadConfig()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	m := metrics.New(version.Version)
	logger := slog.Default().With(
		slog.String("service", cfg.Service),
		slog.String("version", version.Version),
	)
	logger.Info("starting ddflagd",
		slog.String("revision", version.Revision),
		slog.String("dd_trace_go_version", metrics.DDTraceGoVersion()),
		slog.String("env", cfg.Env),
		slog.String("agent_url", cfg.AgentURL),
		slog.String("listen_addr", cfg.ListenAddr),
		slog.String("admin_addr", cfg.AdminAddr),
	)

	// Three things end this process: a termination signal, a listener that
	// cannot serve, and a provider that never receives a flag configuration.
	// They all become the cancellation of one context, so that the rest of the
	// function has a single thing to wait on.
	//
	// The signal context is installed before the provider is created, so that a
	// SIGTERM during a slow provider startup is not lost.
	signalCtx, stopSignals := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	ctx, cancel := donegroup.WithCancelCause(signalCtx)
	defer cancel(nil)

	b, err := bridge.New(ctx, bridge.Options{Config: cfg, Logger: logger, Metrics: m})
	if err != nil {
		return err
	}

	ofrepHandler, err := ofrep.NewHandler(ofrep.Options{
		Evaluator: b,
		Metrics:   m,
		Logger:    logger,
		Timeout:   cfg.HandlerTimeout,
	})
	if err != nil {
		return err
	}
	adminHandler, err := admin.NewHandler(admin.Options{
		Status:       b,
		Metrics:      m,
		Logger:       logger,
		Revision:     version.Revision,
		PprofEnabled: cfg.PprofEnabled,
	})
	if err != nil {
		return err
	}

	evalServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           auth.APIKey(cfg.APIKey, ofrepHandler),
		ReadHeaderTimeout: readHeaderTimeout,
	}
	adminServer := &http.Server{
		Addr:              cfg.AdminAddr,
		Handler:           adminHandler,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	// The termination sequence is the context's cleanup, which is what it is:
	// the work that has to happen once the process is on its way out, whichever
	// of the three reasons ended it.
	if err := donegroup.Cleanup(ctx, func() error {
		return shutdown(ctx, logger, cfg, b, ofrepHandler, evalServer, adminServer)
	}); err != nil {
		return err
	}

	// Both listeners come up before the first flag configuration arrives, so
	// that the probes can answer while the provider is still starting.
	for _, s := range []*http.Server{adminServer, evalServer} {
		donegroup.Go(ctx, func() error {
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				err = fmt.Errorf("listening on %s: %w", s.Addr, err)
				cancel(err)
				return err
			}
			return nil
		})
	}

	donegroup.Go(ctx, func() error {
		if err := b.Start(ctx); err != nil {
			// A startup cut short by the process already stopping is not a
			// failure. Only the root context's state tells the two apart: a
			// signal cancels it, while an Agent that never delivered a
			// configuration trips DDFLAGD_INIT_TIMEOUT on a context of Start's
			// own and leaves this one alive.
			if ctx.Err() != nil {
				return nil
			}
			// Exiting lets Kubernetes restart the container, which is the only
			// thing that can recover from an Agent that never delivered a
			// configuration.
			cancel(err)
			return err
		}
		return nil
	})

	// One place to wait. It returns once the context has ended, for whichever
	// of the three reasons, and the termination sequence and both listeners
	// have finished, carrying whatever any of them reported.
	//
	// Nothing wraps a deadline around it. The sequence bounds every step it
	// takes with DDFLAGD_SHUTDOWN_TIMEOUT, and a listener returns as soon as
	// Shutdown closes it rather than when Shutdown finishes, so an outer
	// deadline would only ever fire on a hang it could not name. The Pod's
	// terminationGracePeriodSeconds is the backstop for that.
	return donegroup.Wait(ctx)
}

// shutdown runs the termination sequence.
//
// Readiness drops first and the evaluation listener stays open for the drain
// delay, because a Deployment keeps receiving requests until the Service
// endpoint update has propagated. The provider is shut down only after the
// listener has drained, since that shutdown is what flushes the last exposure
// events and evaluation counts.
func shutdown(ctx context.Context, logger *slog.Logger, cfg *bridge.Config, b *bridge.Bridge, ofrepHandler *ofrep.Handler, evalServer, adminServer *http.Server) error {
	b.BeginShutdown()
	logger.Info("draining",
		slog.String("cause", shutdownCause(ctx)),
		slog.String("drain_delay", cfg.DrainDelay.String()))

	// The drain delay is not part of the shutdown budget: it is time spent
	// answering requests normally, not time spent stopping.
	if cfg.DrainDelay > 0 {
		time.Sleep(cfg.DrainDelay)
	}
	ofrepHandler.StopServing()

	// The sequence runs on its own deadline rather than on the context that
	// just ended, which is already canceled.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
	defer cancel()

	var errs []error
	if err := evalServer.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("draining the evaluation listener: %w", err))
	}
	if err := b.Shutdown(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := adminServer.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("stopping the operational listener: %w", err))
	}
	logger.Info("stopped")
	return errors.Join(errs...)
}

// shutdownCause names why the process is stopping. A signal leaves the plain
// cancellation behind, so anything else is a failure worth naming in the log.
func shutdownCause(ctx context.Context) string {
	cause := context.Cause(ctx)
	if cause == nil || errors.Is(cause, context.Canceled) {
		return "a termination signal"
	}
	return cause.Error()
}
