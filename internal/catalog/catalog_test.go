package catalog

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjzsdu/free-router/internal/provider"
)

func TestFormulaDiscoveryKeepsOnlyZeroPricedModels(t *testing.T) {
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
	discoverForTest(t, store)
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
	for _, function := range []string{FunctionChat, FunctionChatTools, FunctionImageUnderstanding} {
		if !model.SupportsFunction(function) {
			t.Fatalf("model functions %v do not contain %q", model.Functions, function)
		}
	}
}

func TestDiscoveredModelsConvertWithoutProviderCatalogRequest(t *testing.T) {
	models := modelsFromDiscovery(provider.Spec{ID: "test", Tier: "free", DiscoveredModels: []provider.DiscoveredModel{{ID: "free-chat", Functions: []string{FunctionChatTools}, ContextLength: 32768, Pricing: provider.DiscoveredPricing{Prompt: "0", Completion: "0"}}}})
	if len(models) != 1 || models[0].UpstreamID != "free-chat" || !models[0].Capabilities.ToolCall || models[0].Pricing.Prompt != "0" {
		t.Fatalf("models=%#v", models)
	}
}

func TestLegacyCacheIsRejected(t *testing.T) {
	clearBuiltinKeys(t)
	registry, err := provider.NewRegistry(`[{"id":"test","base_url":"https://example.invalid/v1","no_auth":true,"filter":"zero-price"}]`)
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(t.TempDir(), "models.json")
	content := `[
		{"id":"test/free","provider":"test","upstream_id":"free","type":"normal","functions":["chat"],"pricing":{"prompt":"0","completion":"0"}},
		{"id":"test/paid","provider":"test","upstream_id":"paid","type":"normal","functions":["chat"],"pricing":{"prompt":"0.1","completion":"0"}}
	]`
	if err := os.WriteFile(cache, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	store := New(registry, cache, http.DefaultClient)
	if err := store.loadCache(); err == nil {
		t.Fatal("legacy provider-discovered cache was accepted")
	}
}

func TestUnverifiedInventoryPrunesCachedProviderModels(t *testing.T) {
	clearBuiltinKeys(t)
	t.Setenv("GROQ_API_KEY", "test")
	manifestPath := filepath.Join(t.TempDir(), "free-models.json")
	if err := os.WriteFile(manifestPath, []byte(`{"schema_version":1,"providers":{"groq":{"policy":"unverified","free_basis":"evidence expired"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := provider.NewRegistryWithManifest("", provider.DefaultEnvMap(), manifestPath, false)
	if err != nil {
		t.Fatal(err)
	}
	store := New(registry, filepath.Join(t.TempDir(), "models.json"), http.DefaultClient)
	store.set([]Model{{ID: "groq/stale", Provider: "groq", UpstreamID: "stale", Free: true}}, time.Now())
	if err := store.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.Models()) != 0 {
		t.Fatalf("unverified provider remained in cache: %#v", store.Models())
	}
}

func TestFailedModelStaysRemovedUntilFormulaManifestChanges(t *testing.T) {
	clearBuiltinKeys(t)
	t.Setenv("GROQ_API_KEY", "test")
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "free-models.json")
	cachePath := filepath.Join(dir, "models.json")
	writeManifest := func(generatedAt string) {
		t.Helper()
		content := `{"schema_version":1,"generated_at":"` + generatedAt + `","providers":{"groq":{"policy":"inventory","source_urls":["https://example.com/models"],"models":[{"id":"verified-chat","functions":["chat"]}]}}}`
		if err := os.WriteFile(manifestPath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	newStore := func() *Store {
		t.Helper()
		registry, err := provider.NewRegistryWithManifest("", provider.DefaultEnvMap(), manifestPath, false)
		if err != nil {
			t.Fatal(err)
		}
		return New(registry, cachePath, http.DefaultClient)
	}

	writeManifest("2026-07-21T00:00:00Z")
	store := newStore()
	if err := store.Bootstrap(context.Background()); err != nil || len(store.Models()) != 1 {
		t.Fatalf("bootstrap err=%v models=%#v", err, store.Models())
	}
	if err := store.RemoveModel("groq/verified-chat"); err != nil {
		t.Fatal(err)
	}
	store = newStore()
	if err := store.Bootstrap(context.Background()); err != nil || len(store.Models()) != 0 {
		t.Fatalf("failed model returned in same manifest: err=%v models=%#v", err, store.Models())
	}

	writeManifest("2026-07-22T00:00:00Z")
	store = newStore()
	if err := store.Bootstrap(context.Background()); err != nil || len(store.Models()) != 1 {
		t.Fatalf("new Formula manifest did not reintroduce model: err=%v models=%#v", err, store.Models())
	}
}

func TestMultimodalUnderstandingProbeUsesOpenAIContentParts(t *testing.T) {
	clearBuiltinKeys(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"vision-model","architecture":{"input_modalities":["text","image"],"output_modalities":["text"]}}]}`))
		case "/chat/completions":
			var input struct {
				Messages []struct {
					Content []struct {
						Type     string `json:"type"`
						ImageURL struct {
							URL string `json:"url"`
						} `json:"image_url"`
					} `json:"content"`
				} `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			if len(input.Messages) != 1 || len(input.Messages[0].Content) != 2 || !strings.HasPrefix(input.Messages[0].Content[1].ImageURL.URL, "data:image/png;base64,iVBOR") {
				t.Fatalf("invalid OpenAI multimodal payload: %#v", input)
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"white"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	registry, _ := provider.NewRegistry(`[{"id":"test","base_url":"` + server.URL + `","no_auth":true}]`)
	store := New(registry, filepath.Join(t.TempDir(), "models.json"), server.Client())
	discoverForTest(t, store)
	if _, err := store.ProbeModel(context.Background(), "test/vision-model", FunctionImageUnderstanding); err != nil {
		t.Fatal(err)
	}
}

func TestAllowedModelPatternsAreCaseInsensitive(t *testing.T) {
	if !allowed(nil, []string{"*flash*"}, "GLM-4.7-Flash") {
		t.Fatal("free Flash model should match")
	}
	if allowed(nil, []string{"*flash*"}, "glm-5") {
		t.Fatal("paid model should not match")
	}
	if !allowed([]string{"hunyuan-lite"}, nil, "HUNYUAN-LITE") {
		t.Fatal("exact allowlist should be case-insensitive")
	}
}

func TestProbeIncludesSafeUpstreamErrorDetail(t *testing.T) {
	clearBuiltinKeys(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"organization permission denied","type":"permissions_error"}}`))
	}))
	defer server.Close()
	registry, _ := provider.NewRegistry(`[{"id":"test","base_url":"` + server.URL + `","no_auth":true}]`)
	store := New(registry, filepath.Join(t.TempDir(), "models.json"), server.Client())
	_, err := store.Probe(context.Background(), "test")
	if err == nil || !strings.Contains(err.Error(), "organization permission denied") {
		t.Fatalf("error = %v", err)
	}
}

func TestRefreshProviderNeverContactsUpstream(t *testing.T) {
	clearBuiltinKeys(t)
	requestedCalls := 0
	requested := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestedCalls++
		_, _ = w.Write([]byte(`{"data":[{"id":"chat-model"}]}`))
	}))
	defer requested.Close()
	otherCalls := 0
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		otherCalls++
		_, _ = w.Write([]byte(`{"data":[{"id":"other-model"}]}`))
	}))
	defer other.Close()
	custom := `[{"id":"requested","base_url":"` + requested.URL + `","no_auth":true},{"id":"other","base_url":"` + other.URL + `","no_auth":true}]`
	registry, _ := provider.NewRegistry(custom)
	store := New(registry, filepath.Join(t.TempDir(), "models.json"), requested.Client())
	if err := store.RefreshProvider(context.Background(), "requested"); err != nil {
		t.Fatal(err)
	}
	if requestedCalls != 0 || otherCalls != 0 || len(store.Models()) != 0 {
		t.Fatalf("requested_calls=%d other_calls=%d models=%d", requestedCalls, otherCalls, len(store.Models()))
	}
}

func TestAudioProbeUsesEmbeddedWAVMultipart(t *testing.T) {
	clearBuiltinKeys(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"whisper-test","type":"audio","supported_endpoints":["/audio/transcriptions"]}]}`))
		case "/audio/transcriptions":
			if err := r.ParseMultipartForm(32 << 10); err != nil {
				t.Fatal(err)
			}
			if r.FormValue("model") != "whisper-test" {
				t.Fatalf("model=%q", r.FormValue("model"))
			}
			file, _, err := r.FormFile("file")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			header := make([]byte, 4)
			if _, err := io.ReadFull(file, header); err != nil || string(header) != "RIFF" {
				t.Fatalf("invalid WAV header %q: %v", header, err)
			}
			_, _ = w.Write([]byte(`{"text":""}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	registry, _ := provider.NewRegistry(`[{"id":"test","base_url":"` + server.URL + `","no_auth":true}]`)
	store := New(registry, filepath.Join(t.TempDir(), "models.json"), server.Client())
	discoverForTest(t, store)
	if _, err := store.ProbeModel(context.Background(), "test/whisper-test", FunctionSpeechToText); err != nil {
		t.Fatal(err)
	}
}

func TestImageEditProbeUsesEmbeddedPNGMultipart(t *testing.T) {
	clearBuiltinKeys(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"image-edit-test","type":"image","supported_endpoints":["/images/edits"]}]}`))
		case "/images/edits":
			if err := r.ParseMultipartForm(32 << 10); err != nil {
				t.Fatal(err)
			}
			file, _, err := r.FormFile("image")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			header := make([]byte, 8)
			if _, err := io.ReadFull(file, header); err != nil || string(header) != "\x89PNG\r\n\x1a\n" {
				t.Fatalf("invalid PNG header %q: %v", header, err)
			}
			_, _ = w.Write([]byte(`{"data":[{"url":"https://example.invalid/probe.png"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	registry, _ := provider.NewRegistry(`[{"id":"test","base_url":"` + server.URL + `","no_auth":true}]`)
	store := New(registry, filepath.Join(t.TempDir(), "models.json"), server.Client())
	discoverForTest(t, store)
	if _, err := store.ProbeModel(context.Background(), "test/image-edit-test", FunctionImageGeneration); err != nil {
		t.Fatal(err)
	}
}

func TestImageToVideoProbeIncludesEmbeddedPNG(t *testing.T) {
	clearBuiltinKeys(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"tiny-i2v","type":"video"}]}`))
		case "/videos/generations":
			var input map[string]any
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			image, _ := input["image"].(string)
			if !strings.HasPrefix(image, "data:image/png;base64,iVBOR") {
				t.Fatalf("missing embedded image: %#v", input)
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	registry, _ := provider.NewRegistry(`[{"id":"test","base_url":"` + server.URL + `","no_auth":true}]`)
	store := New(registry, filepath.Join(t.TempDir(), "models.json"), server.Client())
	discoverForTest(t, store)
	if _, err := store.ProbeModel(context.Background(), "test/tiny-i2v", FunctionVideoGeneration); err != nil {
		t.Fatal(err)
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

func TestFormulaDiscoverySupportsCloudflareStyleCatalog(t *testing.T) {
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
	discoverForTest(t, store)
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

func discoverForTest(t *testing.T, store *Store) {
	t.Helper()
	models, failures := store.DiscoverFromProviders(context.Background())
	if len(failures) > 0 || len(models) == 0 {
		t.Fatalf("models=%d failures=%#v", len(models), failures)
	}
}
