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
	if len(spec.DiscoveredModels) == 0 && spec.DiscoveryStatus != "verification-failed" {
		t.Fatalf("siliconflow should either load Formula models or surface verification-failed, got status=%q", spec.DiscoveryStatus)
	}
}

func TestFormulaCatalogIncludesProvidersWithoutCredentials(t *testing.T) {
	for _, key := range SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
	manifest, err := loadFreeModelManifest("")
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]Spec)
	for _, spec := range applyFreeModelManifest(builtins(), manifest) {
		byID[spec.ID] = spec
	}
	for _, id := range []string{"bigmodel", "qianfan", "openrouter"} {
		_, ok := byID[id]
		if !ok {
			t.Fatalf("missing Chinese provider %s", id)
		}
	}
	registry, err := NewRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.All()) != 0 || len(registry.CatalogAll()) != len(builtins()) {
		t.Fatalf("configured=%d catalog=%d", len(registry.All()), len(registry.CatalogAll()))
	}
}

func TestExternalManifestReplacesEmbeddedEligibilityData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "free-models.json")
	content := `{"schema_version":2,"providers":{"groq":{"source_urls":["https://example.com/models"],"models":[{"id":"free-chat","functions":["chat","chat-tools"],"context_length":8192}]}}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, key := range SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
	t.Setenv("GROQ_API_KEY", "test")
	registry, err := NewRegistryWithManifest("", DefaultEnvMap(), path)
	if err != nil {
		t.Fatal(err)
	}
	spec, ok := registry.Get("groq")
	if !ok || len(spec.DiscoveredModels) != 1 || spec.DiscoveredModels[0].ID != "free-chat" {
		t.Fatalf("external manifest was not applied: %#v, %v", spec, ok)
	}
}

func TestExternalManifestAppliesToCustomProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "free-models.json")
	content := `{"schema_version":2,"providers":{"custom":{"source_urls":["https://example.com/models"],"models":[{"id":"free-chat","functions":["chat"]}]}}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistryWithManifest(`[{"id":"custom","base_url":"https://example.com/v1","no_auth":true}]`, DefaultEnvMap(), path)
	if err != nil {
		t.Fatal(err)
	}
	spec, ok := registry.CatalogGet("custom")
	if !ok || len(spec.DiscoveredModels) != 1 || spec.DiscoveredModels[0].ID != "free-chat" {
		t.Fatalf("external manifest was not joined to custom provider: %#v, %v", spec, ok)
	}
}

func TestManifestRejectsUnknownFieldsAndTrailingJSON(t *testing.T) {
	for name, content := range map[string]string{
		"unknown top-level field": `{"schema_version":2,"providers":{},"research-new-providers":{}}`,
		"legacy policy field":     `{"schema_version":2,"providers":{"groq":{"policy":"inventory","models":[]}}}`,
		"unknown model field":     `{"schema_version":2,"providers":{"groq":{"source_urls":["https://example.com"],"models":[{"id":"free","functions":["chat"],"invented":true}]}}}`,
		"trailing JSON":           `{"schema_version":2,"providers":{}} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "free-models.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadFreeModelManifest(path); err == nil {
				t.Fatal("invalid manifest was accepted")
			}
		})
	}
}

func TestManifestPricingMustBeNonNegativeNumericStrings(t *testing.T) {
	valid := []string{"", "0", "0.000", "1", "1.25", "1e-6"}
	for _, value := range valid {
		if err := validateManifestPrice(value); err != nil {
			t.Errorf("valid price %q rejected: %v", value, err)
		}
	}
	invalid := []string{"-1", "+1", "NaN", "Inf", "$0", "free", " 0", "0 ", "1/2"}
	for _, value := range invalid {
		if err := validateManifestPrice(value); err == nil {
			t.Errorf("invalid price %q accepted", value)
		}
	}
}

func TestBuiltinStatusWithManifestTreatsVerificationFailedProvidersAsEmptyCatalog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "free-models.json")
	content := `{"schema_version":2,"generated_at":"test","providers":{"gemini":{"source_urls":["https://example.com/pricing"],"models":[{"id":"models/gemini-test","functions":["chat"]}],"discovery_status":"verification-failed","discovery_message":"agent could not identify free models from official catalog; provider abandoned"}}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	status := BuiltinStatusWithManifest(DefaultEnvMap(), path)
	for _, item := range status {
		if item["id"] == "gemini" {
			if item["catalog_status"] != "empty" {
				t.Fatalf("verification-failed provider must not be routable: %#v", item)
			}
			if item["formula_model_count"] != 0 {
				t.Fatalf("verification-failed provider must expose zero formula models: %#v", item)
			}
			if item["discovery_status"] != "verification-failed" {
				t.Fatalf("unexpected discovery status: %#v", item)
			}
			return
		}
	}
	t.Fatal("gemini status not found")
}

func TestManifestDiscoveryStatusIsValidatedAndExposed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "free-models.json")
	content := `{"schema_version":2,"generated_at":"test","providers":{"groq":{"models":[],"discovery_status":"discovery-failed","discovery_message":"official endpoint returned 403"}}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	status := BuiltinStatusWithManifest(DefaultEnvMap(), path)
	for _, item := range status {
		if item["id"] == "groq" {
			if item["discovery_status"] != "discovery-failed" || item["discovery_message"] != "official endpoint returned 403" {
				t.Fatalf("discovery reason not exposed: %#v", item)
			}
			return
		}
	}
	t.Fatal("groq status not found")
}

func TestManifestRejectsUnknownDiscoveryStatus(t *testing.T) {
	manifest := FreeModelManifest{SchemaVersion: 2, Providers: map[string]FreeProviderCatalog{
		"groq": {Models: []DiscoveredModel{}, DiscoveryStatus: "maybe-free"},
	}}
	if err := ValidateFreeModelManifest(manifest); err == nil {
		t.Fatal("unknown discovery status was accepted")
	}
}

func TestCatalogAllIsSorted(t *testing.T) {
	registry := &Registry{catalog: map[string]Spec{
		"zeta": {ID: "zeta"}, "alpha": {ID: "alpha"}, "middle": {ID: "middle"},
	}}
	got := registry.CatalogAll()
	if len(got) != 3 || got[0].ID != "alpha" || got[1].ID != "middle" || got[2].ID != "zeta" {
		t.Fatalf("CatalogAll order = %#v", got)
	}
}

func TestApplyManifestDoesNotMutateInput(t *testing.T) {
	original := []Spec{{ID: "openrouter"}, {ID: "groq"}}
	before := append([]Spec(nil), original...)
	manifest := FreeModelManifest{SchemaVersion: 2, GeneratedAt: "2026-07-21T00:00:00Z", Providers: map[string]FreeProviderCatalog{
		"groq": {FreeBasis: "free plan", BillingWarning: "rate limited", Models: []DiscoveredModel{{ID: "free", Functions: []string{"chat"}}}},
	}}
	applied := applyFreeModelManifest(original, manifest)
	if !reflect.DeepEqual(original, before) {
		t.Fatalf("input specs were mutated: before=%#v after=%#v", before, original)
	}
	if len(applied[0].DiscoveredModels) != 0 || len(applied[1].DiscoveredModels) != 1 {
		t.Fatalf("manifest models were not applied: %#v", applied)
	}
	if applied[1].BillingWarning != "rate limited" || applied[1].FreeBasis != "free plan" || applied[1].ManifestGeneratedAt == "" {
		t.Fatalf("manifest metadata was not applied: %#v", applied[1])
	}
}

func TestBuiltinStatusIncludesFormulaCatalog(t *testing.T) {
	status := BuiltinStatusWithEnv(DefaultEnvMap())
	for _, item := range status {
		if item["id"] == "openrouter" {
			if item["catalog_status"] != "ready" || item["formula_model_count"].(int) == 0 || item["free_basis"] == "" || item["manifest_generated_at"] == "" {
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
	expected := map[string]string{
		"mistral":        "https://console.mistral.ai/home?profile_dialog=api-keys",
		"dashscope":      "https://bailian.console.aliyun.com/cn-beijing/?tab=app#/api-key",
		"volcengine-ark": "https://console.volcengine.com/ark/region:ark+cn-beijing/apikey",
		"baichuan":       "https://platform.baichuan-ai.com/homePage",
		"cloudflare":     "https://dash.cloudflare.com/?to=%2F%3Aaccount%2Fai%2Fworkers-ai",
	}
	for _, spec := range builtins() {
		parsed, err := url.Parse(spec.RegisterURL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
			t.Errorf("provider %s has invalid registration URL %q", spec.ID, spec.RegisterURL)
		}
		if want, ok := expected[spec.ID]; ok && spec.RegisterURL != want {
			t.Errorf("provider %s registration URL = %q, want %q", spec.ID, spec.RegisterURL, want)
		}
		if spec.ID == "baichuan" && spec.RegisterLabel != "申请接入" {
			t.Errorf("baichuan registration label = %q, want 申请接入", spec.RegisterLabel)
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
