package doctor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjzsdu/free-router/internal/provider"
)

// clearProviderKeys keeps the diagnosis independent of the machine running the
// test: provider status is read from the environment, so a developer with real
// keys exported would otherwise see different results.
func clearProviderKeys(t *testing.T) {
	t.Helper()
	for _, name := range provider.SupportedKeyEnvs() {
		t.Setenv(name, "")
	}
}

func testOptions(t *testing.T, addr string) Options {
	t.Helper()
	dir := t.TempDir()
	return Options{
		Addr:            addr,
		ConfigPath:      filepath.Join(dir, "config.json"),
		FreeModels:      filepath.Join(dir, "free-models"),
		CredentialsPath: filepath.Join(dir, "credentials.json"),
		HealthPath:      filepath.Join(dir, "health.json"),
		StatisticsPath:  filepath.Join(dir, "statistics.json"),
		LogPath:         filepath.Join(dir, "free-router.log"),
	}
}

func checkByName(t *testing.T, report Report, name string) Check {
	t.Helper()
	for _, check := range report.Checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("check %q not found in %#v", name, report.Checks)
	return Check{}
}

// An installation with no credentials and an unreachable service must fail,
// not report a reassuring "ok": that is precisely the case users run doctor
// for.
func TestRunReportsBrokenInstallation(t *testing.T) {
	clearProviderKeys(t)
	opts := testOptions(t, "127.0.0.1:1")
	report := Run(context.Background(), opts)
	if !report.Failed() {
		t.Fatalf("expected failures, got %#v", report.Checks)
	}
	if check := checkByName(t, report, "provider credentials"); check.Status != StatusFail {
		t.Fatalf("provider check = %#v", check)
	}
	if check := checkByName(t, report, "running service"); check.Status != StatusWarn {
		t.Fatalf("service check = %#v", check)
	}
	if check := checkByName(t, report, "recent failures"); check.Status != StatusSkip {
		t.Fatalf("recent failures should be skipped when the service is down: %#v", check)
	}
}

func TestRunReportsHealthyInstallation(t *testing.T) {
	clearProviderKeys(t)
	t.Setenv("GROQ_API_KEY", "test-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","free_models":12,"providers":3,"catalog_count":15}`))
		case "/admin/api/failures":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"failures":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	opts := testOptions(t, strings.TrimPrefix(server.URL, "http://"))
	report := Run(context.Background(), opts)
	if report.Failed() {
		t.Fatalf("unexpected failure: %#v", report.Checks)
	}
	if check := checkByName(t, report, "running service"); check.Status != StatusOK || !strings.Contains(check.Detail, "12 maintained models") {
		t.Fatalf("service check = %#v", check)
	}
	if check := checkByName(t, report, "recent failures"); check.Status != StatusOK {
		t.Fatalf("recent failures check = %#v", check)
	}
	if check := checkByName(t, report, "provider credentials"); check.Status != StatusOK || !strings.Contains(check.Detail, "groq") {
		t.Fatalf("provider check = %#v", check)
	}
}

func TestFailureLogIsSkippedWhenAdminRequiresToken(t *testing.T) {
	clearProviderKeys(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	opts := testOptions(t, strings.TrimPrefix(server.URL, "http://"))
	report := Run(context.Background(), opts)
	check := checkByName(t, report, "recent failures")
	if check.Status != StatusSkip || !strings.Contains(check.Hint, "FREE_ROUTER_ADMIN_TOKEN") {
		t.Fatalf("recent failures check = %#v", check)
	}
}

func TestFailureLogSurfacesLatestCause(t *testing.T) {
	clearProviderKeys(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/admin/api/failures":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"failures":[{"at":"2026-10-17T15:00:00Z","request_id":"abc123","route":"chat","status":429,"code":"rate_limited","message":"all 2 configured free providers are rate limited"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	opts := testOptions(t, strings.TrimPrefix(server.URL, "http://"))
	report := Run(context.Background(), opts)
	check := checkByName(t, report, "recent failures")
	if check.Status != StatusWarn {
		t.Fatalf("recent failures check = %#v", check)
	}
	for _, want := range []string{"rate_limited", "abc123", "chat"} {
		if !strings.Contains(check.Detail, want) {
			t.Fatalf("detail %q missing %q", check.Detail, want)
		}
	}
}

func TestConfigCheckFailsOnInvalidJSON(t *testing.T) {
	clearProviderKeys(t)
	opts := testOptions(t, "127.0.0.1:1")
	if err := os.WriteFile(opts.ConfigPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	report := Run(context.Background(), opts)
	check := checkByName(t, report, "route configuration")
	if check.Status != StatusFail || !strings.Contains(check.Hint, "onboard") {
		t.Fatalf("config check = %#v", check)
	}
}

func TestServiceURLAcceptsHostAndScheme(t *testing.T) {
	if got := serviceURL("", "/healthz"); got != "http://127.0.0.1:1314/healthz" {
		t.Fatalf("default URL = %q", got)
	}
	if got := serviceURL("localhost:9000", "/healthz"); got != "http://localhost:9000/healthz" {
		t.Fatalf("host URL = %q", got)
	}
	if got := serviceURL("https://router.example.com/", "/healthz"); got != "https://router.example.com/healthz" {
		t.Fatalf("scheme URL = %q", got)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{512: "512 B", 2048: "2.0 KiB", 5 << 20: "5.0 MiB"}
	for size, want := range cases {
		if got := humanBytes(size); got != want {
			t.Fatalf("humanBytes(%d) = %q, want %q", size, got, want)
		}
	}
}
