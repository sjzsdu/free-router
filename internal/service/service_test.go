package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLaunchdPlistEscapesPathsAndSetsManager(t *testing.T) {
	content := launchdPlist("/tmp/free & router", "/tmp/router <log>.txt")
	for _, expected := range []string{"/tmp/free &amp; router", "/tmp/router &lt;log&gt;.txt", "FREE_ROUTER_SERVICE_MANAGER", "launchd"} {
		if !strings.Contains(content, expected) {
			t.Fatalf("plist does not contain %q: %s", expected, content)
		}
	}
}

func TestSystemdServiceQuotesExecutable(t *testing.T) {
	content := systemdService(`/opt/free router/free-router`)
	if !strings.Contains(content, `ExecStart="/opt/free router/free-router" serve`) {
		t.Fatalf("unexpected unit: %s", content)
	}
	if !strings.Contains(content, "Restart=on-failure") {
		t.Fatalf("unit should restart after failures: %s", content)
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
