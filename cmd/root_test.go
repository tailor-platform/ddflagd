package cmd

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/tailor-platform/ddflagd/internal/bridge"
	"github.com/tailor-platform/ddflagd/version"
)

// TestVersionFlag states that the binary reports the version tagpr keeps in
// step with the git tag.
func TestVersionFlag(t *testing.T) {
	cmd, out := newTestRootCmd(t)
	cmd.SetArgs([]string{"--version"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("--version: %v", err)
	}
	if !strings.Contains(out.String(), version.Version) {
		t.Errorf("--version printed %q, want it to name %s", out.String(), version.Version)
	}
}

// TestRejectsPositionalArguments states that ddflagd takes no arguments, so a
// mistyped flag is reported rather than silently ignored.
func TestRejectsPositionalArguments(t *testing.T) {
	cmd, _ := newTestRootCmd(t)
	if err := cmd.Args(cmd, []string{"ufc-config.json"}); err == nil {
		t.Fatal("want an error for a positional argument")
	}
}

// TestAConfigurationFailureIsNotAUsageError states what a misconfigured
// container writes to its log: one structured error naming the missing
// variable, and not the command's help text.
func TestAConfigurationFailureIsNotAUsageError(t *testing.T) {
	for _, name := range []string{bridge.EnvService, bridge.EnvFlaggingProviderEnabled} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}

	cmd, out := newTestRootCmd(t)
	cmd.SetArgs([]string{})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("want an error when the required configuration is absent")
	}
	if !strings.Contains(err.Error(), bridge.EnvService) {
		t.Errorf("error: got %v, want it to name %s", err, bridge.EnvService)
	}
	if strings.Contains(out.String(), "Usage:") {
		t.Errorf("a configuration failure must not print the usage:\n%s", out.String())
	}
}

// newTestRootCmd returns a root command writing into a buffer.
func newTestRootCmd(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()

	var out bytes.Buffer
	cmd := newRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	return cmd, &out
}
