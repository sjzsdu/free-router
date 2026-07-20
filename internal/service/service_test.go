package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLaunchdPlistEscapesPathsAndSetsManager(t *testing.T) {
	content := launchdPlist("/tmp/free & router", "/tmp/router <log>.txt", "/tmp/daemon & env.json")
	for _, expected := range []string{"/tmp/free &amp; router", "/tmp/router &lt;log&gt;.txt", "/tmp/daemon &amp; env.json", "FREE_ROUTER_SERVICE_MANAGER", "FREE_ROUTER_DAEMON_ENV_FILE", "launchd"} {
		if !strings.Contains(content, expected) {
			t.Fatalf("plist does not contain %q: %s", expected, content)
		}
	}
}

func TestSystemdServiceQuotesExecutable(t *testing.T) {
	content := systemdService(`/opt/free router/free-router`, `/home/test user/daemon-env.json`)
	if !strings.Contains(content, `ExecStart="/opt/free router/free-router" serve`) {
		t.Fatalf("unexpected unit: %s", content)
	}
	if !strings.Contains(content, "Restart=on-failure") {
		t.Fatalf("unit should restart after failures: %s", content)
	}
	if !strings.Contains(content, `FREE_ROUTER_DAEMON_ENV_FILE="/home/test user/daemon-env.json"`) {
		t.Fatalf("unit should include daemon environment file: %s", content)
	}
}

func TestWriteEnvironmentUsesPrivateFile(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	manager := &Manager{goos: "linux", home: home}
	if err := manager.writeEnvironment(map[string]string{"MY_GEMINI_KEY": "secret"}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(manager.daemonEnvPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != `{"MY_GEMINI_KEY":"secret"}` {
		t.Fatalf("environment content = %s", content)
	}
	info, err := os.Stat(manager.daemonEnvPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("environment permissions = %o", info.Mode().Perm())
	}
	if got, want := manager.daemonEnvPath(), filepath.Join(home, ".free-router", "daemon-env.json"); got != want {
		t.Fatalf("daemon environment path = %s, want %s", got, want)
	}
}

func TestProjectFilesUseUnifiedDataDirectory(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	for _, goos := range []string{"darwin", "linux"} {
		manager := &Manager{goos: goos, home: home}
		if got, want := manager.daemonEnvPath(), filepath.Join(home, ".free-router", "daemon-env.json"); got != want {
			t.Fatalf("%s daemon environment path = %s, want %s", goos, got, want)
		}
		if got, want := manager.logPath(), filepath.Join(home, ".free-router", "free-router.log"); got != want {
			t.Fatalf("%s log path = %s, want %s", goos, got, want)
		}
	}
}

func TestParseLaunchdPID(t *testing.T) {
	if got := parseLaunchdPID("state = running\n\tpid = 1314\n"); got != 1314 {
		t.Fatalf("parseLaunchdPID() = %d", got)
	}
}

func TestInstallBinaryCopiesToStableUserPath(t *testing.T) {
	temporary := t.TempDir()
	source := filepath.Join(temporary, "downloaded-free-router")
	if err := os.WriteFile(source, []byte("test-binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{goos: "darwin", executable: source, home: filepath.Join(temporary, "home"), uid: "501"}
	installed, err := manager.installBinary()
	if err != nil {
		t.Fatal(err)
	}
	if installed != filepath.Join(manager.home, ".local", "bin", "free-router") {
		t.Fatalf("installed path = %s", installed)
	}
	content, err := os.ReadFile(installed)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "test-binary" {
		t.Fatalf("installed content = %q", content)
	}
	info, err := os.Stat(installed)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("installed permissions = %o", info.Mode().Perm())
	}
}
