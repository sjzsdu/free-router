package service

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const (
	launchdLabel = "io.github.sjzsdu.free-router"
	systemdUnit  = "free-router.service"
)

type Status struct {
	Supported bool   `json:"supported"`
	Installed bool   `json:"installed"`
	Running   bool   `json:"running"`
	Manager   string `json:"manager"`
	Binary    string `json:"binary"`
	PID       int    `json:"pid,omitempty"`
	Message   string `json:"message,omitempty"`
}

type Manager struct {
	goos       string
	executable string
	home       string
	uid        string
}

func New() (*Manager, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("find free-router executable: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil {
		executable = resolved
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("find user home: %w", err)
	}
	uid := "0"
	if current, userErr := user.Current(); userErr == nil && current.Uid != "" {
		uid = current.Uid
	}
	return &Manager{goos: runtime.GOOS, executable: executable, home: home, uid: uid}, nil
}

func (m *Manager) Install(ctx context.Context) error {
	if m.goos != "darwin" && m.goos != "linux" {
		return m.unsupported()
	}
	installed, err := m.installBinary()
	if err != nil {
		return err
	}
	m.executable = installed
	switch m.goos {
	case "darwin":
		return m.installLaunchd(ctx)
	case "linux":
		return m.installSystemd(ctx)
	}
	return nil
}

func (m *Manager) BinaryPath() string { return filepath.Join(m.home, ".local", "bin", "free-router") }

func (m *Manager) Start(ctx context.Context) error {
	switch m.goos {
	case "darwin":
		if _, err := os.Stat(m.launchdPath()); err != nil {
			return errors.New("service is not installed; run free-router service install")
		}
		_ = m.run(ctx, "launchctl", "enable", m.launchdTarget())
		if err := m.run(ctx, "launchctl", "bootstrap", m.launchdDomain(), m.launchdPath()); err != nil {
			status, statusErr := m.Status(ctx)
			if statusErr != nil || status.Message == "stopped" {
				return err
			}
		}
		return m.run(ctx, "launchctl", "kickstart", "-k", m.launchdTarget())
	case "linux":
		return m.run(ctx, "systemctl", "--user", "start", systemdUnit)
	default:
		return m.unsupported()
	}
}

func (m *Manager) Stop(ctx context.Context) error {
	switch m.goos {
	case "darwin":
		status, err := m.Status(ctx)
		if err == nil && status.Message == "stopped" {
			return nil
		}
		return m.run(ctx, "launchctl", "bootout", m.launchdTarget())
	case "linux":
		return m.run(ctx, "systemctl", "--user", "stop", systemdUnit)
	default:
		return m.unsupported()
	}
}

func (m *Manager) Restart(ctx context.Context) error {
	switch m.goos {
	case "darwin":
		if status, _ := m.Status(ctx); status.Message != "stopped" {
			return m.run(ctx, "launchctl", "kickstart", "-k", m.launchdTarget())
		}
		return m.Start(ctx)
	case "linux":
		return m.run(ctx, "systemctl", "--user", "restart", systemdUnit)
	default:
		return m.unsupported()
	}
}

func (m *Manager) Uninstall(ctx context.Context) error {
	switch m.goos {
	case "darwin":
		_ = m.run(ctx, "launchctl", "bootout", m.launchdTarget())
		_ = m.run(ctx, "launchctl", "disable", m.launchdTarget())
		if err := os.Remove(m.launchdPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove LaunchAgent: %w", err)
		}
		return nil
	case "linux":
		_ = m.run(ctx, "systemctl", "--user", "disable", "--now", systemdUnit)
		if err := os.Remove(m.systemdPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove systemd unit: %w", err)
		}
		return m.run(ctx, "systemctl", "--user", "daemon-reload")
	default:
		return m.unsupported()
	}
}

func (m *Manager) Status(ctx context.Context) (Status, error) {
	switch m.goos {
	case "darwin":
		status := Status{Supported: true, Installed: fileExists(m.launchdPath()), Manager: "launchd", Binary: m.BinaryPath()}
		output, err := m.output(ctx, "launchctl", "print", m.launchdTarget())
		if err != nil {
			status.Message = "stopped"
			return status, nil
		}
		status.Running = strings.Contains(output, "state = running")
		status.PID = parseLaunchdPID(output)
		if status.Running {
			status.Message = "running"
		} else {
			status.Message = "loaded"
		}
		return status, nil
	case "linux":
		status := Status{Supported: true, Installed: fileExists(m.systemdPath()), Manager: "systemd", Binary: m.BinaryPath()}
		output, err := m.output(ctx, "systemctl", "--user", "show", systemdUnit, "--property=ActiveState,MainPID", "--value")
		if err != nil {
			status.Message = "stopped"
			return status, nil
		}
		for _, line := range strings.Fields(output) {
			if line == "active" {
				status.Running = true
			} else if pid, parseErr := strconv.Atoi(line); parseErr == nil && pid > 0 {
				status.PID = pid
			}
		}
		status.Message = strings.TrimSpace(output)
		return status, nil
	default:
		return Status{Manager: "unsupported", Binary: m.BinaryPath(), Message: "service management is supported on macOS and Linux"}, nil
	}
}

func (m *Manager) Logs(ctx context.Context, output io.Writer, follow bool, lines int) error {
	if lines < 1 {
		lines = 100
	}
	var command *exec.Cmd
	switch m.goos {
	case "darwin":
		args := []string{"-n", strconv.Itoa(lines)}
		if follow {
			args = append(args, "-f")
		}
		args = append(args, m.logPath())
		command = exec.CommandContext(ctx, "tail", args...)
	case "linux":
		args := []string{"--user", "-u", systemdUnit, "-n", strconv.Itoa(lines), "--no-pager"}
		if follow {
			args = append(args, "-f")
		}
		command = exec.CommandContext(ctx, "journalctl", args...)
	default:
		return m.unsupported()
	}
	command.Stdout, command.Stderr = output, output
	return command.Run()
}

func (m *Manager) installLaunchd(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(m.launchdPath()), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.logPath()), 0o755); err != nil {
		return err
	}
	content := launchdPlist(m.executable, m.logPath())
	if err := os.WriteFile(m.launchdPath(), []byte(content), 0o644); err != nil {
		return fmt.Errorf("write LaunchAgent: %w", err)
	}
	_ = m.run(ctx, "launchctl", "bootout", m.launchdTarget())
	_ = m.run(ctx, "launchctl", "enable", m.launchdTarget())
	if err := m.run(ctx, "launchctl", "bootstrap", m.launchdDomain(), m.launchdPath()); err != nil {
		return err
	}
	return m.run(ctx, "launchctl", "kickstart", "-k", m.launchdTarget())
}

func (m *Manager) installSystemd(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(m.systemdPath()), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(m.systemdPath(), []byte(systemdService(m.executable)), 0o644); err != nil {
		return fmt.Errorf("write systemd unit: %w", err)
	}
	if err := m.run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	return m.run(ctx, "systemctl", "--user", "enable", "--now", systemdUnit)
}

func (m *Manager) installBinary() (string, error) {
	if strings.Contains(m.executable, string(filepath.Separator)+"go-build") {
		return "", errors.New("cannot install a temporary `go run` executable; build or download free-router first")
	}
	target := m.BinaryPath()
	if filepath.Clean(m.executable) == filepath.Clean(target) {
		if err := os.Chmod(target, 0o755); err != nil {
			return "", fmt.Errorf("make installed binary executable: %w", err)
		}
		return target, nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", fmt.Errorf("create binary directory: %w", err)
	}
	source, err := os.Open(m.executable)
	if err != nil {
		return "", fmt.Errorf("open current executable: %w", err)
	}
	defer source.Close()
	temporary, err := os.CreateTemp(filepath.Dir(target), ".free-router-*")
	if err != nil {
		return "", fmt.Errorf("create temporary binary: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o755); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("set binary permissions: %w", err)
	}
	if _, err := io.Copy(temporary, source); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("copy binary: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("sync binary: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close binary: %w", err)
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		return "", fmt.Errorf("install binary: %w", err)
	}
	return target, nil
}

func (m *Manager) run(ctx context.Context, name string, args ...string) error {
	output, err := m.output(ctx, name, args...)
	if err != nil {
		return fmt.Errorf("%s failed: %v: %s", name, err, strings.TrimSpace(output))
	}
	return nil
}

func (m *Manager) output(ctx context.Context, name string, args ...string) (string, error) {
	var output bytes.Buffer
	command := exec.CommandContext(ctx, name, args...)
	command.Stdout, command.Stderr = &output, &output
	err := command.Run()
	return output.String(), err
}

func (m *Manager) launchdPath() string {
	return filepath.Join(m.home, "Library", "LaunchAgents", launchdLabel+".plist")
}
func (m *Manager) logPath() string {
	return filepath.Join(m.home, "Library", "Logs", "free-router.log")
}
func (m *Manager) launchdDomain() string { return "gui/" + m.uid }
func (m *Manager) launchdTarget() string { return m.launchdDomain() + "/" + launchdLabel }
func (m *Manager) systemdPath() string {
	return filepath.Join(m.home, ".config", "systemd", "user", systemdUnit)
}

func (m *Manager) unsupported() error {
	return fmt.Errorf("service management is not supported on %s; use macOS LaunchAgent or Linux systemd user services", m.goos)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func parseLaunchdPID(output string) int {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 3 && fields[0] == "pid" && fields[1] == "=" {
			pid, _ := strconv.Atoi(fields[2])
			return pid
		}
	}
	return 0
}

func launchdPlist(executable, logPath string) string {
	escape := func(value string) string {
		var output strings.Builder
		_ = xml.EscapeText(&output, []byte(value))
		return output.String()
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key><array><string>%s</string><string>serve</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>EnvironmentVariables</key><dict><key>FREE_ROUTER_SERVICE_MANAGER</key><string>launchd</string></dict>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, launchdLabel, escape(executable), escape(logPath), escape(logPath))
}

func systemdService(executable string) string {
	quoted := strconv.Quote(executable)
	return fmt.Sprintf(`[Unit]
Description=Free Router model gateway
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s serve
Environment=FREE_ROUTER_SERVICE_MANAGER=systemd
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, quoted)
}
