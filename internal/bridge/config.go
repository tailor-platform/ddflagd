package bridge

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment variables ddflagd reads. The DD_ prefixed ones are consumed by
// dd-trace-go itself; ddflagd only reads them to validate the startup
// preconditions and to report the effective configuration.
const (
	EnvFlaggingProviderEnabled = "DD_EXPERIMENTAL_FLAGGING_PROVIDER_ENABLED"
	EnvService                 = "DD_SERVICE"
	EnvEnv                     = "DD_ENV"
	EnvVersion                 = "DD_VERSION"
	EnvTraceAgentURL           = "DD_TRACE_AGENT_URL"
	EnvAgentHost               = "DD_AGENT_HOST"
	EnvTraceAgentPort          = "DD_TRACE_AGENT_PORT"
	EnvAPMTracingEnabled       = "DD_APM_TRACING_ENABLED"
	EnvTraceStartupLogs        = "DD_TRACE_STARTUP_LOGS"
	EnvTraceEnabled            = "DD_TRACE_ENABLED"

	EnvListenAddr      = "DDFLAGD_LISTEN_ADDR"
	EnvAdminAddr       = "DDFLAGD_ADMIN_ADDR"
	EnvDrainDelay      = "DDFLAGD_DRAIN_DELAY"
	EnvPprofEnabled    = "DDFLAGD_PPROF_ENABLED"
	EnvInitTimeout     = "DDFLAGD_INIT_TIMEOUT"
	EnvShutdownTimeout = "DDFLAGD_SHUTDOWN_TIMEOUT"
	EnvHandlerTimeout  = "DDFLAGD_HANDLER_TIMEOUT"
	EnvAPIKey          = "DDFLAGD_API_KEY" //nolint:gosec // G101: a variable name, not a credential
)

// Defaults of the DDFLAGD_ settings.
const (
	DefaultListenAddr      = "127.0.0.1:8016"
	DefaultAdminAddr       = "0.0.0.0:8017"
	DefaultDrainDelay      = 5 * time.Second
	DefaultInitTimeout     = 30 * time.Second
	DefaultShutdownTimeout = 10 * time.Second
	DefaultHandlerTimeout  = 200 * time.Millisecond

	// MaxRequestBodyBytes caps the OFREP request body.
	MaxRequestBodyBytes int64 = 64 * 1024
	// MaxContextAttributes caps the number of evaluation context attributes.
	MaxContextAttributes = 256
	// MaxContextStringBytes caps the length of a single string attribute value.
	MaxContextStringBytes = 4 * 1024
)

// Config is the effective configuration of one ddflagd process.
type Config struct {
	Service string
	Env     string
	Version string

	// AgentURL is the Agent endpoint as configured, for reporting only. An
	// empty value means dd-trace-go falls back to its own default.
	AgentURL string

	ListenAddr      string
	AdminAddr       string
	DrainDelay      time.Duration
	PprofEnabled    bool
	InitTimeout     time.Duration
	ShutdownTimeout time.Duration
	HandlerTimeout  time.Duration
	APIKey          string
}

// LoadConfig reads the configuration from the process environment and validates
// the startup preconditions.
func LoadConfig() (*Config, error) {
	c := &Config{
		Service:         os.Getenv(EnvService),
		Env:             os.Getenv(EnvEnv),
		Version:         os.Getenv(EnvVersion),
		AgentURL:        agentURL(),
		ListenAddr:      stringEnv(EnvListenAddr, DefaultListenAddr),
		AdminAddr:       stringEnv(EnvAdminAddr, DefaultAdminAddr),
		PprofEnabled:    boolEnv(EnvPprofEnabled, false),
		DrainDelay:      DefaultDrainDelay,
		InitTimeout:     DefaultInitTimeout,
		ShutdownTimeout: DefaultShutdownTimeout,
		HandlerTimeout:  DefaultHandlerTimeout,
		APIKey:          os.Getenv(EnvAPIKey),
	}

	var errs []error
	for _, d := range []struct {
		name string
		dst  *time.Duration
	}{
		{EnvDrainDelay, &c.DrainDelay},
		{EnvInitTimeout, &c.InitTimeout},
		{EnvShutdownTimeout, &c.ShutdownTimeout},
		{EnvHandlerTimeout, &c.HandlerTimeout},
	} {
		v, err := durationEnv(d.name, *d.dst)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		*d.dst = v
	}

	if err := c.Validate(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

// Validate reports why the configuration cannot start a bridge.
func (c *Config) Validate() error {
	var errs []error
	if c.Service == "" {
		errs = append(errs, fmt.Errorf("%s is required: it identifies the consuming service in exposure events and evaluation metrics", EnvService))
	}
	if !boolEnv(EnvFlaggingProviderEnabled, false) {
		errs = append(errs, fmt.Errorf("%s=true is required: without it the official provider returns a NoopProvider", EnvFlaggingProviderEnabled))
	}
	// DD_TRACE_ENABLED=false stops the tracer from starting, which stops the
	// Remote Configuration client that feeds the provider. It is easy to
	// confuse with DD_APM_TRACING_ENABLED, so reject it outright.
	if v, ok := os.LookupEnv(EnvTraceEnabled); ok && !parseBool(v, true) {
		errs = append(errs, fmt.Errorf("%s must not be false: the tracer carries the Remote Configuration client. Use %s=false to keep APM data out instead", EnvTraceEnabled, EnvAPMTracingEnabled))
	}
	if c.ListenAddr == "" {
		errs = append(errs, fmt.Errorf("%s must not be empty", EnvListenAddr))
	}
	if c.AdminAddr == "" {
		errs = append(errs, fmt.Errorf("%s must not be empty", EnvAdminAddr))
	}
	if c.ListenAddr == c.AdminAddr {
		errs = append(errs, fmt.Errorf("%s and %s must differ: the evaluation and operational listeners are deliberately separate", EnvListenAddr, EnvAdminAddr))
	}
	if c.DrainDelay < 0 {
		errs = append(errs, fmt.Errorf("%s must not be negative", EnvDrainDelay))
	}
	for _, d := range []struct {
		name string
		v    time.Duration
	}{
		{EnvInitTimeout, c.InitTimeout},
		{EnvShutdownTimeout, c.ShutdownTimeout},
		{EnvHandlerTimeout, c.HandlerTimeout},
	} {
		if d.v <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive", d.name))
		}
	}
	return errors.Join(errs...)
}

// Redacted returns the effective configuration for /debug/status. The shared
// secret is replaced by whether it is set.
func (c *Config) Redacted() map[string]any {
	return map[string]any{
		EnvService:         c.Service,
		EnvEnv:             c.Env,
		EnvVersion:         c.Version,
		"agent_url":        c.AgentURL,
		EnvListenAddr:      c.ListenAddr,
		EnvAdminAddr:       c.AdminAddr,
		EnvDrainDelay:      c.DrainDelay.String(),
		EnvPprofEnabled:    c.PprofEnabled,
		EnvInitTimeout:     c.InitTimeout.String(),
		EnvShutdownTimeout: c.ShutdownTimeout.String(),
		EnvHandlerTimeout:  c.HandlerTimeout.String(),
		EnvAPIKey:          redactedSecret(c.APIKey),
	}
}

func redactedSecret(v string) string {
	if v == "" {
		return "(unset)"
	}
	return "(set)"
}

// agentURL reports the Agent endpoint as configured by the environment, in the
// same order of precedence dd-trace-go uses.
func agentURL() string {
	if v := os.Getenv(EnvTraceAgentURL); v != "" {
		return v
	}
	host := os.Getenv(EnvAgentHost)
	port := os.Getenv(EnvTraceAgentPort)
	if host == "" && port == "" {
		return ""
	}
	if host == "" {
		host = "localhost"
	}
	if port == "" {
		port = "8126"
	}
	return "http://" + host + ":" + port
}

func stringEnv(name, def string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return def
}

func boolEnv(name string, def bool) bool {
	v, ok := os.LookupEnv(name)
	if !ok {
		return def
	}
	return parseBool(v, def)
}

func parseBool(v string, def bool) bool {
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return b
}

func durationEnv(name string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return def, fmt.Errorf("%s: %q is not a duration: %w", name, v, err)
	}
	return d, nil
}
