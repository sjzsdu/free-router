package cmd

import (
	"path/filepath"
	"testing"
)

// The daemon snapshot is the only channel through which foreground settings
// reach launchd/systemd. Every regression here is silent: the daemon simply
// starts without the setting, which is why this test enumerates the fields
// that used to be dropped instead of spot-checking one of them.
func TestDaemonEnvironmentCarriesRuntimeSettings(t *testing.T) {
	opts := defaultOptions()
	dataDir := t.TempDir()
	opts.cache = filepath.Join(dataDir, "models.json")
	opts.config = filepath.Join(dataDir, "config.json")
	opts.credentials = filepath.Join(dataDir, "credentials.json")
	opts.health = filepath.Join(dataDir, "health.json")
	opts.statistics = filepath.Join(dataDir, "statistics.json")
	opts.apiToken = "inference-token"
	opts.adminToken = "admin-token"
	opts.adminAllowRemote = true
	opts.maxAttempts = 9
	opts.logLevel = "debug"
	opts.logFormat = "json"

	environment, err := daemonEnvironment(opts)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"FREE_ROUTER_STATISTICS":         opts.statistics,
		"FREE_ROUTER_MAX_ATTEMPTS":       "9",
		"FREE_ROUTER_API_TOKEN":          "inference-token",
		"FREE_ROUTER_ADMIN_TOKEN":        "admin-token",
		"FREE_ROUTER_ADMIN_ALLOW_REMOTE": "true",
		"FREE_ROUTER_LOG_LEVEL":          "debug",
		"FREE_ROUTER_LOG_FORMAT":         "json",
	}
	for key, value := range want {
		if got := environment[key]; got != value {
			t.Fatalf("%s = %q, want %q", key, got, value)
		}
	}
}

func TestDaemonEnvironmentOmitsUnsetSecrets(t *testing.T) {
	opts := defaultOptions()
	opts.apiToken = ""
	opts.adminToken = ""
	opts.adminAllowRemote = false

	environment, err := daemonEnvironment(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"FREE_ROUTER_API_TOKEN", "FREE_ROUTER_ADMIN_TOKEN", "FREE_ROUTER_ADMIN_ALLOW_REMOTE"} {
		if value, ok := environment[key]; ok {
			t.Fatalf("%s should be omitted when unset, got %q", key, value)
		}
	}
}
