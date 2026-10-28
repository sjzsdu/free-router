package cmd

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjzsdu/free-router/internal/gateway"
	"github.com/sjzsdu/free-router/internal/transport"
	"github.com/spf13/cobra"
)

func TestValidateModelDataRejectsEmptyPath(t *testing.T) {
	if err := validateModelData("  ", &bytes.Buffer{}); err == nil {
		t.Fatal("empty model data path should be rejected")
	}
}

func TestLoadDaemonEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon-env.json")
	if err := os.WriteFile(path, []byte(`{"MY_GEMINI_KEY":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FREE_ROUTER_DAEMON_ENV_FILE", path)
	t.Setenv("MY_GEMINI_KEY", "")
	if err := loadDaemonEnvironment(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("MY_GEMINI_KEY"); got != "secret" {
		t.Fatalf("loaded environment = %q", got)
	}
}

func TestLoadDaemonEnvironmentDoesNotOverrideProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon-env.json")
	if err := os.WriteFile(path, []byte(`{"MY_GEMINI_KEY":"snapshot"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FREE_ROUTER_DAEMON_ENV_FILE", path)
	t.Setenv("MY_GEMINI_KEY", "process")
	if err := loadDaemonEnvironment(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("MY_GEMINI_KEY"); got != "process" {
		t.Fatalf("process environment was overwritten: %q", got)
	}
}

func TestDefaultOptionsUseUnifiedDataDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FREE_ROUTER_CACHE", "")
	t.Setenv("FREE_ROUTER_CONFIG", "")
	t.Setenv("FREE_ROUTER_CREDENTIALS", "")
	t.Setenv("FREE_ROUTER_HEALTH", "")

	opts := defaultOptions()
	dataDir := filepath.Join(home, ".free-router")
	if got, want := opts.cache, filepath.Join(dataDir, "models.json"); got != want {
		t.Fatalf("cache path = %s, want %s", got, want)
	}
	if got, want := opts.config, filepath.Join(dataDir, "config.json"); got != want {
		t.Fatalf("config path = %s, want %s", got, want)
	}
	if got, want := opts.credentials, filepath.Join(dataDir, "credentials.json"); got != want {
		t.Fatalf("credentials path = %s, want %s", got, want)
	}
	if got, want := opts.health, filepath.Join(dataDir, "health.json"); got != want {
		t.Fatalf("health path = %s, want %s", got, want)
	}
}

func TestDefaultAddressIsLoopback(t *testing.T) {
	t.Setenv("FREE_ROUTER_ADDR", "")
	opts := defaultOptions()
	if opts.addr != "127.0.0.1:1314" {
		t.Fatalf("default addr = %q, want %q", opts.addr, "127.0.0.1:1314")
	}
}

func TestIsRemoteAddr(t *testing.T) {
	tests := []struct {
		name     string
		addr     string
		expected bool
	}{
		{"loopback ipv4", "127.0.0.1:1314", false},
		{"loopback ipv6", "[::1]:1314", false},
		{"localhost", "localhost:1314", false},
		{"wildcard ipv4", "0.0.0.0:1314", true},
		{"wildcard ipv6", ":::1314", true},
		{"empty host", ":1314", true},
		{"external ip", "192.168.1.100:1314", true},
		{"hostname", "myhost.local:1314", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRemoteAddr(tt.addr); got != tt.expected {
				t.Fatalf("isRemoteAddr(%q) = %v, want %v", tt.addr, got, tt.expected)
			}
		})
	}
}

func TestDefaultLoggingOptionsComeFromEnv(t *testing.T) {
	t.Setenv("FREE_ROUTER_LOG_LEVEL", "debug")
	t.Setenv("FREE_ROUTER_LOG_FORMAT", "json")
	opts := defaultOptions()
	if opts.logLevel != "debug" || opts.logFormat != "json" {
		t.Fatalf("logging options = %q/%q, want debug/json", opts.logLevel, opts.logFormat)
	}
}

func TestLoggingFlagsAreRegistered(t *testing.T) {
	opts := defaultOptions()
	command := &cobra.Command{Use: "free-router"}
	bindFlags(command, &opts)
	for _, name := range []string{"log-level", "log-format"} {
		if command.PersistentFlags().Lookup(name) == nil {
			t.Fatalf("persistent flag --%s is not registered", name)
		}
	}
}

// clearTimeoutEnv keeps the operator's shell from influencing the default
// assertions below.
func clearTimeoutEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"FREE_ROUTER_UPSTREAM_HEADER_TIMEOUT",
		"FREE_ROUTER_STREAM_STALL_TIMEOUT",
		"FREE_ROUTER_WRITE_TIMEOUT",
	} {
		t.Setenv(key, "")
	}
}

// The three timeouts are new configuration surfaces: their defaults must stay
// exactly what they were before the surface existed.
func TestDefaultOptionsKeepHistoricalTimeouts(t *testing.T) {
	clearTimeoutEnv(t)
	opts := defaultOptions()
	if opts.upstreamHeaderTimeout != transport.DefaultResponseHeaderTimeout {
		t.Fatalf("upstream header timeout = %v, want %v", opts.upstreamHeaderTimeout, transport.DefaultResponseHeaderTimeout)
	}
	if opts.upstreamHeaderTimeout != 20*time.Second {
		t.Fatalf("upstream header timeout default changed: %v", opts.upstreamHeaderTimeout)
	}
	if opts.streamStallTimeout != gateway.DefaultStreamStallTimeout {
		t.Fatalf("stream stall timeout = %v, want %v", opts.streamStallTimeout, gateway.DefaultStreamStallTimeout)
	}
	if opts.streamStallTimeout != 5*time.Minute {
		t.Fatalf("stream stall timeout default changed: %v", opts.streamStallTimeout)
	}
	if opts.writeTimeout != defaultWriteTimeout {
		t.Fatalf("write timeout = %v, want %v", opts.writeTimeout, defaultWriteTimeout)
	}
	if opts.writeTimeout != 15*time.Minute {
		t.Fatalf("write timeout default changed: %v", opts.writeTimeout)
	}
}

func TestTimeoutEnvironmentOverrides(t *testing.T) {
	clearTimeoutEnv(t)
	t.Setenv("FREE_ROUTER_UPSTREAM_HEADER_TIMEOUT", "45s")
	t.Setenv("FREE_ROUTER_STREAM_STALL_TIMEOUT", "90s")
	t.Setenv("FREE_ROUTER_WRITE_TIMEOUT", "3m")
	opts := defaultOptions()
	if opts.upstreamHeaderTimeout != 45*time.Second || opts.streamStallTimeout != 90*time.Second || opts.writeTimeout != 3*time.Minute {
		t.Fatalf("environment overrides not applied: %+v", opts)
	}
}

// A typo must not silently disable a timeout: net/http and the stall guard both
// read zero as "no limit".
func TestInvalidTimeoutEnvironmentFallsBackToDefault(t *testing.T) {
	clearTimeoutEnv(t)
	t.Setenv("FREE_ROUTER_UPSTREAM_HEADER_TIMEOUT", "not-a-duration")
	t.Setenv("FREE_ROUTER_STREAM_STALL_TIMEOUT", "-1s")
	t.Setenv("FREE_ROUTER_WRITE_TIMEOUT", "0s")
	opts := defaultOptions()
	if opts.upstreamHeaderTimeout != 20*time.Second || opts.streamStallTimeout != 5*time.Minute || opts.writeTimeout != 15*time.Minute {
		t.Fatalf("invalid values did not fall back: %+v", opts)
	}
}

func TestValidateTimeoutsRejectsNonPositive(t *testing.T) {
	base := options{upstreamHeaderTimeout: 20 * time.Second, streamStallTimeout: 5 * time.Minute, writeTimeout: 15 * time.Minute}
	if err := validateTimeouts(base); err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
	cases := map[string]options{
		"upstream-header-timeout": {upstreamHeaderTimeout: 0, streamStallTimeout: time.Minute, writeTimeout: time.Minute},
		"stream-stall-timeout":    {upstreamHeaderTimeout: time.Minute, streamStallTimeout: 0, writeTimeout: time.Minute},
		"write-timeout":           {upstreamHeaderTimeout: time.Minute, streamStallTimeout: time.Minute, writeTimeout: -time.Second},
	}
	for name, opts := range cases {
		err := validateTimeouts(opts)
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func TestHTTPServerUsesConfiguredTimeout(t *testing.T) {
	opts := options{addr: "127.0.0.1:0", upstreamHeaderTimeout: time.Minute, streamStallTimeout: time.Minute, writeTimeout: 42 * time.Second}
	server := newHTTPServer(opts, http.NotFoundHandler())
	if server.WriteTimeout != 42*time.Second {
		t.Fatalf("WriteTimeout = %v, want 42s", server.WriteTimeout)
	}
	// ReadHeaderTimeout/ReadTimeout/IdleTimeout are not part of this surface and
	// must keep their historical values.
	if server.ReadHeaderTimeout != 10*time.Second || server.ReadTimeout != 60*time.Second || server.IdleTimeout != 90*time.Second {
		t.Fatalf("unrelated server timeouts changed: %+v", server)
	}

	// Zero must not disable the deadline.
	fallback := newHTTPServer(options{addr: "127.0.0.1:0"}, http.NotFoundHandler())
	if fallback.WriteTimeout != 15*time.Minute {
		t.Fatalf("zero WriteTimeout was not restored to the default: %v", fallback.WriteTimeout)
	}
}

func TestTransportConfigAppliesUpstreamTimeout(t *testing.T) {
	opts := options{upstreamHeaderTimeout: 37 * time.Second}
	config := newTransportConfig(opts)
	if config.ResponseHeaderTimeout != 37*time.Second {
		t.Fatalf("ResponseHeaderTimeout = %v, want 37s", config.ResponseHeaderTimeout)
	}
	defaults := transport.NewConfig()
	if config.ConnectTimeout != defaults.ConnectTimeout || config.TLSHandshakeTimeout != defaults.TLSHandshakeTimeout ||
		config.IdleConnTimeout != defaults.IdleConnTimeout || config.MaxIdleConns != defaults.MaxIdleConns {
		t.Fatalf("unrelated transport settings changed: %+v vs %+v", config, defaults)
	}
}
