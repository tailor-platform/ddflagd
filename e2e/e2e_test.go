//go:build e2e

// Package e2e drives a real ddflagd process against Datadog's own fake Agent.
//
// The Remote Configuration delivery and the EVP proxy come from
// dd-apm-test-agent, the fake Agent Datadog uses in every tracer's CI, so the
// suite never has to reimplement either. No Datadog account is involved.
package e2e

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	ufcPath = "../testdata/ffe-system-test-data/ufc-config.json"

	service        = "ddflagd-e2e-service"
	environment    = "e2e"
	serviceVersion = "1.2.3"

	primaryConfigID = "ddflagd-e2e"
)

var (
	bridge    *Bridge
	proxy     *AgentProxy
	testAgent *TestAgent
	ufc       json.RawMessage
	setupErr  error
)

func TestMain(m *testing.M) {
	code := run(m)
	os.Exit(code)
}

func run(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	tempDir, err := os.MkdirTemp("", "ddflagd-e2e")
	if err != nil {
		setupErr = err
		return runWithSetupError(m)
	}
	defer os.RemoveAll(tempDir)

	raw, err := os.ReadFile(ufcPath)
	if err != nil {
		setupErr = err
		return runWithSetupError(m)
	}
	ufc = json.RawMessage(raw)

	agent, stopAgent, err := StartTestAgent(ctx)
	if err != nil {
		setupErr = err
		return runWithSetupError(m)
	}
	defer stopAgent()
	testAgent = agent

	if err := testAgent.SetFlagConfiguration(ctx, primaryConfigID, ufc); err != nil {
		setupErr = err
		return runWithSetupError(m)
	}

	proxy, err = NewAgentProxy(testAgent.URL())
	if err != nil {
		setupErr = err
		return runWithSetupError(m)
	}
	defer proxy.Close()

	binary, err := BuildBridge(ctx, tempDir)
	if err != nil {
		setupErr = err
		return runWithSetupError(m)
	}

	bridge, err = StartBridge(ctx, BridgeConfig{
		Binary:   binary,
		AgentURL: proxy.URL(),
		Service:  service,
		Env:      environment,
		Version:  serviceVersion,
	})
	if err != nil {
		setupErr = err
		return runWithSetupError(m)
	}
	if err := bridge.WaitReady(ctx); err != nil {
		setupErr = err
		code := runWithSetupError(m)
		_ = bridge.Terminate(10 * time.Second)
		return code
	}

	code := m.Run()

	if err := bridge.Terminate(20 * time.Second); err != nil {
		// A non-zero exit here means the shutdown sequence did not complete,
		// which is a failure of the thing under test.
		println("terminating the bridge:", err.Error())
		println(bridge.Logs())
		if code == 0 {
			code = 1
		}
	}
	return code
}

func runWithSetupError(m *testing.M) int {
	return m.Run()
}

func requireSetup(t *testing.T) {
	t.Helper()
	if setupErr != nil {
		t.Fatalf("the e2e environment is not available: %v", setupErr)
	}
}

// TestBridgeBecomesReady states that the bridge reports readiness only once the
// official provider has received a flag configuration over Remote
// Configuration, and that its status names the versions an operator needs.
func TestBridgeBecomesReady(t *testing.T) {
	requireSetup(t)

	status, err := bridge.Status(t.Context())
	if err != nil {
		t.Fatalf("fetching the status: %v", err)
	}

	if status["state"] != "ready" {
		t.Errorf("state: got %#v, want ready", status["state"])
	}
	if _, ok := status["ready_at"]; !ok {
		t.Error("ready_at is missing from a ready bridge")
	}
	ddVersion, _ := status["dd_trace_go_version"].(string)
	if !strings.HasPrefix(ddVersion, "v") {
		t.Errorf("dd_trace_go_version: got %#v, want a v prefixed module version", status["dd_trace_go_version"])
	}

	config, ok := status["config"].(map[string]any)
	if !ok {
		t.Fatalf("config: got %#v", status["config"])
	}
	if config["DD_SERVICE"] != service {
		t.Errorf("config[DD_SERVICE]: got %#v, want %s", config["DD_SERVICE"], service)
	}

	code, healthz, err := bridge.Get(t.Context(), "/healthz")
	if err != nil {
		t.Fatalf("fetching liveness: %v", err)
	}
	if code != 200 {
		t.Errorf("/healthz: got %d, want 200 (%v)", code, healthz)
	}
}

// TestEvaluationsFollowTheFlagConfiguration states that a flag from the shared
// Datadog configuration evaluates through OFREP, for a value and for a code
// default.
func TestEvaluationsFollowTheFlagConfiguration(t *testing.T) {
	requireSetup(t)

	t.Run("a matching allocation returns the value", func(t *testing.T) {
		code, body, err := bridge.Evaluate(t.Context(), "new-user-onboarding", map[string]any{
			"targetingKey": "e2e-value-1",
			"country":      "France",
			"email":        "alice@mycompany.com",
		})
		if err != nil {
			t.Fatalf("evaluating: %v", err)
		}
		if code != 200 {
			t.Fatalf("status: got %d, want 200 (%v)", code, body)
		}
		if body["value"] == nil {
			t.Errorf("the response carries no value: %v", body)
		}
		if body["reason"] == "UNKNOWN" {
			t.Errorf("reason: got UNKNOWN, want an allocation to have matched: %v", body)
		}
	})

	t.Run("an unknown flag is 404", func(t *testing.T) {
		code, body, err := bridge.Evaluate(t.Context(), "flag-that-does-not-exist", map[string]any{
			"targetingKey": "e2e-value-2",
		})
		if err != nil {
			t.Fatalf("evaluating: %v", err)
		}
		if code != 404 || body["errorCode"] != "FLAG_NOT_FOUND" {
			t.Fatalf("got %d %v, want 404 FLAG_NOT_FOUND", code, body)
		}
	})

	t.Run("a disabled flag asks the caller for its code default", func(t *testing.T) {
		code, body, err := bridge.Evaluate(t.Context(), "disabled_flag", map[string]any{
			"targetingKey": "e2e-value-3",
		})
		if err != nil {
			t.Fatalf("evaluating: %v", err)
		}
		if code != 200 {
			t.Fatalf("status: got %d, want 200 (%v)", code, body)
		}
		if body["reason"] != "DISABLED" {
			t.Errorf("reason: got %#v, want DISABLED", body["reason"])
		}
		if _, ok := body["value"]; ok {
			t.Errorf("a disabled flag must omit the value: %v", body)
		}
	})
}

// TestExposureReachesTheAgent states that an evaluation produces an exposure
// event carrying the consuming service's identifiers and the caller's targeting
// key and attributes. This is the whole point of putting the official SDK
// behind the bridge.
func TestExposureReachesTheAgent(t *testing.T) {
	requireSetup(t)

	// numeric_flag is used because its allocation sets doLog, which is what
	// makes the official provider emit an exposure at all.
	const (
		targetingKey = "e2e-exposure-subject"
		flag         = "numeric_flag"
	)
	code, body, err := bridge.Evaluate(t.Context(), flag, map[string]any{
		"targetingKey": targetingKey,
		"country":      "France",
		"age":          42,
	})
	if err != nil {
		t.Fatalf("evaluating: %v", err)
	}
	if code != 200 {
		t.Fatalf("status: got %d, want 200 (%v)", code, body)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	var found ExposurePayload
	err = waitFor(ctx, 250*time.Millisecond, func() error {
		for _, payload := range proxy.Exposures() {
			for _, exposure := range payload.Exposures {
				if exposure.Subject.ID == targetingKey {
					found = payload
					return nil
				}
			}
		}
		return errNotYet
	})
	if err != nil {
		t.Fatalf("waiting for the exposure event: %v\n%s", err, bridge.Logs())
	}

	if found.Context.Service != service {
		t.Errorf("context.service: got %q, want %q", found.Context.Service, service)
	}
	if found.Context.Env != environment {
		t.Errorf("context.env: got %q, want %q", found.Context.Env, environment)
	}
	if found.Context.Version != serviceVersion {
		t.Errorf("context.version: got %q, want %q", found.Context.Version, serviceVersion)
	}

	var exposure = found.Exposures[0]
	for _, e := range found.Exposures {
		if e.Subject.ID == targetingKey {
			exposure = e
		}
	}
	if exposure.Flag.Key != flag {
		t.Errorf("flag.key: got %q, want %q", exposure.Flag.Key, flag)
	}
	if exposure.Allocation.Key == "" {
		t.Error("allocation.key is empty")
	}
	if exposure.Variant.Key == "" {
		t.Error("variant.key is empty")
	}
	if got := exposure.Subject.Attributes["country"]; got != "France" {
		t.Errorf("subject.attributes[country]: got %#v, want France", got)
	}
	if got := exposure.Subject.Attributes["age"]; got != float64(42) {
		t.Errorf("subject.attributes[age]: got %#v, want 42", got)
	}

	// The EVP proxy needs the subdomain header to route to the intake.
	var sawSubdomain bool
	for _, header := range proxy.ExposureHeaders() {
		if header.Get("X-Datadog-EVP-Subdomain") == "event-platform-intake" {
			sawSubdomain = true
		}
	}
	if !sawSubdomain {
		t.Error("no exposure request carried the EVP subdomain header")
	}
}

// TestEvaluationCountsReachTheAgent states that the evaluation count path runs
// as well, which is what feeds the flag evaluation view in Datadog.
func TestEvaluationCountsReachTheAgent(t *testing.T) {
	requireSetup(t)

	code, body, err := bridge.Evaluate(t.Context(), "numeric_flag", map[string]any{
		"targetingKey": "e2e-counts-subject",
	})
	if err != nil {
		t.Fatalf("evaluating: %v", err)
	}
	if code != 200 {
		t.Fatalf("status: got %d, want 200 (%v)", code, body)
	}

	// The provider flushes evaluation counts every ten seconds.
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()

	err = waitFor(ctx, 500*time.Millisecond, func() error {
		if len(proxy.FlagEvaluations()) > 0 {
			return nil
		}
		return errNotYet
	})
	if err != nil {
		t.Fatalf("waiting for an evaluation count payload: %v\n%s", err, bridge.Logs())
	}
}

// TestMetricsReportTheOutcomes states that the operational listener reports the
// request result distribution, which is how the share of evaluations that fell
// back to a code default is monitored. The caller cannot tell those apart.
func TestMetricsReportTheOutcomes(t *testing.T) {
	requireSetup(t)

	if _, _, err := bridge.Evaluate(t.Context(), "disabled_flag", map[string]any{"targetingKey": "e2e-metrics"}); err != nil {
		t.Fatalf("evaluating: %v", err)
	}
	if _, _, err := bridge.Evaluate(t.Context(), "flag-that-does-not-exist", map[string]any{"targetingKey": "e2e-metrics"}); err != nil {
		t.Fatalf("evaluating: %v", err)
	}

	body, err := bridge.Metrics(t.Context())
	if err != nil {
		t.Fatalf("fetching the metrics: %v", err)
	}
	for _, want := range []string{
		"ddflagd_provider_ready 1",
		`ddflagd_evaluations_total{outcome="code_default"}`,
		`ddflagd_evaluations_total{outcome="flag_not_found"}`,
		"ddflagd_evaluation_duration_seconds_count",
		"ddflagd_build_info{version=",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the metrics are missing %q:\n%s", want, body)
		}
	}
}

// TestFlagConfigurationUpdateIsPickedUp states that a changed flag
// configuration reaches evaluations without restarting the bridge, which is
// what makes the propagation delay the Remote Configuration poll interval and
// nothing else.
func TestFlagConfigurationUpdateIsPickedUp(t *testing.T) {
	requireSetup(t)

	const flag = "numeric_flag"

	_, before, err := bridge.Evaluate(t.Context(), flag, map[string]any{"targetingKey": "e2e-update"})
	if err != nil {
		t.Fatalf("evaluating: %v", err)
	}
	if before["value"] == nil {
		t.Fatalf("the flag does not evaluate to a value before the update: %v", before)
	}

	// Disabling the flag is a change every allocation shape agrees on, so the
	// assertion does not depend on the fixture's variation values.
	updated, err := disableFlag(ufc, flag)
	if err != nil {
		t.Fatal(err)
	}
	if err := testAgent.SetFlagConfiguration(t.Context(), primaryConfigID, updated); err != nil {
		t.Fatalf("installing the updated configuration: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := testAgent.SetFlagConfiguration(ctx, primaryConfigID, ufc); err != nil {
			t.Errorf("restoring the configuration: %v", err)
			return
		}
		if err := waitForReason(ctx, flag, "e2e-update-restore", "STATIC"); err != nil {
			t.Errorf("waiting for the restored configuration: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := waitForReason(ctx, flag, "e2e-update-after", "DISABLED"); err != nil {
		t.Fatalf("waiting for the updated configuration: %v\n%s", err, bridge.Logs())
	}
}

// TestSharedSecretGuardsTheEvaluationListener states that a Deployment style
// bridge can require a shared secret.
func TestSharedSecretGuardsTheEvaluationListener(t *testing.T) {
	requireSetup(t)

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	guarded, err := StartBridge(ctx, BridgeConfig{
		Binary:   bridge.cmd.Path,
		AgentURL: proxy.URL(),
		Service:  service,
		Env:      environment,
		Version:  serviceVersion,
		Extra:    map[string]string{"DDFLAGD_API_KEY": "shared-secret"},
	})
	if err != nil {
		t.Fatalf("starting a guarded bridge: %v", err)
	}
	t.Cleanup(func() {
		if err := guarded.Terminate(20 * time.Second); err != nil {
			t.Errorf("terminating the guarded bridge: %v\n%s", err, guarded.Logs())
		}
	})
	if err := guarded.WaitReady(ctx); err != nil {
		t.Fatalf("waiting for the guarded bridge: %v", err)
	}

	code, _, err := guarded.Evaluate(ctx, "numeric_flag", map[string]any{"targetingKey": "e2e-auth"})
	if err != nil {
		t.Fatalf("evaluating without the secret: %v", err)
	}
	if code != 401 {
		t.Errorf("without the secret: got %d, want 401", code)
	}
}

// helpers

var errNotYet = errNotReady{}

type errNotReady struct{}

func (errNotReady) Error() string { return "not yet" }

func waitForReason(ctx context.Context, flag, targetingKeyPrefix, wantReason string) error {
	attempt := 0
	return waitFor(ctx, 500*time.Millisecond, func() error {
		attempt++
		// A fresh targeting key each attempt keeps the provider's exposure
		// deduplication from hiding a later evaluation.
		_, body, err := bridge.Evaluate(ctx, flag, map[string]any{
			"targetingKey": targetingKeyPrefix + "-" + itoa(attempt),
		})
		if err != nil {
			return err
		}
		if body["reason"] != wantReason {
			return errNotYet
		}
		return nil
	})
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

// disableFlag returns the flag configuration with one flag switched off.
func disableFlag(config json.RawMessage, flag string) (json.RawMessage, error) {
	var decoded map[string]any
	if err := json.Unmarshal(config, &decoded); err != nil {
		return nil, err
	}
	flags, ok := decoded["flags"].(map[string]any)
	if !ok {
		return nil, errFlagNotInFixture{flag: flag}
	}
	entry, ok := flags[flag].(map[string]any)
	if !ok {
		return nil, errFlagNotInFixture{flag: flag}
	}
	entry["enabled"] = false
	return json.Marshal(decoded)
}

type errFlagNotInFixture struct{ flag string }

func (e errFlagNotInFixture) Error() string {
	return "the shared flag configuration has no flag named " + e.flag
}
