package routing

import (
	"path/filepath"
	"reflect"
	"testing"
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
