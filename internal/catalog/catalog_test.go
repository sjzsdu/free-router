package catalog

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestFormulaDiscoveryReturnsRawProviderCatalog(t *testing.T) {
	clearBuiltinKeys(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
			{"id":"paid/model","pricing":{"prompt":"0.1","completion":"0"}},
			{"id":"free/b","context_length":131072,"pricing":{"prompt":"0","completion":"0.000"},"supported_parameters":["tools","reasoning"],"architecture":{"input_modalities":["text","image"],"output_modalities":["text"]},"top_provider":{"max_completion_tokens":8192}},
			{"id":"free/a","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]}}
		]}`))
	}))
	defer server.Close()

	registry, err := provider.NewRegistry(`[{"id":"test","base_url":"` + server.URL + `","api_key":"test"}]`)
	if err != nil {
		t.Fatal(err)
	}
	store := New(registry, filepath.Join(t.TempDir(), "models.json"), server.Client())
	discoverForTest(t, store)
	models := store.Models()
	if len(models) != 3 || models[0].ID != "test/free/a" || models[1].ID != "test/free/b" || models[2].ID != "test/paid/model" {
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

func TestOfficialCatalogZeroPricePolicyHandlesScalarAndTieredPricing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
			{"id":"paid","pricing":{"prompt":"0.1","completion":"0"}},
			{"id":"free-scalar","pricing":{"prompt":"0","completion":0}},
			{"id":"free-tiered","pricing":{"prompt":[{"price":"0"},{"price":"0.000"}],"completion":[{"price":"0"}]}},
			{"id":"paid-tiered","pricing":{"prompt":[{"price":"0"},{"price":"0.01"}],"completion":[{"price":"0"}]}},
			{"id":"paid-per-unit","description":"30 second clips are priced at $0.04 per clip.","pricing":{"prompt":"0","completion":"0"}},
			{"id":"unsupported-audio","type":"audio","pricing":{"prompt":"0","completion":"0"}}
		]}`))
	}))
	defer server.Close()
	registry, err := provider.NewRegistry(`[{"id":"test","base_url":"` + server.URL + `","no_auth":true}]`)
	if err != nil {
		t.Fatal(err)
	}
	store := New(registry, "", server.Client())
	models, failures := store.DiscoverFromSpecs(context.Background(), []provider.Spec{{ID: "test", BaseURL: server.URL, NoAuth: true, FreeModelPolicy: "zero-price"}})
	if len(failures) != 0 {
		t.Fatalf("failures=%#v", failures)
	}
	if len(models) != 2 || models[0].UpstreamID != "free-scalar" || models[1].UpstreamID != "free-tiered" {
		t.Fatalf("models=%#v", models)
	}
}

func TestExplicitUnitPriceDetection(t *testing.T) {
	for _, description := range []string{
		"30 second clips are priced at $0.04 per clip.",
		"Image generation costs US $1 per request.",
	} {
		if !explicitUnitPrice.MatchString(description) {
			t.Errorf("explicit unit price not detected in %q", description)
		}
	}
	for _, description := range []string{"free tier", "pricing is $0", "zero token price"} {
		if explicitUnitPrice.MatchString(description) {
			t.Errorf("false explicit unit price match in %q", description)
		}
	}
}

func TestModelFingerprintUsesStableRoutingMetadata(t *testing.T) {
	base := Model{ID: "test/model", Type: "normal", Functions: []string{"chat-tools", "chat"}, ContextLength: 8192, SupportedParameters: []string{"tools", "stream"}, Pricing: Pricing{Prompt: "0", Completion: "0"}}
	cosmetic := base
	cosmetic.Name = "renamed"
	cosmetic.Description = "new description"
	cosmetic.Created = 12345
	cosmetic.Functions = []string{"chat", "chat-tools"}
	cosmetic.SupportedParameters = []string{"stream", "tools"}
	if modelFingerprint(base) != modelFingerprint(cosmetic) {
		t.Fatal("cosmetic metadata or slice ordering changed the model fingerprint")
	}
	changed := base
	changed.ContextLength = 16384
	if modelFingerprint(base) == modelFingerprint(changed) {
		t.Fatal("routing metadata change did not change the model fingerprint")
	}
}

func TestCapabilityVerificationPersistsAndInvalidatesWithModelFingerprint(t *testing.T) {
	clearBuiltinKeys(t)
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "free-models.json")
	cachePath := filepath.Join(dir, "models.json")
	writeManifest := func(contextLength int) {
		t.Helper()
		content := fmt.Sprintf(`{"schema_version":2,"generated_at":"test","providers":{"test":{"source_urls":["https://example.com/models"],"models":[{"id":"chat-a","functions":["chat","chat-tools"],"context_length":%d}]}}}`, contextLength)
		if err := os.WriteFile(manifestPath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	newStore := func() *Store {
		t.Helper()
		registry, err := provider.NewRegistryWithManifest(
			`[{"id":"test","base_url":"https://example.invalid","no_auth":true}]`,
			provider.DefaultEnvMap(), manifestPath,
		)
		if err != nil {
			t.Fatal(err)
		}
		return New(registry, cachePath, http.DefaultClient)
	}

	writeManifest(8192)
	store := newStore()
	if err := store.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkedAt := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	if err := store.RecordCapabilityVerification("test/chat-a", FunctionChatTools, checkedAt, 25*time.Millisecond); err != nil {
		t.Fatal(err)
	}

	reloaded := newStore()
	if err := reloaded.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reloaded.CapabilityVerified("test/chat-a", FunctionChatTools) {
		t.Fatal("successful capability verification was not restored from cache")
	}
	verifications := reloaded.CapabilityVerifications()
	if len(verifications) != 1 || !verifications[0].CheckedAt.Equal(checkedAt) || verifications[0].LatencyMS != 25 {
		t.Fatalf("verifications=%#v", verifications)
	}

	writeManifest(16384)
	changed := newStore()
	if err := changed.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if changed.CapabilityVerified("test/chat-a", FunctionChatTools) || len(changed.CapabilityVerifications()) != 0 {
		t.Fatal("routing metadata change retained stale capability verification")
	}
}

func TestResetCapabilityVerificationRequiresAProbeAgain(t *testing.T) {
	clearBuiltinKeys(t)
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "free-models.json")
	if err := os.WriteFile(manifestPath, []byte(`{"schema_version":2,"providers":{"test":{"source_urls":["https://example.com/models"],"models":[{"id":"chat-a","functions":["chat"]}]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := provider.NewRegistryWithManifest(
		`[{"id":"test","base_url":"https://example.invalid","no_auth":true}]`,
		provider.DefaultEnvMap(), manifestPath,
	)
	if err != nil {
		t.Fatal(err)
	}
	store := New(registry, filepath.Join(dir, "models.json"), http.DefaultClient)
	if err := store.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordCapabilityVerification("test/chat-a", FunctionChat, time.Now(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := store.ResetCapabilityVerification("test/chat-a", FunctionChat); err != nil {
		t.Fatal(err)
	}
	if store.CapabilityVerified("test/chat-a", FunctionChat) {
		t.Fatal("reset capability remained verified")
	}
}

func TestApplyQuarantineDoesNotMutateQuarantineState(t *testing.T) {
	old := Model{ID: "test/model", Type: "normal", Functions: []string{"chat"}, ContextLength: 8192}
	changed := old
	changed.ContextLength = 16384
	store := &Store{quarantine: map[string]string{old.ID: modelFingerprint(old)}}
	if got := store.applyQuarantine([]Model{old, changed}); len(got) != 1 || got[0].ContextLength != changed.ContextLength {
		t.Fatalf("filtered models = %#v", got)
	}
	if got := store.quarantine[old.ID]; got != modelFingerprint(old) {
		t.Fatalf("applyQuarantine mutated quarantine state: %q", got)
	}
}

func TestOfficialAudioTextEndpointIsTextToSpeech(t *testing.T) {
	functions := inferFunctions(upstreamModel{ID: "universal-3-pro", Type: "audio", SupportedEndpoints: []string{"/audio/{text}"}}, "audio", []string{"audio"}, []string{"text"}, nil)
	if len(functions) != 1 || functions[0] != FunctionTextToSpeech {
		t.Fatalf("functions=%#v", functions)
	}
}

func TestDiscoveredModelsConvertWithoutProviderCatalogRequest(t *testing.T) {
	models := modelsFromDiscovery(provider.Spec{ID: "test", Tier: "free", DiscoveredModels: []provider.DiscoveredModel{{ID: "free-chat", Functions: []string{FunctionChatTools}, ContextLength: 32768, Pricing: provider.DiscoveredPricing{Prompt: "0", Completion: "0"}}}})
	if len(models) != 1 || models[0].UpstreamID != "free-chat" || !models[0].Capabilities.ToolCall || !models[0].Capabilities.ToolCallKnown || models[0].Pricing.Prompt != "0" {
		t.Fatalf("models=%#v", models)
	}
}

func TestDiscoveredToolCapabilityUsesMaintainedModelContract(t *testing.T) {
	models := modelsFromDiscovery(provider.Spec{ID: "test", DiscoveredModels: []provider.DiscoveredModel{
		{ID: "unknown", Functions: []string{FunctionChat}},
		{ID: "unsupported", Functions: []string{FunctionChat}, SupportedParameters: []string{"stream"}},
		{ID: "supported", Functions: []string{FunctionChat}, SupportedParameters: []string{"stream", "tools"}},
	}})
	if len(models) != 3 {
		t.Fatalf("models=%#v", models)
	}
	byID := make(map[string]Model, len(models))
	for _, model := range models {
		byID[model.UpstreamID] = model
	}
	if !byID["unknown"].Capabilities.ToolCallKnown || !byID["unknown"].Capabilities.ToolCall || !byID["unknown"].SupportsFunction(FunctionChatTools) {
		t.Fatalf("maintained Chat model did not inherit complete capabilities: %#v", byID["unknown"])
	}
	if !byID["unsupported"].Capabilities.ToolCallKnown || byID["unsupported"].Capabilities.ToolCall {
		t.Fatalf("explicit parameter inventory was not treated as unsupported: %#v", byID["unsupported"].Capabilities)
	}
	if byID["unsupported"].SupportsFunction(FunctionChatTools) {
		t.Fatalf("explicitly unsupported model became a tool candidate: %#v", byID["unsupported"])
	}
	if !byID["supported"].Capabilities.ToolCallKnown || !byID["supported"].Capabilities.ToolCall || !byID["supported"].SupportsFunction(FunctionChatTools) {
		t.Fatalf("tools parameter was not treated as supported: %#v", byID["supported"])
	}
}

func TestToolProbeRequiresStructuredToolCall(t *testing.T) {
	clearBuiltinKeys(t)
	var response = `{"choices":[{"message":{"content":"ping"}}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat/completions" {
			_, _ = w.Write([]byte(response))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	registry, _ := provider.NewRegistry(`[{"id":"test","base_url":"` + server.URL + `","no_auth":true}]`)
	store := New(registry, filepath.Join(t.TempDir(), "models.json"), server.Client())
	store.set([]Model{{ID: "test/chat", Provider: "test", UpstreamID: "chat", Type: "normal", Functions: []string{FunctionChat}}}, time.Now())
	if _, err := store.ProbeModel(context.Background(), "test/chat", FunctionChatTools); err == nil {
		t.Fatal("plain text response was accepted as tool-call support")
	}
	response = `{"choices":[{"message":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"ping","arguments":"{}"}}]}}]}`
	if _, err := store.ProbeModel(context.Background(), "test/chat", FunctionChatTools); err != nil {
		t.Fatalf("structured tool call was rejected: %v", err)
	}
}

func TestMaintainedToolSupportSurvivesRefreshAndCacheReload(t *testing.T) {
	clearBuiltinKeys(t)
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "free-models.json")
	cachePath := filepath.Join(dir, "models.json")
	manifest := `{"schema_version":2,"generated_at":"test","providers":{"test":{"source_urls":["https://example.com/models"],"models":[{"id":"chat-a","functions":["chat"]}]}}}`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := provider.NewRegistryWithManifest(
		`[{"id":"test","base_url":"https://example.invalid","no_auth":true}]`,
		provider.DefaultEnvMap(), manifestPath,
	)
	if err != nil {
		t.Fatal(err)
	}
	store := New(registry, cachePath, http.DefaultClient)
	if err := store.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	model, ok := store.Find("test/chat-a")
	if !ok || !model.Capabilities.ToolCallKnown || !model.Capabilities.ToolCall || !model.SupportsFunction(FunctionChatTools) {
		t.Fatalf("initial model should inherit complete tool support: %#v", model)
	}
	if err := store.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	model, _ = store.Find("test/chat-a")
	if !model.Capabilities.ToolCall || !model.SupportsFunction(FunctionChatTools) {
		t.Fatalf("refresh discarded maintained tool support: %#v", model)
	}
	reloaded := New(registry, cachePath, http.DefaultClient)
	if err := reloaded.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	model, _ = reloaded.Find("test/chat-a")
	if !model.Capabilities.ToolCallKnown || !model.Capabilities.ToolCall || !model.SupportsFunction(FunctionChatTools) {
		t.Fatalf("cache reload discarded maintained tool support: %#v", model)
	}
}

func TestFormulaModelsLoadWithoutProviderCredentials(t *testing.T) {
	clearBuiltinKeys(t)
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "free-models.json")
	manifest := `{"schema_version":2,"generated_at":"test","providers":{"groq":{"source_urls":["https://example.com/models"],"models":[{"id":"free-chat","functions":["chat"]}]}}}`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := provider.NewRegistryWithManifest("", provider.DefaultEnvMap(), manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	store := New(registry, filepath.Join(dir, "models.json"), http.DefaultClient)
	if err := store.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.Models()) != 1 {
		t.Fatalf("Formula models = %#v", store.Models())
	}
	if _, callable := store.Find("groq/free-chat"); callable {
		t.Fatal("model without Provider credentials became callable")
	}
}

func TestLegacyCacheIsRejected(t *testing.T) {
	clearBuiltinKeys(t)
	registry, err := provider.NewRegistry(`[{"id":"test","base_url":"https://example.invalid/v1","no_auth":true}]`)
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

func TestEmptyFormulaCatalogPrunesCachedProviderModels(t *testing.T) {
	clearBuiltinKeys(t)
	t.Setenv("GROQ_API_KEY", "test")
	manifestPath := filepath.Join(t.TempDir(), "free-models.json")
	if err := os.WriteFile(manifestPath, []byte(`{"schema_version":2,"providers":{"groq":{"free_basis":"no verified models","models":[]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := provider.NewRegistryWithManifest("", provider.DefaultEnvMap(), manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	store := New(registry, filepath.Join(t.TempDir(), "models.json"), http.DefaultClient)
	store.set([]Model{{ID: "groq/stale", Provider: "groq", UpstreamID: "stale", Free: true}}, time.Now())
	if err := store.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.Models()) != 0 {
		t.Fatalf("empty Formula provider remained in cache: %#v", store.Models())
	}
}

func TestFailedModelStaysRemovedAcrossUnrelatedFormulaManifestChanges(t *testing.T) {
	clearBuiltinKeys(t)
	t.Setenv("GROQ_API_KEY", "test")
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "free-models.json")
	cachePath := filepath.Join(dir, "models.json")
	writeManifest := func(generatedAt string, contextLength int) {
		t.Helper()
		content := fmt.Sprintf(`{"schema_version":2,"generated_at":%q,"providers":{"groq":{"source_urls":["https://example.com/models"],"models":[{"id":"verified-chat","functions":["chat"],"context_length":%d}]}}}`, generatedAt, contextLength)
		if err := os.WriteFile(manifestPath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	newStore := func() *Store {
		t.Helper()
		registry, err := provider.NewRegistryWithManifest("", provider.DefaultEnvMap(), manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		return New(registry, cachePath, http.DefaultClient)
	}

	writeManifest("2026-07-21T00:00:00Z", 8192)
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

	writeManifest("2026-07-22T00:00:00Z", 8192)
	store = newStore()
	if err := store.Bootstrap(context.Background()); err != nil || len(store.Models()) != 0 {
		t.Fatalf("unchanged failed model returned after unrelated Formula update: err=%v models=%#v", err, store.Models())
	}
	if err := store.RestoreModel("groq/verified-chat"); err != nil || len(store.Models()) != 1 {
		t.Fatalf("explicit reset did not restore quarantined model: err=%v models=%#v", err, store.Models())
	}
	if err := store.RemoveModel("groq/verified-chat"); err != nil {
		t.Fatal(err)
	}

	writeManifest("2026-07-23T00:00:00Z", 16384)
	store = newStore()
	if err := store.Bootstrap(context.Background()); err != nil || len(store.Models()) != 1 {
		t.Fatalf("changed model metadata did not make model eligible for retest: err=%v models=%#v", err, store.Models())
	}
	if _, ok := store.quarantine["groq/verified-chat"]; !ok {
		t.Fatal("read-only quarantine filtering discarded the previous failure record")
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
