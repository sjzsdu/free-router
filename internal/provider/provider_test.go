package provider

import (
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSiliconFlowDiscoversOnlyChatModels(t *testing.T) {
	for _, key := range SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
	t.Setenv("SILICONFLOW_API_KEY", "test")

	registry, err := NewRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	spec, ok := registry.Get("siliconflow")
	if !ok {
		t.Fatal("siliconflow provider was not enabled")
	}
	endpoint, err := url.Parse(spec.ModelsEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.Query().Get("type") != "text" || endpoint.Query().Get("sub_type") != "chat" {
		t.Fatalf("unexpected models endpoint: %s", endpoint)
	}
	if len(spec.AllowedModels) == 0 {
		t.Fatal("siliconflow must have an explicit free-model allowlist")
	}
}

func TestChineseFreeProvidersHaveExplicitFreeModelPolicies(t *testing.T) {
	manifest, err := loadFreeModelManifest("")
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]Spec)
	for _, spec := range applyFreeModelManifest(builtins(), manifest) {
		byID[spec.ID] = spec
	}
	for _, id := range []string{"bigmodel", "qianfan"} {
		spec, ok := byID[id]
		if !ok {
			t.Fatalf("missing Chinese provider %s", id)
		}
		if len(spec.AllowedModels) == 0 && len(spec.AllowedModelPatterns) == 0 {
			t.Fatalf("provider %s must restrict discovery to verified free models", id)
		}
	}
}

func TestExternalManifestReplacesEmbeddedEligibilityData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "free-models.json")
	content := `{"schema_version":1,"providers":{"groq":{"policy":"inventory","source_urls":["https://example.com/models"],"models":[{"id":"free-chat","functions":["chat","chat-tools"],"context_length":8192}]}}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, key := range SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
	t.Setenv("GROQ_API_KEY", "test")
	registry, err := NewRegistryWithManifest("", DefaultEnvMap(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	spec, ok := registry.Get("groq")
	if !ok || len(spec.DiscoveredModels) != 1 || spec.DiscoveredModels[0].ID != "free-chat" {
		t.Fatalf("external manifest was not applied: %#v, %v", spec, ok)
	}
}

func TestApplyManifestDoesNotMutateInputAndNormalizesPolicies(t *testing.T) {
	original := []Spec{
		{ID: "openrouter", Filter: FilterZeroPrice, AllowedModels: []string{"old"}},
		{ID: "groq", Filter: FilterZeroPrice, AllowedModels: []string{"old"}},
	}
	before := append([]Spec(nil), original...)
	before[0].AllowedModels = append([]string(nil), original[0].AllowedModels...)
	before[1].AllowedModels = append([]string(nil), original[1].AllowedModels...)
	manifest := FreeModelManifest{SchemaVersion: 1, GeneratedAt: "2026-07-21T00:00:00Z", Providers: map[string]FreeProviderCatalog{
		"groq": {Policy: "all-listed", FreeBasis: "free plan", BillingWarning: "rate limited"},
	}}
	applied := applyFreeModelManifest(original, manifest)
	if !reflect.DeepEqual(original, before) {
		t.Fatalf("input specs were mutated: before=%#v after=%#v", before, original)
	}
	if applied[0].DiscoveryPolicy != "zero-price" || applied[0].Filter != FilterZeroPrice {
		t.Fatalf("openrouter fallback was lost: %#v", applied[0])
	}
	if applied[1].DiscoveryPolicy != "all-listed" || applied[1].Filter != FilterAll || len(applied[1].AllowedModels) != 0 {
		t.Fatalf("all-listed was not normalized: %#v", applied[1])
	}
	if applied[1].BillingWarning != "rate limited" || applied[1].FreeBasis != "free plan" || applied[1].ManifestGeneratedAt == "" {
		t.Fatalf("manifest metadata was not applied: %#v", applied[1])
	}
}

func TestBuiltinStatusIncludesManifestPolicy(t *testing.T) {
	status := BuiltinStatusWithEnv(DefaultEnvMap())
	for _, item := range status {
		if item["id"] == "openrouter" {
			if item["discovery_policy"] != "zero-price" || item["free_basis"] == "" || item["manifest_generated_at"] == "" {
				t.Fatalf("manifest status is incomplete: %#v", item)
			}
			return
		}
	}
	t.Fatal("openrouter status not found")
}

func TestCreditProvidersExposeBillingWarnings(t *testing.T) {
	byID := make(map[string]Spec)
	for _, spec := range builtins() {
		byID[spec.ID] = spec
	}
	for _, id := range []string{"xiaomi-mimo", "dashscope", "volcengine-ark", "baichuan"} {
		spec, ok := byID[id]
		if !ok {
			t.Fatalf("missing credit provider %s", id)
		}
		if spec.FreeKind == "" || spec.BillingWarning == "" {
			t.Fatalf("credit provider %s must disclose its free kind and billing warning", id)
		}
	}
}

func TestBuiltinsHaveOfficialRegistrationURLs(t *testing.T) {
	for _, spec := range builtins() {
		parsed, err := url.Parse(spec.RegisterURL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
			t.Errorf("provider %s has invalid registration URL %q", spec.ID, spec.RegisterURL)
		}
	}
}

func TestProviderEndpointOverride(t *testing.T) {
	spec := Spec{BaseURL: "https://example.com/v1", Endpoints: map[string]string{"/embeddings": "https://embed.example.com/run"}}
	if got := spec.APIEndpoint("/embeddings"); got != "https://embed.example.com/run" {
		t.Fatalf("endpoint = %q", got)
	}
	if got := spec.APIEndpoint("/audio/speech"); got != "https://example.com/v1/audio/speech" {
		t.Fatalf("default endpoint = %q", got)
	}
}

func TestSavedCredentialEnablesProviderAndEnvironmentWins(t *testing.T) {
	for _, key := range SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
	resolver := func(id string) (string, bool) {
		return "saved-key", id == "groq"
	}
	registry, err := NewRegistry("", resolver)
	if err != nil {
		t.Fatal(err)
	}
	spec, ok := registry.Get("groq")
	if !ok || spec.APIKey != "saved-key" {
		t.Fatalf("saved credential not used: %#v, %v", spec, ok)
	}

	t.Setenv("GROQ_API_KEY", "environment-key")
	registry, err = NewRegistry("", resolver)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ = registry.Get("groq")
	if spec.APIKey != "environment-key" {
		t.Fatalf("environment must take precedence, got %q", spec.APIKey)
	}
}

func TestConfiguredEnvironmentAliasEnablesProvider(t *testing.T) {
	for _, key := range SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
	t.Setenv("MY_GEMINI_KEY", "mapped-key")
	registry, err := NewRegistryWithEnv("", EnvMap{"gemini": {"MY_GEMINI_KEY"}})
	if err != nil {
		t.Fatal(err)
	}
	spec, ok := registry.Get("gemini")
	if !ok || spec.APIKey != "mapped-key" {
		t.Fatalf("mapped environment did not enable provider: %#v, %v", spec, ok)
	}
	status := BuiltinStatusWithEnv(EnvMap{"gemini": {"MY_GEMINI_KEY"}})
	for _, item := range status {
		if item["id"] == "gemini" && item["matched_env"] != "MY_GEMINI_KEY" {
			t.Fatalf("matched environment = %#v", item["matched_env"])
		}
	}
}

func TestCustomEnvironmentAliasesPrecedeBuiltins(t *testing.T) {
	merged := MergeEnvMap(EnvMap{"gemini": {"MY_GEMINI_KEY", "GEMINI_API_KEY"}})
	want := []string{"MY_GEMINI_KEY", "GEMINI_API_KEY"}
	if got := merged["gemini"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("merged aliases = %#v", got)
	}
}
