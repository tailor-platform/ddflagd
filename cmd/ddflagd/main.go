// Command ddflagd serves the OpenFeature Remote Evaluation Protocol backed by
// the official Datadog Feature Flags provider.
package main

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

	"github.com/k1LoW/ddflagd/internal/admin"
	"github.com/k1LoW/ddflagd/internal/auth"
	"github.com/k1LoW/ddflagd/internal/bridge"
	"github.com/k1LoW/ddflagd/internal/metrics"
	"github.com/k1LoW/ddflagd/internal/ofrep"
	"github.com/k1LoW/ddflagd/version"
)

// commit and date are set at build time.
var (
	commit = "none"
	date   = "unknown"
)

// readHeaderTimeout bounds how long a client may take to send its request
// headers. It is short because every caller is either a sidecar on loopback or
// a Pod in the same cluster.
const readHeaderTimeout = 5 * time.Second

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("ddflagd exited", slog.Any("error", err))
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := bridge.LoadConfig()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	m := metrics.New(version.Version)
	logger = logger.With(
		slog.String("service", cfg.Service),
		slog.String("version", version.Version),
	)
	logger.Info("starting ddflagd",
		slog.String("commit", commit),
		slog.String("date", date),
		slog.String("dd_trace_go_version", metrics.DDTraceGoVersion()),
		slog.String("env", cfg.Env),
		slog.String("agent_url", cfg.AgentURL),
		slog.String("listen_addr", cfg.ListenAddr),
		slog.String("admin_addr", cfg.AdminAddr),
	)

	// The signal context is installed before the provider is created so that a
	// SIGTERM during a slow provider startup is not lost.
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	b, err := bridge.New(signalCtx, bridge.Options{Config: cfg, Logger: logger, Metrics: m})
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
		Revision:     commit,
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

	// Both listeners come up before the first flag configuration arrives, so
	// that the probes can answer while the provider is still starting.
	serveErrs := make(chan error, 2)
	for _, s := range []*http.Server{adminServer, evalServer} {
		go func(s *http.Server) {
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErrs <- fmt.Errorf("listening on %s: %w", s.Addr, err)
			}
		}(s)
	}

	initErrs := make(chan error, 1)
	go func() {
		initErrs <- b.Start(signalCtx)
	}()

	var runErr error
	select {
	case err := <-serveErrs:
		runErr = err
	case err := <-initErrs:
		if err != nil {
			// Exiting lets Kubernetes restart the container, which is the only
			// thing that can recover from an Agent that never delivered a
			// configuration.
			runErr = err
		} else {
			select {
			case err := <-serveErrs:
				runErr = err
			case <-signalCtx.Done():
				logger.Info("received a termination signal")
			}
		}
	case <-signalCtx.Done():
		logger.Info("received a termination signal during startup")
	}

	shutdownErr := shutdown(logger, cfg, b, ofrepHandler, evalServer, adminServer)
	return errors.Join(runErr, shutdownErr)
}

// shutdown runs the termination sequence.
//
// Readiness drops first and the evaluation listener stays open for the drain
// delay, because a Deployment keeps receiving requests until the Service
// endpoint update has propagated. The provider is shut down only after the
// listener has drained, since that shutdown is what flushes the last exposure
// events and evaluation counts.
func shutdown(logger *slog.Logger, cfg *bridge.Config, b *bridge.Bridge, ofrepHandler *ofrep.Handler, evalServer, adminServer *http.Server) error {
	b.BeginShutdown()
	logger.Info("draining", slog.String("drain_delay", cfg.DrainDelay.String()))

	// The drain delay is not part of the shutdown budget: it is time spent
	// answering requests normally, not time spent stopping.
	if cfg.DrainDelay > 0 {
		time.Sleep(cfg.DrainDelay)
	}
	ofrepHandler.StopServing()

	ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
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
