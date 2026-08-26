package service

import (
	"context"
	"errors"
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

func TestLaunchdTrayPlistRunsInteractiveMenuBar(t *testing.T) {
	content := launchdTrayPlist("/tmp/free & router", "/tmp/tray <log>.txt", "/tmp/daemon & env.json")
	for _, expected := range []string{"io.github.sjzsdu.free-router.tray", "<string>tray</string>", "Interactive", "Aqua", "SuccessfulExit", "/tmp/free &amp; router", "/tmp/tray &lt;log&gt;.txt"} {
		if !strings.Contains(content, expected) {
			t.Fatalf("tray plist does not contain %q: %s", expected, content)
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
		if got, want := manager.trayLogPath(), filepath.Join(home, ".free-router", "free-router-tray.log"); got != want {
			t.Fatalf("%s tray log path = %s, want %s", goos, got, want)
		}
	}
}

func TestParseLaunchdPID(t *testing.T) {
	if got := parseLaunchdPID("state = running\n\tpid = 1314\n"); got != 1314 {
		t.Fatalf("parseLaunchdPID() = %d", got)
	}
}

func TestInstallBinaryCopiesToStableUserPath(t *testing.T) {
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", "")
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

func TestBinaryPathUsesDefaultGoInstallPath(t *testing.T) {
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", "")
	home := filepath.Join(t.TempDir(), "home")
	installed := filepath.Join(home, "go", "bin", "free-router")
	manager := &Manager{home: home, executable: installed}

	if got := manager.BinaryPath(); got != installed {
		t.Fatalf("BinaryPath() = %s, want %s", got, installed)
	}
}

func TestBinaryPathUsesExplicitGoBin(t *testing.T) {
	t.Setenv("GOBIN", filepath.Join(t.TempDir(), "custom-bin"))
	t.Setenv("GOPATH", "")
	installed := filepath.Join(os.Getenv("GOBIN"), "free-router")
	manager := &Manager{home: filepath.Join(t.TempDir(), "home"), executable: installed}

	if got := manager.BinaryPath(); got != installed {
		t.Fatalf("BinaryPath() = %s, want %s", got, installed)
	}
}

func TestInstallBinaryKeepsMakeInstalledExecutable(t *testing.T) {
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", "")
	home := filepath.Join(t.TempDir(), "home")
	installed := filepath.Join(home, "go", "bin", "free-router")
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, []byte("make-installed"), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{goos: "darwin", executable: installed, home: home, uid: "501"}

	got, err := manager.installBinary()
	if err != nil {
		t.Fatal(err)
	}
	if got != installed {
		t.Fatalf("installBinary() = %s, want %s", got, installed)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "free-router")); !os.IsNotExist(err) {
		t.Fatalf("installBinary should not copy make-installed binary to ~/.local/bin, stat err = %v", err)
	}
	info, err := os.Stat(installed)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("installed permissions = %o", info.Mode().Perm())
	}
}

func fakeManager(t *testing.T, calls *[]string, respond func(string, []string) (string, error)) *Manager {
	t.Helper()
	manager := &Manager{goos: "darwin", home: filepath.Join(t.TempDir(), "home"), uid: "501", command: func(_ context.Context, name string, args ...string) ([]byte, error) {
		*calls = append(*calls, name+" "+strings.Join(args, " "))
		joined := name + " " + strings.Join(args, " ")
		if respond != nil {
			output, err := respond(name, args)
			if err != nil {
				return nil, err
			}
			return []byte(output), err
		}
		return []byte(joined), nil
	}}
	plistPath := manager.launchdPath()
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plistPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestStartRunsLaunchctlSequence(t *testing.T) {
	var calls []string
	manager := fakeManager(t, &calls, nil)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(calls, "|")
	for _, expected := range []string{"launchctl enable", "launchctl bootstrap", "launchctl kickstart -k"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("Start did not run %q: %v", expected, calls)
		}
	}
}

func TestStopRunsLaunchctlBootout(t *testing.T) {
	var calls []string
	manager := fakeManager(t, &calls, nil)
	manager.command = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		// Status must report running, otherwise Stop short-circuits.
		if name == "launchctl" && len(args) > 0 && args[0] == "print" {
			return []byte("state = running\npid = 1"), nil
		}
		return []byte(""), nil
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(calls, "|")
	if !strings.Contains(joined, "launchctl bootout") {
		t.Fatalf("Stop should run launchctl bootout, got %v", calls)
	}
}

func TestStatusParsesLaunchdStateAndPID(t *testing.T) {
	var calls []string
	manager := fakeManager(t, &calls, func(_ string, _ []string) (string, error) {
		return "state = running\npid = 4242", nil
	})
	status, err := manager.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Installed || !status.Running {
		t.Fatalf("status = %+v, want installed+running", status)
	}
	if status.PID != 4242 {
		t.Fatalf("PID = %d, want 4242", status.PID)
	}
	if status.Message != "running" {
		t.Fatalf("message = %q, want running", status.Message)
	}
}

func TestStatusReportsStoppedWhenLaunchctlFails(t *testing.T) {
	manager := fakeManager(t, &[]string{}, func(_ string, _ []string) (string, error) {
		return "", errors.New("could not find service")
	})
	status, err := manager.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Running || status.Message != "stopped" {
		t.Fatalf("status = %+v, want stopped", status)
	}
}

func TestUninstallRemovesLaunchAgentsAndEnv(t *testing.T) {
	var calls []string
	manager := fakeManager(t, &calls, nil)
	envPath := manager.daemonEnvPath()
	if err := os.MkdirAll(filepath.Dir(envPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	trayPath := manager.trayLaunchdPath()
	if err := os.WriteFile(trayPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := manager.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(envPath); !os.IsNotExist(err) {
		t.Fatalf("daemon env should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(trayPath); !os.IsNotExist(err) {
		t.Fatalf("tray LaunchAgent should be removed, stat err = %v", err)
	}
	joined := strings.Join(calls, "|")
	if !strings.Contains(joined, "launchctl bootout") {
		t.Fatalf("Uninstall should boot out launchd targets, got %v", calls)
	}
}

func TestInstallRunsFullSequenceAndCopiesBinary(t *testing.T) {
	var calls []string
	executable := filepath.Join(t.TempDir(), "free-router")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\necho ok"), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{goos: "darwin", executable: executable, home: filepath.Join(t.TempDir(), "home"), uid: "501", command: func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return []byte("state = running"), nil
	}}
	if err := manager.Install(context.Background(), map[string]string{"GEMINI_KEY": "secret"}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(calls, "|")
	for _, expected := range []string{"launchctl enable", "launchctl bootstrap", "launchctl kickstart -k"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("Install did not run %q: %v", expected, calls)
		}
	}
	bin := manager.BinaryPath()
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("binary not installed at %s: %v", bin, err)
	}
	env, err := os.ReadFile(manager.daemonEnvPath())
	if err != nil || !strings.Contains(string(env), "GEMINI_KEY") {
		t.Fatalf("daemon env not written: %v %s", err, env)
	}
}
