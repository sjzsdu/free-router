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
