//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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

// Bridge is a running ddflagd process.
type Bridge struct {
	cmd        *exec.Cmd
	evalAddr   string
	adminAddr  string
	client     *http.Client
	stdout     *bytes.Buffer
	stderr     *bytes.Buffer
	exited     chan error
	exitStatus error
}

// BuildBridge compiles the ddflagd binary into dir and returns its path.
func BuildBridge(ctx context.Context, dir string) (string, error) {
	binary := filepath.Join(dir, "ddflagd")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", binary, "../cmd/ddflagd")
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
		select {
		case err := <-b.exited:
			b.exitStatus = err
			return fmt.Errorf("the process exited before becoming ready: %w\n%s", err, b.Logs())
		default:
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

// Terminate sends SIGTERM and waits for the process to exit, which is the
// shutdown sequence a Kubernetes Pod termination triggers.
func (b *Bridge) Terminate(timeout time.Duration) error {
	if b.cmd.Process == nil {
		return nil
	}
	if err := b.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	select {
	case err := <-b.exited:
		b.exitStatus = err
		return err
	case <-time.After(timeout):
		_ = b.cmd.Process.Kill()
		return fmt.Errorf("the process did not exit within %s", timeout)
	}
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
