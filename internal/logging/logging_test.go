package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	tests := map[string]slog.Level{
		"":        slog.LevelInfo,
		"info":    slog.LevelInfo,
		" DEBUG ": slog.LevelDebug,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
	}
	for input, want := range tests {
		got, err := ParseLevel(input)
		if err != nil {
			t.Fatalf("ParseLevel(%q) error: %v", input, err)
		}
		if got != want {
			t.Fatalf("ParseLevel(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestParseLevelRejectsUnknown(t *testing.T) {
	if _, err := ParseLevel("verbose"); err == nil {
		t.Fatal("unknown level should be rejected instead of silently defaulting")
	}
}

func TestNewWritesTextAtConfiguredLevel(t *testing.T) {
	buffer := &bytes.Buffer{}
	logger, err := New(Options{Level: "warn", Format: "text", Writer: buffer})
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("hidden")
	logger.Warn("visible", "provider", "groq")
	output := buffer.String()
	if strings.Contains(output, "hidden") {
		t.Fatalf("info line was not filtered at warn level: %s", output)
	}
	if !strings.Contains(output, "msg=visible") || !strings.Contains(output, "provider=groq") {
		t.Fatalf("warn line missing: %s", output)
	}
}

func TestNewWritesJSON(t *testing.T) {
	buffer := &bytes.Buffer{}
	logger, err := New(Options{Level: "debug", Format: "json", Writer: buffer})
	if err != nil {
		t.Fatal(err)
	}
	logger.Debug("attempt", "model", "chat-a")
	output := buffer.String()
	if !strings.Contains(output, `"msg":"attempt"`) || !strings.Contains(output, `"model":"chat-a"`) {
		t.Fatalf("json line missing fields: %s", output)
	}
}

func TestNewRejectsUnknownFormat(t *testing.T) {
	if _, err := New(Options{Format: "logfmt"}); err == nil {
		t.Fatal("unknown format should be rejected")
	}
}

func TestSetupInstallsDefault(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	if _, err := Setup(Options{Level: "error"}); err != nil {
		t.Fatal(err)
	}
	if slog.Default().Enabled(t.Context(), slog.LevelWarn) {
		t.Fatal("default logger should reject warn after setup at error level")
	}
}
