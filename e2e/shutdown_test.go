//go:build e2e

package e2e

import (
	"context"
	"syscall"
	"testing"
	"time"
)

// TestShutdownSequence states the order of the termination sequence.
//
// Readiness has to fail before the evaluation listener closes, because a
// Deployment keeps receiving requests until the Service endpoint removal has
// propagated. Evaluations therefore have to keep working during the drain
// delay, and the last exposure events have to reach the Agent before the
// process exits.
func TestShutdownSequence(t *testing.T) {
	requireSetup(t)

	const (
		targetingKey = "e2e-shutdown-subject"
		drainDelay   = 3 * time.Second
	)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	victim, err := StartBridge(ctx, BridgeConfig{
		Binary:   bridge.cmd.Path,
		AgentURL: proxy.URL(),
		Service:  service,
		Env:      environment,
		Version:  serviceVersion,
		Extra: map[string]string{
			"DDFLAGD_DRAIN_DELAY": drainDelay.String(),
			// Long enough that the exposure flush interval fits inside it.
			"DDFLAGD_SHUTDOWN_TIMEOUT": "10s",
		},
	})
	if err != nil {
		t.Fatalf("starting a bridge to terminate: %v", err)
	}
	if err := victim.WaitReady(ctx); err != nil {
		t.Fatalf("waiting for the bridge: %v", err)
	}

	if err := victim.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("sending SIGTERM: %v", err)
	}

	// Readiness must drop straight away.
	var sawUnready bool
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		code, body, err := victim.Get(ctx, "/readyz")
		if err != nil {
			t.Fatalf("fetching readiness during the drain: %v", err)
		}
		if code == 503 {
			if body["state"] != "shutting_down" {
				t.Errorf("state during the drain: got %#v, want shutting_down", body["state"])
			}
			sawUnready = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !sawUnready {
		t.Fatalf("readiness did not drop within 2s of SIGTERM\n%s", victim.Logs())
	}

	// Evaluation has to keep working while readiness is already failing.
	code, body, err := victim.Evaluate(ctx, "numeric_flag", map[string]any{"targetingKey": targetingKey})
	if err != nil {
		t.Fatalf("evaluating during the drain: %v\n%s", err, victim.Logs())
	}
	if code != 200 {
		t.Fatalf("evaluating during the drain: got %d, want 200 (%v)", code, body)
	}
	if body["value"] == nil {
		t.Errorf("the evaluation during the drain carries no value: %v", body)
	}

	// The process must then exit on its own, within the drain delay plus the
	// shutdown budget.
	if err := victim.Terminate(30 * time.Second); err != nil {
		t.Fatalf("the bridge did not shut down cleanly: %v\n%s", err, victim.Logs())
	}

	// The exposure of the evaluation made during the drain has to have reached
	// the Agent, which is what the provider shutdown flushes.
	found := false
	for _, payload := range proxy.Exposures() {
		for _, exposure := range payload.Exposures {
			if exposure.Subject.ID == targetingKey {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("the exposure of the evaluation made during the drain never reached the Agent\n%s", victim.Logs())
	}
}
