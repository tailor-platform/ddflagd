//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// BridgeConfig configures a bridge process the suite starts.
type BridgeConfig struct {
	Binary   string
	AgentURL string
	Service  string
	Env      string
	Version  string
	// Extra holds additional environment variables, which is how a test
	// exercises a setting such as the drain delay.
	Extra map[string]string
}

// stopGrace is how long Stop waits at each step before escalating.
const stopGrace = 5 * time.Second

// Bridge is a running ddflagd process.
type Bridge struct {
	cmd       *exec.Cmd
	evalAddr  string
	adminAddr string
	client    *http.Client
	stdout    *bytes.Buffer
	stderr    *bytes.Buffer

	exited chan error

	mu      sync.Mutex
	reaped  bool
	exitErr error
}

// BuildBridge compiles the ddflagd binary into dir and returns its path.
func BuildBridge(ctx context.Context, dir string) (string, error) {
	binary := filepath.Join(dir, "ddflagd")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", binary, "..")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("building ddflagd: %w: %s", err, out)
	}
	return binary, nil
}

// StartBridge starts a bridge process with both listeners on free loopback
// ports.
func StartBridge(ctx context.Context, cfg BridgeConfig) (*Bridge, error) {
	evalAddr, err := freeLoopbackAddr()
	if err != nil {
		return nil, err
	}
	adminAddr, err := freeLoopbackAddr()
	if err != nil {
		return nil, err
	}

	env := map[string]string{
		"DD_EXPERIMENTAL_FLAGGING_PROVIDER_ENABLED": "true",
		"DD_SERVICE":                             cfg.Service,
		"DD_ENV":                                 cfg.Env,
		"DD_VERSION":                             cfg.Version,
		"DD_TRACE_AGENT_URL":                     cfg.AgentURL,
		"DD_REMOTE_CONFIG_POLL_INTERVAL_SECONDS": "1",
		"DD_TRACE_STARTUP_LOGS":                  "false",
		"DD_INSTRUMENTATION_TELEMETRY_ENABLED":   "false",
		"DDFLAGD_LISTEN_ADDR":                    evalAddr,
		"DDFLAGD_ADMIN_ADDR":                     adminAddr,
		"DDFLAGD_DRAIN_DELAY":                    "0s",
		"DDFLAGD_INIT_TIMEOUT":                   "60s",
	}
	maps.Copy(env, cfg.Extra)

	// The child gets an explicit environment so that a DD_ variable in the
	// developer's shell cannot change the outcome.
	environ := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	for name, value := range env {
		environ = append(environ, name+"="+value)
	}

	b := &Bridge{
		evalAddr:  evalAddr,
		adminAddr: adminAddr,
		client:    &http.Client{Timeout: 10 * time.Second},
		stdout:    &bytes.Buffer{},
		stderr:    &bytes.Buffer{},
		exited:    make(chan error, 1),
	}

	// exec.Command, not CommandContext: the shutdown sequence is under test, so
	// the process is stopped with SIGTERM rather than killed.
	// The binary is the one this suite compiled.
	b.cmd = exec.Command(cfg.Binary) //nolint:gosec // G204
	b.cmd.Env = environ
	b.cmd.Stdout = b.stdout
	b.cmd.Stderr = b.stderr

	if err := b.cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting ddflagd: %w", err)
	}
	go func() { b.exited <- b.cmd.Wait() }()

	return b, nil
}

// EvalURL is the base URL of the evaluation listener.
func (b *Bridge) EvalURL() string { return "http://" + b.evalAddr }

// AdminURL is the base URL of the operational listener.
func (b *Bridge) AdminURL() string { return "http://" + b.adminAddr }

// Logs returns everything the process wrote to stdout and stderr.
func (b *Bridge) Logs() string {
	return "stdout:\n" + b.stdout.String() + "\nstderr:\n" + b.stderr.String()
}

// WaitReady blocks until /readyz answers 200.
func (b *Bridge) WaitReady(ctx context.Context) error {
	return waitFor(ctx, 200*time.Millisecond, func() error {
		if exited, exitErr := b.reap(0); exited {
			if exitErr == nil {
				return fmt.Errorf("the process exited cleanly before becoming ready\n%s", b.Logs())
			}
			return fmt.Errorf("the process exited before becoming ready: %w\n%s", exitErr, b.Logs())
		}

		status, _, err := b.getJSON(ctx, b.AdminURL()+"/readyz")
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("/readyz returned %d", status)
		}
		return nil
	})
}

// Evaluate posts an OFREP single flag evaluation and returns the status code
// and the decoded body.
func (b *Bridge) Evaluate(ctx context.Context, key string, evalContext map[string]any) (int, map[string]any, error) {
	payload, err := json.Marshal(map[string]any{"context": evalContext})
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		b.EvalURL()+"/ofrep/v1/evaluate/flags/"+key, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := b.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return 0, nil, err
	}
	var decoded map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &decoded); err != nil {
			return res.StatusCode, nil, fmt.Errorf("decoding %q: %w", body, err)
		}
	}
	return res.StatusCode, decoded, nil
}

// Status fetches /debug/status.
func (b *Bridge) Status(ctx context.Context) (map[string]any, error) {
	_, body, err := b.getJSON(ctx, b.AdminURL()+"/debug/status")
	return body, err
}

// Metrics fetches /metrics.
func (b *Bridge) Metrics(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.AdminURL()+"/metrics", nil)
	if err != nil {
		return "", err
	}
	res, err := b.client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	return string(body), err
}

// Get performs a GET against the operational listener and returns the status
// code and the decoded body.
func (b *Bridge) Get(ctx context.Context, path string) (int, map[string]any, error) {
	return b.getJSON(ctx, b.AdminURL()+path)
}

// WaitExit blocks until the process exits on its own. It reports whether it
// exited at all, separately from how it exited, so that a caller testing for a
// non-zero exit cannot mistake a process that never stopped for one that did.
func (b *Bridge) WaitExit(timeout time.Duration) (bool, error) {
	return b.reap(timeout)
}

// Stop makes sure the process is not left behind, whatever state it is in. It
// is what a test registers with t.Cleanup, so it reports nothing and escalates
// to a kill rather than waiting indefinitely.
func (b *Bridge) Stop() {
	if err := b.signal(); err != nil {
		return
	}
	if exited, _ := b.reap(stopGrace); exited || b.cmd.Process == nil {
		return
	}
	_ = b.cmd.Process.Kill()
	_, _ = b.reap(stopGrace)
}

// Terminate sends SIGTERM and waits for the process to exit, which is the
// shutdown sequence a Kubernetes Pod termination triggers.
func (b *Bridge) Terminate(timeout time.Duration) error {
	if err := b.signal(); err != nil {
		return err
	}
	exited, exitErr := b.reap(timeout)
	if !exited {
		if b.cmd.Process != nil {
			_ = b.cmd.Process.Kill()
		}
		return fmt.Errorf("the process did not exit within %s", timeout)
	}
	return exitErr
}

// reap records how the process exited, at most once, and reports whether it
// has exited at all. A timeout of zero asks without waiting.
//
// The exit arrives on a channel carrying exactly one value and several helpers
// ask about it, so whoever reads it first has to keep the answer for the rest.
func (b *Bridge) reap(timeout time.Duration) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.reaped {
		return true, b.exitErr
	}
	if timeout <= 0 {
		select {
		case err := <-b.exited:
			b.reaped, b.exitErr = true, err
			return true, err
		default:
			return false, nil
		}
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-b.exited:
		b.reaped, b.exitErr = true, err
		return true, err
	case <-timer.C:
		return false, nil
	}
}

// signal asks the process to stop. A process that has already gone is not an
// error: a test may terminate a bridge that stopped on its own.
func (b *Bridge) signal() error {
	if b.cmd.Process == nil {
		return nil
	}
	if exited, _ := b.reap(0); exited {
		return nil
	}
	if err := b.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func (b *Bridge) getJSON(ctx context.Context, u string) (int, map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, err
	}
	res, err := b.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return res.StatusCode, nil, err
	}
	var decoded map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &decoded); err != nil {
			return res.StatusCode, nil, fmt.Errorf("decoding %q: %w", body, err)
		}
	}
	return res.StatusCode, decoded, nil
}

func freeLoopbackAddr() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer l.Close()
	return l.Addr().String(), nil
}
