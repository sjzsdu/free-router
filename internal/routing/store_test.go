package routing

import (
	"path/filepath"
	"reflect"
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
	config.Models["provider/model"] = ModelOverride{Type: "normal", ToolCall: &toolCall}
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
