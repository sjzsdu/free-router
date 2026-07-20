package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

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
}
