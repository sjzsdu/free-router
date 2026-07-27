package adapter

import (
	"testing"

	"github.com/sjzsdu/free-router/internal/catalog"
)

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
		{"401 auth error", 401, []byte(`{"error":{"message":"unauthorized"}}`), "unauthorized", false, false},
		{"400 error without message", 400, []byte(`{}`), "HTTP 0", false, false},
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
