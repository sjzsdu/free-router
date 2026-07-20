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
	config.Models["provider/model"] = ModelOverride{Functions: []string{catalog.FunctionChat, catalog.FunctionChatTools}, ToolCall: &toolCall}
	config.Models["provider/disabled"] = ModelOverride{Disabled: true}
	if err := store.Update(config); err != nil {
		t.Fatal(err)
	}
	model, enabled := store.Apply(catalog.Model{ID: "provider/model", Type: "embedding", Functions: []string{catalog.FunctionEmbedding}})
	if !enabled || !reflect.DeepEqual(model.Functions, []string{catalog.FunctionChat, catalog.FunctionChatTools}) || !model.Capabilities.ToolCall || !model.Capabilities.ToolCallKnown {
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
	if config.Version != CurrentVersion || config.Routes["chat"].Capability != "chat" || config.Routes["chat-tools"].Capability != "chat-tools" || !reflect.DeepEqual(config.Models["provider/model"].Functions, []string{"chat"}) {
		t.Fatalf("migrated config = %#v", config)
	}
	persisted, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(persisted), `"version": 6`) {
		t.Fatalf("migration was not persisted: %s, %v", persisted, err)
	}
}

func TestVersionFiveMediaRoutesMigrateToExplicitCapabilities(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	content := []byte(`{"version":5,"routes":{"image":{"type":"image","models":["p/image"]},"video":{"type":"video","models":["p/video"]},"audio":{"type":"audio","models":["p/audio"]}},"models":{}}`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	config := store.Config()
	if _, exists := config.Routes["image"]; exists {
		t.Fatal("ambiguous image route survived migration")
	}
	if !reflect.DeepEqual(config.Routes[catalog.FunctionImageGeneration].Models, []string{"p/image"}) || !reflect.DeepEqual(config.Routes[catalog.FunctionVideoGeneration].Models, []string{"p/video"}) {
		t.Fatalf("media generation routes were not migrated: %#v", config.Routes)
	}
	if !reflect.DeepEqual(config.Routes[catalog.FunctionSpeechToText].Models, []string{"p/audio"}) || !reflect.DeepEqual(config.Routes[catalog.FunctionTextToSpeech].Models, []string{"p/audio"}) {
		t.Fatalf("audio route was not split: %#v", config.Routes)
	}
}

func TestRouteStrategyDefaultsAndValidation(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	config := store.Config()
	route := config.Routes["chat"]
	if route.Strategy != StrategyOrdered {
		t.Fatalf("default strategy = %q", route.Strategy)
	}
	route.Strategy = StrategyRoundRobin
	config.Routes["chat"] = route
	if err := store.Update(config); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Route("chat"); got.Strategy != StrategyRoundRobin {
		t.Fatalf("saved strategy = %q", got.Strategy)
	}
	route.Strategy = "random"
	config.Routes["chat"] = route
	if err := store.Update(config); err == nil {
		t.Fatal("invalid strategy was accepted")
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
