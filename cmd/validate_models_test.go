package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseEnvLine(t *testing.T) {
	cases := []struct {
		line    string
		wantKey string
		wantVal string
		wantOK  bool
	}{
		{"KEY=value", "KEY", "value", true},
		{"KEY=value with spaces", "KEY", "value with spaces", true},
		{"KEY=\"quoted value\"", "KEY", "quoted value", true},
		{"KEY='single quoted'", "KEY", "single quoted", true},
		{"  KEY = value  ", "KEY", "value", true},
		{"KEY=", "KEY", "", true},
		{"=value", "", "", false},
		{"NOEQUALS", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		key, val, ok := parseEnvLine(c.line)
		if ok != c.wantOK || key != c.wantKey || val != c.wantVal {
			t.Errorf("parseEnvLine(%q) = (%q,%q,%v), want (%q,%q,%v)", c.line, key, val, ok, c.wantKey, c.wantVal, c.wantOK)
		}
	}
}

func TestLoadEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "# comment\n\nFOO=bar\nBAZ=\"quoted value\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// Pre-set an existing var that must not be overridden.
	t.Setenv("PRE", "keep")
	if err := loadEnvFile(path); err != nil {
		t.Fatalf("loadEnvFile: %v", err)
	}
	if os.Getenv("FOO") != "bar" {
		t.Errorf("FOO = %q, want bar", os.Getenv("FOO"))
	}
	if os.Getenv("BAZ") != "quoted value" {
		t.Errorf("BAZ = %q, want \"quoted value\"", os.Getenv("BAZ"))
	}
	if os.Getenv("PRE") != "keep" {
		t.Errorf("PRE = %q, want keep (existing vars must not be overridden)", os.Getenv("PRE"))
	}
}

func TestLoadEnvFileInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("BADLINE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadEnvFile(path); err == nil {
		t.Fatal("expected error for invalid line")
	}
}

func TestHTTPErrorCategory(t *testing.T) {
	cases := map[int]string{
		401: "authentication",
		403: "authentication",
		402: "quota",
		429: "rate-limit",
		404: "unavailable",
		410: "unavailable",
		500: "upstream",
		400: "http",
		0:   "unknown",
	}
	for status, want := range cases {
		if got := httpErrorCategory(status); got != want {
			t.Errorf("httpErrorCategory(%d) = %q, want %q", status, got, want)
		}
	}
}

func TestSortedKeys(t *testing.T) {
	got := sortedKeys(map[string]bool{"b": true, "a": true, "c": true})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sortedKeys = %v, want %v", got, want)
		}
	}
}
