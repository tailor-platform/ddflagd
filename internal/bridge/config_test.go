package bridge

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestLoadConfig states which environments start a bridge and which do not.
func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    func(*testing.T, *Config)
		wantErr string
	}{
		{
			name: "the defaults bind the evaluation listener to loopback",
			env: map[string]string{
				EnvFlaggingProviderEnabled: "true",
				EnvService:                 "rust-service",
			},
			want: func(t *testing.T, c *Config) {
				if c.ListenAddr != DefaultListenAddr {
					t.Errorf("ListenAddr: got %q, want %q", c.ListenAddr, DefaultListenAddr)
				}
				if c.AdminAddr != DefaultAdminAddr {
					t.Errorf("AdminAddr: got %q, want %q", c.AdminAddr, DefaultAdminAddr)
				}
				if c.DrainDelay != DefaultDrainDelay {
					t.Errorf("DrainDelay: got %s, want %s", c.DrainDelay, DefaultDrainDelay)
				}
				if c.HandlerTimeout != DefaultHandlerTimeout {
					t.Errorf("HandlerTimeout: got %s, want %s", c.HandlerTimeout, DefaultHandlerTimeout)
				}
				if c.InitTimeout != DefaultInitTimeout {
					t.Errorf("InitTimeout: got %s, want %s", c.InitTimeout, DefaultInitTimeout)
				}
			},
		},
		{
			name: "the durations and addresses are overridable",
			env: map[string]string{
				EnvFlaggingProviderEnabled: "true",
				EnvService:                 "rust-service",
				EnvEnv:                     "production",
				EnvVersion:                 "1.2.3",
				EnvListenAddr:              "0.0.0.0:8016",
				EnvAdminAddr:               "0.0.0.0:9090",
				EnvDrainDelay:              "0s",
				EnvHandlerTimeout:          "500ms",
				EnvInitTimeout:             "60s",
				EnvShutdownTimeout:         "20s",
				EnvPprofEnabled:            "true",
				EnvAPIKey:                  "secret",
			},
			want: func(t *testing.T, c *Config) {
				if c.ListenAddr != "0.0.0.0:8016" || c.AdminAddr != "0.0.0.0:9090" {
					t.Errorf("addresses: got %q and %q", c.ListenAddr, c.AdminAddr)
				}
				if c.DrainDelay != 0 {
					t.Errorf("DrainDelay: got %s, want 0s", c.DrainDelay)
				}
				if c.HandlerTimeout != 500*time.Millisecond {
					t.Errorf("HandlerTimeout: got %s, want 500ms", c.HandlerTimeout)
				}
				if c.InitTimeout != time.Minute || c.ShutdownTimeout != 20*time.Second {
					t.Errorf("timeouts: got %s and %s", c.InitTimeout, c.ShutdownTimeout)
				}
				if !c.PprofEnabled {
					t.Error("PprofEnabled: got false, want true")
				}
				if c.Env != "production" || c.Version != "1.2.3" {
					t.Errorf("identifiers: got env %q version %q", c.Env, c.Version)
				}
			},
		},
		{
			name: "the Agent URL is taken verbatim when set",
			env: map[string]string{
				EnvFlaggingProviderEnabled: "true",
				EnvService:                 "rust-service",
				EnvTraceAgentURL:           "unix:///var/run/datadog/apm.socket",
			},
			want: func(t *testing.T, c *Config) {
				if c.AgentURL != "unix:///var/run/datadog/apm.socket" {
					t.Errorf("AgentURL: got %q", c.AgentURL)
				}
			},
		},
		{
			name: "the Agent host and port are composed into a URL",
			env: map[string]string{
				EnvFlaggingProviderEnabled: "true",
				EnvService:                 "rust-service",
				EnvAgentHost:               "10.0.0.1",
			},
			want: func(t *testing.T, c *Config) {
				if c.AgentURL != "http://10.0.0.1:8126" {
					t.Errorf("AgentURL: got %q, want http://10.0.0.1:8126", c.AgentURL)
				}
			},
		},
		{
			name: "a missing service identifier is rejected",
			env: map[string]string{
				EnvFlaggingProviderEnabled: "true",
			},
			wantErr: EnvService,
		},
		{
			name: "a missing provider enablement is rejected",
			env: map[string]string{
				EnvService: "rust-service",
			},
			wantErr: EnvFlaggingProviderEnabled,
		},
		{
			name: "disabling the tracer is rejected because it carries Remote Configuration",
			env: map[string]string{
				EnvFlaggingProviderEnabled: "true",
				EnvService:                 "rust-service",
				EnvTraceEnabled:            "false",
			},
			wantErr: EnvTraceEnabled,
		},
		{
			name: "an unparsable duration is rejected",
			env: map[string]string{
				EnvFlaggingProviderEnabled: "true",
				EnvService:                 "rust-service",
				EnvHandlerTimeout:          "200",
			},
			wantErr: EnvHandlerTimeout,
		},
		{
			name: "sharing one address between both listeners is rejected",
			env: map[string]string{
				EnvFlaggingProviderEnabled: "true",
				EnvService:                 "rust-service",
				EnvListenAddr:              "0.0.0.0:8016",
				EnvAdminAddr:               "0.0.0.0:8016",
			},
			wantErr: EnvAdminAddr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearDDEnv(t)
			for name, value := range tt.env {
				t.Setenv(name, value)
			}

			cfg, err := LoadConfig()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("want an error mentioning %s", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error: got %v, want it to mention %s", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			tt.want(t, cfg)
		})
	}
}

// TestRedactedHidesTheSharedSecret states that /debug/status never leaks the
// shared secret.
func TestRedactedHidesTheSharedSecret(t *testing.T) {
	t.Parallel()

	cfg := &Config{Service: "rust-service", APIKey: "super-secret"}
	redacted := cfg.Redacted()

	if got := redacted[EnvAPIKey]; got != "(set)" {
		t.Errorf("%s: got %#v, want (set)", EnvAPIKey, got)
	}
	for name, value := range redacted {
		if s, ok := value.(string); ok && strings.Contains(s, "super-secret") {
			t.Errorf("%s leaks the shared secret", name)
		}
	}

	if got := (&Config{}).Redacted()[EnvAPIKey]; got != "(unset)" {
		t.Errorf("%s: got %#v, want (unset)", EnvAPIKey, got)
	}
}

func clearDDEnv(t *testing.T) {
	t.Helper()

	for _, name := range []string{
		EnvFlaggingProviderEnabled, EnvService, EnvEnv, EnvVersion,
		EnvTraceAgentURL, EnvAgentHost, EnvTraceAgentPort, EnvTraceEnabled,
		EnvListenAddr, EnvAdminAddr, EnvDrainDelay, EnvPprofEnabled,
		EnvInitTimeout, EnvShutdownTimeout, EnvHandlerTimeout, EnvAPIKey,
	} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}
