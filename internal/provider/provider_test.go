package provider

import (
	"net/url"
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
