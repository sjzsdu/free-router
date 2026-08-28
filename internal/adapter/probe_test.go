package adapter

import (
	"context"
	"net/http"
	"testing"

	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/provider"
)

// Ensure the registry satisfies the catalog probe interface so probes route
// through the same wire protocol the gateway uses.
var _ catalog.ProbeRequestBuilder = (*Registry)(nil)

func TestRegistryBuildProbeRequestUsesResolvedAdapter(t *testing.T) {
	registry := NewRegistry()
	model := catalog.Model{ID: "test/chat", Provider: "openai-like", UpstreamID: "chat"}
	spec := provider.Spec{ID: "openai-like", BaseURL: "https://example.test/v1", APIKey: "k"}

	req, err := registry.BuildProbeRequest(context.Background(), model, spec, http.MethodPost, "/chat/completions", "application/json", catalog.FunctionChat, []byte(`{"model":"chat"}`))
	if err != nil {
		t.Fatalf("BuildProbeRequest: %v", err)
	}
	if req.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", req.Method)
	}
	if got := req.URL.String(); got != "https://example.test/v1/chat/completions" {
		t.Errorf("url = %q, want https://example.test/v1/chat/completions", got)
	}
	if req.Header.Get("Authorization") == "" {
		t.Errorf("auth header not applied by adapter")
	}
	if req.Body == nil {
		t.Errorf("request body not set")
	}
}
