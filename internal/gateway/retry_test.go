package gateway

import (
	"net/http"
	"testing"
	"time"

	"github.com/sjzsdu/free-router/internal/catalog"
)

func TestRequestTypeForCapability(t *testing.T) {
	tests := []struct {
		capability string
		expected   RequestType
	}{
		{"chat", RequestIdempotent},
		{"chat-tools", RequestIdempotent},
		{"embedding", RequestIdempotent},
		{"rerank", RequestIdempotent},
		{"moderation", RequestIdempotent},
		{"image-generation", RequestNonIdempotent},
		{"video-generation", RequestNonIdempotent},
		{"text-to-speech", RequestNonIdempotent},
		{"speech-to-text", RequestIdempotent},
		{"image-understanding", RequestIdempotent},
		{"video-understanding", RequestIdempotent},
	}

	for _, tt := range tests {
		t.Run(tt.capability, func(t *testing.T) {
			if got := requestTypeForCapability(tt.capability); got != tt.expected {
				t.Errorf("requestTypeForCapability(%q) = %v, want %v", tt.capability, got, tt.expected)
			}
		})
	}
}

func TestRetryPolicyShouldRetry(t *testing.T) {
	policy := DefaultRetryPolicy()

	tests := []struct {
		name            string
		reqType         RequestType
		statusCode      int
		hasResponseBody bool
		attempt         int
		expectedRetry   bool
		expectedReason  string
	}{
		{"idempotent 500", RequestIdempotent, 500, false, 0, true, "server error"},
		{"idempotent 503", RequestIdempotent, 503, false, 0, true, "server error"},
		{"idempotent 429", RequestIdempotent, 429, false, 0, true, "rate limited"},
		{"idempotent 408", RequestIdempotent, 408, false, 0, true, "timeout"},
		{"idempotent 0 connection error", RequestIdempotent, 0, false, 0, true, "connection error"},
		{"idempotent 400", RequestIdempotent, 400, false, 0, false, "client error not retryable"},
		{"idempotent 401", RequestIdempotent, 401, false, 0, true, "provider account unavailable"},
		{"idempotent 402", RequestIdempotent, 402, false, 0, true, "provider account unavailable"},
		{"idempotent 403", RequestIdempotent, 403, false, 0, true, "provider account unavailable"},
		{"idempotent 404", RequestIdempotent, 404, false, 0, true, "upstream model unavailable"},
		{"non-idempotent 500 no body", RequestNonIdempotent, 500, false, 0, false, "server error on non-idempotent request"},
		{"non-idempotent 500 with body", RequestNonIdempotent, 500, true, 0, false, "server error on non-idempotent request"},
		{"non-idempotent 400", RequestNonIdempotent, 400, false, 0, false, "client error on non-idempotent request"},
		{"non-idempotent 200 with body", RequestNonIdempotent, 200, true, 0, false, "non-idempotent request already processed"},
		{"max attempts exceeded", RequestIdempotent, 500, false, 3, false, "max attempts exceeded"},
		{"non-idempotent timeout with body", RequestNonIdempotent, 408, true, 0, false, "timeout on non-idempotent request"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := policy.ShouldRetry(tt.reqType, tt.statusCode, tt.hasResponseBody, tt.attempt)
			if result.ShouldRetry != tt.expectedRetry {
				t.Errorf("ShouldRetry() ShouldRetry = %v, want %v", result.ShouldRetry, tt.expectedRetry)
			}
			if result.Reason != tt.expectedReason {
				t.Errorf("ShouldRetry() Reason = %q, want %q", result.Reason, tt.expectedReason)
			}
		})
	}
}

func TestBackoffDelay(t *testing.T) {
	tests := []struct {
		attempt  int
		maxDelay time.Duration
		expected time.Duration
	}{
		{0, 30 * time.Second, 500 * time.Millisecond},
		{1, 30 * time.Second, 1 * time.Second},
		{2, 30 * time.Second, 2 * time.Second},
		{3, 30 * time.Second, 4 * time.Second},
		{10, 5 * time.Second, 5 * time.Second},
	}

	for _, tt := range tests {
		t.Run("attempt_"+string(rune('0'+tt.attempt)), func(t *testing.T) {
			if got := backoffDelay(tt.attempt, tt.maxDelay); got != tt.expected {
				t.Errorf("backoffDelay(%d, %v) = %v, want %v", tt.attempt, tt.maxDelay, got, tt.expected)
			}
		})
	}
}

func TestShouldRetryNonIdempotentDuplicatePrevention(t *testing.T) {
	g := &Gateway{}
	model := catalog.Model{
		ID:        "test/model",
		Provider:  "test",
		Functions: []string{"image-generation"},
	}

	tests := []struct {
		name          string
		statusCode    int
		contentLength int64
		attempt       int
		expectedRetry bool
	}{
		{"image generation 500 no body", 500, 0, 0, false},
		{"image generation 500 with body", 500, 100, 0, false},
		{"image generation 200", http.StatusOK, 100, 0, false},
		{"image generation 400", 400, 0, 0, false},
		{"image generation timeout no body", 408, 0, 0, false},
		{"image generation timeout with body", 408, 100, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision := g.shouldRetry(model, "image-generation", tt.statusCode, tt.contentLength > 0, tt.attempt)
			if decision.ShouldRetry != tt.expectedRetry {
				t.Errorf("shouldRetry() = %v, want %v", decision.ShouldRetry, tt.expectedRetry)
			}
		})
	}
}
