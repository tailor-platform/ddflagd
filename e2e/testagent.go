//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// testAgentImage is Datadog's fake Agent, the one every tracer's CI runs
// against. It is pinned so that a change on their side shows up as a
// deliberate bump here rather than as a suite that behaves differently from
// one day to the next.
const testAgentImage = "ghcr.io/datadog/dd-apm-test-agent/ddapm-test-agent:v1.65.0"

// agentPort is the port the fake Agent serves on inside the container. The
// host port is whatever Docker assigns, so nothing collides with an Agent the
// developer already runs.
const agentPort = "8126/tcp"

// envTestAgentURL points the suite at an already running fake Agent instead of
// starting one.
const envTestAgentURL = "DDFLAGD_TEST_AGENT_URL"

// teardownTimeout bounds reading the logs and stopping the container.
const teardownTimeout = time.Minute

// StartTestAgent brings up the fake Agent and returns a client for it, along
// with the function that stops it. That function takes whether the suite
// failed, reports whether the container could be removed, and prints the
// container's logs before termination when the suite failed: a
// Remote Configuration or trace ingestion problem shows up on the Agent's side,
// not on the bridge's, and nothing else in the suite would report it.
//
// The container is started from the suite rather than from a compose file, so
// that `go test -tags e2e ./e2e/...` is the whole command: no setup step to
// forget, no fixed host port to collide with, and nothing left running when
// the tests are over. Setting DDFLAGD_TEST_AGENT_URL skips the container and
// uses the Agent at that address, which keeps a local edit loop off the
// container start altogether.
func StartTestAgent(ctx context.Context) (*TestAgent, func(failed bool) error, error) {
	if url := os.Getenv(envTestAgentURL); url != "" {
		agent := NewTestAgent(url)
		if err := agent.WaitReady(ctx); err != nil {
			return nil, nil, fmt.Errorf("the fake Agent at %s (%s) is not answering: %w", url, envTestAgentURL, err)
		}
		// The Agent belongs to whoever started it, so the suite neither stops
		// it nor reads its logs.
		return agent, func(bool) error { return nil }, nil
	}

	container, err := testcontainers.Run(ctx, testAgentImage,
		testcontainers.WithExposedPorts(agentPort),
		testcontainers.WithEnv(map[string]string{
			"LOG_LEVEL": "INFO",
			// The suite asserts on what the bridge sent, not on Datadog's own
			// trace snapshots, so the snapshot checks only add noise.
			"SNAPSHOT_CI":                    "0",
			"ENABLED_CHECKS":                 "",
			"DD_SUPPRESS_TRACE_PARSE_ERRORS": "true",
			"DD_POOL_TRACE_CHECK_FAILURES":   "true",
			"DD_DISABLE_ERROR_RESPONSES":     "true",
		}),
		// /info is what the tracer itself probes to decide whether Remote
		// Configuration is available, so it is the right readiness signal.
		testcontainers.WithWaitStrategy(
			wait.ForHTTP("/info").
				WithPort(agentPort).
				WithStartupTimeout(2*time.Minute).
				WithStatusCodeMatcher(func(status int) bool { return status == http.StatusOK }),
		),
	)
	stop := func(failed bool) error {
		// Teardown runs on a budget of its own. The context handed in bounds
		// the suite's setup, and by the time anything stops it has usually
		// expired.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), teardownTimeout)
		defer cancel()

		// The nil check belongs here, where the static type is still a
		// pointer: Run returns a live container alongside an error when the
		// wait strategy gives up, and a nil one when the request itself was
		// rejected.
		if failed && container != nil {
			printAgentLogs(stopCtx, container)
		}
		// StopContext is what puts termination on the same budget. Without it
		// TerminateContainer uses a background context of its own, and a
		// stalled Docker daemon would hold the suite open indefinitely.
		if err := testcontainers.TerminateContainer(container, testcontainers.StopContext(stopCtx)); err != nil {
			return fmt.Errorf("terminating the fake Agent: %w", err)
		}
		return nil
	}
	if err != nil {
		// A container that never became ready is exactly the case its logs
		// explain, so they are printed even though no test has run yet.
		if stopErr := stop(true); stopErr != nil {
			fmt.Fprintf(os.Stderr, "%v\n", stopErr)
		}
		return nil, nil, fmt.Errorf("starting the fake Agent: %w", err)
	}

	endpoint, err := container.PortEndpoint(ctx, agentPort, "http")
	if err != nil {
		if stopErr := stop(true); stopErr != nil {
			fmt.Fprintf(os.Stderr, "%v\n", stopErr)
		}
		return nil, nil, fmt.Errorf("resolving the fake Agent's address: %w", err)
	}
	return NewTestAgent(endpoint), stop, nil
}

// printAgentLogs writes the container's logs to stderr, where `go test` shows
// them alongside the failure that prompted them.
func printAgentLogs(ctx context.Context, container testcontainers.Container) {
	logs, err := container.Logs(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reading the fake Agent's logs: %v\n", err)
		return
	}
	defer func() {
		if err := logs.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "closing the fake Agent's logs: %v\n", err)
		}
	}()

	fmt.Fprintf(os.Stderr, "--- %s logs ---\n", testAgentImage)
	if _, err := io.Copy(os.Stderr, logs); err != nil {
		fmt.Fprintf(os.Stderr, "reading the fake Agent's logs: %v\n", err)
	}
	fmt.Fprintln(os.Stderr, "--- end of logs ---")
}
