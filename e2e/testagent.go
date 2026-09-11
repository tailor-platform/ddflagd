//go:build e2e

package e2e

import (
	"context"
	"fmt"
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

// StartTestAgent brings up the fake Agent and returns a client for it, along
// with the function that stops it.
//
// The container is started from the suite rather than from a compose file, so
// that `go test -tags e2e ./e2e/...` is the whole command: no setup step to
// forget, no fixed host port to collide with, and nothing left running when
// the tests are over. Setting DDFLAGD_TEST_AGENT_URL skips the container and
// uses the Agent at that address, which keeps a local edit loop off the
// container start altogether.
func StartTestAgent(ctx context.Context) (*TestAgent, func(), error) {
	if url := os.Getenv(envTestAgentURL); url != "" {
		agent := NewTestAgent(url)
		if err := agent.WaitReady(ctx); err != nil {
			return nil, nil, fmt.Errorf("the fake Agent at %s (%s) is not answering: %w", url, envTestAgentURL, err)
		}
		return agent, func() {}, nil
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
	stop := func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			fmt.Fprintf(os.Stderr, "terminating the fake Agent: %v\n", err)
		}
	}
	if err != nil {
		stop()
		return nil, nil, fmt.Errorf("starting the fake Agent: %w", err)
	}

	endpoint, err := container.PortEndpoint(ctx, agentPort, "http")
	if err != nil {
		stop()
		return nil, nil, fmt.Errorf("resolving the fake Agent's address: %w", err)
	}
	return NewTestAgent(endpoint), stop, nil
}
