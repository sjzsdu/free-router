package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
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
