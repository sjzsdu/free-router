package adapter

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/provider"
)

func TestRegistryResolvesDeclaredAdapterKind(t *testing.T) {
	registry := NewRegistry()
	custom := &CloudflareAdapter{}
	registry.Register("custom-wire", custom)
	if got := registry.Resolve(provider.Spec{ID: "new-provider", Adapter: "custom-wire"}); got != custom {
		t.Fatalf("Resolve returned %T, want registered adapter", got)
	}
	if got := registry.Resolve(provider.Spec{ID: "cloudflare"}); got.Name() != "cloudflare-workers-ai" {
		t.Fatalf("provider-id compatibility fallback returned %q", got.Name())
	}
}

func TestClassifyError(t *testing.T) {
	adapter := &OpenAICompatibleAdapter{}

	tests := []struct {
		name       string
		statusCode int
		body       []byte
		wantMsg    string
		wantRetry  bool
		wantRate   bool
	}{
		{"500 server error", 500, nil, "server error", true, false},
		{"503 server error", 503, nil, "server error", true, false},
		{"429 rate limit", 429, nil, "rate limited", true, true},
		{"408 timeout", 408, nil, "request timeout", true, false},
		{"400 client error", 400, []byte(`{"error":{"message":"bad request"}}`), "bad request", false, false},
		{"401 auth error", 401, []byte(`{"error":{"message":"unauthorized"}}`), "unauthorized", true, false},
		{"402 quota exhausted", 402, []byte(`{"error":{"message":"quota exhausted"}}`), "quota exhausted", true, false},
		{"403 account disabled", 403, []byte(`{"error":{"message":"forbidden"}}`), "forbidden", true, false},
		{"404 model removed", 404, []byte(`{"error":{"message":"missing model"}}`), "missing model", true, false},
		{"400 error without message", 400, []byte(`{}`), "HTTP 400", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := adapter.ClassifyError(tt.statusCode, tt.body)
			if err.Message != tt.wantMsg {
				t.Errorf("Message = %q, want %q", err.Message, tt.wantMsg)
			}
			if err.Retryable != tt.wantRetry {
				t.Errorf("Retryable = %v, want %v", err.Retryable, tt.wantRetry)
			}
			if err.RateLimit != tt.wantRate {
				t.Errorf("RateLimit = %v, want %v", err.RateLimit, tt.wantRate)
			}
		})
	}
}

func TestNormalizeResponseClassifiesErrorAndPreservesBody(t *testing.T) {
	adapter := &OpenAICompatibleAdapter{}
	response := &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"invalid model"}}`)),
	}
	normalized, err := adapter.NormalizeResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Error == nil || normalized.Error.StatusCode != http.StatusBadRequest || normalized.Error.Message != "invalid model" {
		t.Fatalf("normalized=%#v", normalized)
	}
	preserved, err := io.ReadAll(response.Body)
	if err != nil || string(preserved) != `{"error":{"message":"invalid model"}}` {
		t.Fatalf("preserved body=%q err=%v", preserved, err)
	}
}

func TestCloudflareAdapterHandlesProviderErrorEnvelope(t *testing.T) {
	registry := NewRegistry()
	cloudflare := registry.Get("cloudflare")
	if cloudflare.Name() != "cloudflare-workers-ai" {
		t.Fatalf("adapter=%q", cloudflare.Name())
	}
	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"success":false,"errors":[{"message":"daily neuron limit reached"}]}`)),
	}
	normalized, err := cloudflare.NormalizeResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Error == nil || normalized.Error.Message != "daily neuron limit reached" || !normalized.Error.RateLimit || !normalized.Error.Retryable {
		t.Fatalf("normalized=%#v", normalized)
	}
}

func TestNormalizeResponseLeavesStreamingBodyUntouched(t *testing.T) {
	adapter := &OpenAICompatibleAdapter{}
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader("data: ok\n\n")),
	}
	normalized, err := adapter.NormalizeResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if !normalized.Stream || normalized.Error != nil {
		t.Fatalf("normalized=%#v", normalized)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "data: ok\n\n" {
		t.Fatalf("stream body=%q err=%v", body, err)
	}
}

func TestCapabilities(t *testing.T) {
	adapter := &OpenAICompatibleAdapter{}
	model := catalog.Model{
		ID:           "test/model",
		Capabilities: catalog.Capabilities{ToolCall: true, Vision: false},
	}

	caps := adapter.Capabilities(model)
	if !caps.ToolCall || caps.Vision {
		t.Errorf("Capabilities = %#v", caps)
	}
}

func TestRegistryRegisterAndGet(t *testing.T) {
	reg := NewRegistry()

	customAdapter := &OpenAICompatibleAdapter{}
	reg.Register("custom-provider", customAdapter)

	if reg.Get("custom-provider") != customAdapter {
		t.Fatal("custom provider adapter not returned")
	}

	if reg.Get("unknown-provider") == nil {
		t.Fatal("default adapter should be returned for unknown provider")
	}
}

func TestIdempotencySupport(t *testing.T) {
	adapter := &OpenAICompatibleAdapter{}

	tests := []struct {
		function string
		want     bool
	}{
		{"chat", true},
		{"chat-tools", true},
		{"embedding", true},
		{"rerank", true},
		{"moderation", true},
		{"image-generation", false},
		{"video-generation", false},
		{"text-to-speech", false},
		{"speech-to-text", false},
	}

	for _, tt := range tests {
		t.Run(tt.function, func(t *testing.T) {
			if got := adapter.IdempotencySupport(tt.function); got != tt.want {
				t.Errorf("IdempotencySupport(%q) = %v, want %v", tt.function, got, tt.want)
			}
		})
	}
}
