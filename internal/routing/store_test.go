package routing

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sjzsdu/free-router/internal/catalog"
)

func TestStorePersistsFallbackOrderAndMergesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "free-router.json")
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	config := store.Config()
	route := config.Routes["chat-tools"]
	route.Models = []string{"groq/a", "openrouter/b", "groq/a"}
	config.Routes["chat-tools"] = route
	if err := store.Update(config); err != nil {
		t.Fatal(err)
	}

	reloaded, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.Route("chat-tools")
	if !ok || !reflect.DeepEqual(got.Models, []string{"groq/a", "openrouter/b"}) {
		t.Fatalf("fallback order = %#v, %v", got.Models, ok)
	}
	if _, ok := reloaded.Route("embedding"); !ok {
		t.Fatal("default routes were not merged")
	}
}

func TestModelOverridesDisableAndCorrectCapabilities(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	config := store.Config()
	toolCall := true
	config.Models["provider/model"] = ModelOverride{Type: "chat", ToolCall: &toolCall}
	config.Models["provider/disabled"] = ModelOverride{Disabled: true}
	if err := store.Update(config); err != nil {
		t.Fatal(err)
	}
	model, enabled := store.Apply(catalog.Model{ID: "provider/model", Type: "embedding"})
	if !enabled || model.Type != "normal" || !model.Capabilities.ToolCall || !model.Capabilities.ToolCallKnown {
		t.Fatalf("overridden model = %#v, enabled=%v", model, enabled)
	}
	if _, enabled := store.Apply(catalog.Model{ID: "provider/disabled"}); enabled {
		t.Fatal("disabled model remained enabled")
	}
}

func TestVersionTwoNormalRoutesMigrateToUserFacingTypes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	content := []byte(`{"version":2,"routes":{"chat":{"type":"normal","models":[]},"chat-tools":{"type":"normal","require_tool":true,"models":[]}},"models":{"provider/model":{"type":"normal"}}}`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	config := store.Config()
	if config.Version != CurrentVersion || config.Routes["chat"].Type != "chat" || config.Routes["chat-tools"].Type != "chat-tools" || config.Models["provider/model"].Type != "chat" {
		t.Fatalf("migrated config = %#v", config)
	}
	persisted, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(persisted), `"version": 4`) {
		t.Fatalf("migration was not persisted: %s, %v", persisted, err)
	}
}

func TestProviderEnvironmentMappingPersistsAndDeduplicates(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	config := store.Config()
	config.ProviderEnv["gemini"] = []string{" MY_GEMINI_KEY ", "GEMINI_API_KEY", "MY_GEMINI_KEY"}
	if err := store.Update(config); err != nil {
		t.Fatal(err)
	}
	reloaded, err := New(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Config().ProviderEnv["gemini"]; !reflect.DeepEqual(got, []string{"MY_GEMINI_KEY", "GEMINI_API_KEY"}) {
		t.Fatalf("provider env = %#v", got)
	}
}

func TestProviderEnvironmentMappingRejectsInvalidNames(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	config := store.Config()
	config.ProviderEnv["gemini"] = []string{"NOT-AN-ENV"}
	if err := store.Update(config); err == nil {
		t.Fatal("invalid environment variable was accepted")
	}
}
