package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/sjzsdu/free-router/internal/provider"
	"github.com/sjzsdu/free-router/internal/routing"
)

func TestDocumentedDefaultConfigUsesCodeDefaults(t *testing.T) {
	content, err := documentedDefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	var generated struct {
		Comment     string                           `json:"_comment"`
		Help        map[string]string                `json:"_help"`
		Version     int                              `json:"version"`
		ProviderEnv map[string][]string              `json:"provider_env"`
		Routes      map[string]routing.Route         `json:"routes"`
		Models      map[string]routing.ModelOverride `json:"models"`
	}
	if err := json.Unmarshal(content, &generated); err != nil {
		t.Fatal(err)
	}
	defaults := routing.DefaultConfig()
	if generated.Comment == "" || len(generated.Help) != 4 {
		t.Fatalf("documentation missing: %#v", generated.Help)
	}
	for alias, route := range generated.Routes {
		route.Comment = ""
		generated.Routes[alias] = route
	}
	if generated.Version != defaults.Version || !reflect.DeepEqual(generated.Routes, defaults.Routes) || !reflect.DeepEqual(generated.Models, defaults.Models) {
		t.Fatalf("generated defaults differ from code defaults: %#v", generated)
	}
	if !reflect.DeepEqual(provider.EnvMap(generated.ProviderEnv), provider.DefaultEnvMap()) {
		t.Fatalf("provider mappings differ: %#v", generated.ProviderEnv)
	}
}

func TestDocumentedConfigLoadsAndPreservesHelp(t *testing.T) {
	content, err := documentedDefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := routing.New(path)
	if err != nil {
		t.Fatal(err)
	}
	config := store.Config()
	if config.Comment == "" || len(config.Help) != 4 || config.Routes["chat"].Comment == "" {
		t.Fatalf("documentation was not loaded: %#v", config)
	}
	if err := store.Update(config); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(saved) || !containsAll(string(saved), `"_comment"`, `"_help"`, "通用文本对话") {
		t.Fatalf("documentation was not preserved: %s", saved)
	}
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}

func TestWriteOnboardConfigProtectsExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	first := []byte("first\n")
	if err := writeOnboardConfig(path, first, false); err != nil {
		t.Fatal(err)
	}
	if err := writeOnboardConfig(path, []byte("second\n"), false); err == nil {
		t.Fatal("expected existing configuration to be protected")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != string(first) {
		t.Fatalf("existing content changed: %q", content)
	}
	if err := writeOnboardConfig(path, []byte("second\n"), true); err != nil {
		t.Fatal(err)
	}
	content, _ = os.ReadFile(path)
	if string(content) != "second\n" {
		t.Fatalf("force content = %q", content)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %o", info.Mode().Perm())
		}
	}
}
