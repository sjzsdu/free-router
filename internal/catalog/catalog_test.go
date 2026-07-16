package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/sjzsdu/free-router/internal/provider"
)

func TestRefreshKeepsOnlyZeroPricedModels(t *testing.T) {
	clearBuiltinKeys(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
			{"id":"paid/model","pricing":{"prompt":"0.1","completion":"0"}},
			{"id":"free/b","context_length":131072,"pricing":{"prompt":"0","completion":"0.000"},"supported_parameters":["tools","reasoning"],"architecture":{"input_modalities":["text","image"],"output_modalities":["text"]},"top_provider":{"max_completion_tokens":8192}},
			{"id":"free/a","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]}}
		]}`))
	}))
	defer server.Close()

	registry, err := provider.NewRegistry(`[{"id":"test","base_url":"` + server.URL + `","api_key":"test","filter":"zero-price"}]`)
	if err != nil {
		t.Fatal(err)
	}
	store := New(registry, filepath.Join(t.TempDir(), "models.json"), server.Client())
	if err := store.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	models := store.Models()
	if len(models) != 2 || models[0].ID != "test/free/a" || models[1].ID != "test/free/b" {
		t.Fatalf("unexpected models: %#v", models)
	}
	model := models[1]
	if model.Type != "normal" || model.ContextLength != 131072 || model.MaxOutputTokens != 8192 {
		t.Fatalf("unexpected normalized metadata: %#v", model)
	}
	if !model.Capabilities.ToolCall || !model.Capabilities.Reasoning || !model.Capabilities.Vision {
		t.Fatalf("unexpected capabilities: %#v", model.Capabilities)
	}
}

func TestClassifyModelIDs(t *testing.T) {
	tests := map[string]string{
		"openai/gpt-oss-20b":               "normal",
		"BAAI/bge-m3":                      "embedding",
		"BAAI/bge-reranker-v2-m3":          "rerank",
		"openai/whisper-large-v3":          "audio",
		"black-forest-labs/FLUX.1-schnell": "image",
		"Wan-AI/Wan2.2-T2V":                "video",
		"meta/llama-guard-3":               "moderation",
	}
	for id, expected := range tests {
		if actual := classifyID(id); actual != expected {
			t.Errorf("classifyID(%q)=%q, want %q", id, actual, expected)
		}
	}
}

func TestRefreshSupportsCloudflareStyleCatalog(t *testing.T) {
	clearBuiltinKeys(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"result":[{"id":"internal-uuid","name":"@cf/openai/gpt-oss-20b"}]}`))
	}))
	defer server.Close()

	registry, err := provider.NewRegistry(`[{"id":"cloudflare-test","base_url":"` + server.URL + `","api_key":"test","use_name_as_id":true}]`)
	if err != nil {
		t.Fatal(err)
	}
	store := New(registry, filepath.Join(t.TempDir(), "models.json"), server.Client())
	if err := store.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	models := store.Models()
	if len(models) != 1 || models[0].UpstreamID != "@cf/openai/gpt-oss-20b" {
		t.Fatalf("unexpected models: %#v", models)
	}
}

func clearBuiltinKeys(t *testing.T) {
	t.Helper()
	for _, key := range provider.SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
}
