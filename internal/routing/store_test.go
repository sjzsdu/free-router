package routing

import (
	"errors"
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

func TestStoreRejectsStaleConcurrentUpdate(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	first := store.Config()
	stale := store.Config()
	first.Comment = "first"
	if err := store.Update(first); err != nil {
		t.Fatal(err)
	}
	stale.Comment = "stale"
	if err := store.Update(stale); !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("stale update error=%v, want conflict", err)
	}
	if got := store.Config().Comment; got != "first" {
		t.Fatalf("stale update changed config to %q", got)
	}
}

func TestHeartbeatConfigurationPersistsAndDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if settings := store.Config().Heartbeat.Resolve(); settings.IntervalMinutes != DefaultHeartbeatIntervalMinutes || settings.RecoveryMinutes != DefaultHeartbeatRecoveryMinutes || !settings.Enabled {
		t.Fatalf("defaults = %#v", settings)
	}
	enabled := false
	interval, recovery, probes, concurrency := 45, 20, 4, 1
	config := store.Config()
	config.Heartbeat = &HeartbeatConfig{Enabled: &enabled, IntervalMinutes: &interval, RecoveryMinutes: &recovery, MaxProbesPerRound: &probes, Concurrency: &concurrency}
	if err := store.Update(config); err != nil {
		t.Fatal(err)
	}
	reloaded, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	settings := reloaded.Config().Heartbeat.Resolve()
	if settings.Enabled || settings.IntervalMinutes != 45 || settings.RecoveryMinutes != 20 || settings.MaxProbesPerRound != 4 || settings.Concurrency != 1 {
		t.Fatalf("reloaded heartbeat = %#v", settings)
	}
}

// The cold-start chat-tools priority defaults to on and can be turned off
// explicitly, including across a reload.
func TestHeartbeatPriorityDefaultsOnAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if settings := store.Config().Heartbeat.Resolve(); !settings.PrioritizeChatTools {
		t.Fatal("prioritize_chat_tools must default to true")
	}

	disabled := false
	config := store.Config()
	config.Heartbeat = &HeartbeatConfig{PrioritizeChatTools: &disabled}
	if err := store.Update(config); err != nil {
		t.Fatal(err)
	}
	reloaded, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if settings := reloaded.Config().Heartbeat.Resolve(); settings.PrioritizeChatTools {
		t.Fatal("prioritize_chat_tools=false was not persisted")
	}

	// An untouched configuration keeps tracking future default changes instead
	// of freezing today's value.
	untouched := DefaultConfig()
	if settings := untouched.Heartbeat.Resolve(); !settings.PrioritizeChatTools {
		t.Fatal("unset heartbeat lost the default priority")
	}
}

func TestHeartbeatConfigurationRejectsUnsafeValues(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	tooSmall, tooMany, zero, zeroRecovery := 0, MaxHeartbeatProbesPerRound+1, 0, 0
	cases := map[string]*HeartbeatConfig{
		"concurrency zero":     {Concurrency: &tooSmall},
		"probe budget too big": {MaxProbesPerRound: &tooMany},
		"both clocks zero":     {IntervalMinutes: &zero, RecoveryMinutes: &zeroRecovery},
	}
	for name, heartbeat := range cases {
		config := store.Config()
		config.Heartbeat = heartbeat
		if err := store.Update(config); err == nil {
			t.Fatalf("%s: unsafe heartbeat configuration was accepted", name)
		}
	}
}
