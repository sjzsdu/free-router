package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/provider"
)

type Request struct {
	Context     context.Context
	Method      string
	Endpoint    string
	Model       catalog.Model
	Provider    provider.Spec
	Body        []byte
	ContentType string
	Headers     map[string]string
	Stream      bool
	Function    string
}

type Response struct {
	StatusCode  int
	Body        []byte
	ContentType string
	Headers     http.Header
	Stream      bool
	Error       *Error
}

type Error struct {
	StatusCode int
	Message    string
	Retryable  bool
	RateLimit  bool
}

type ProviderAdapter interface {
	BuildRequest(req Request) (*http.Request, error)
	NormalizeResponse(resp *http.Response) (Response, error)
	ClassifyError(statusCode int, body []byte) Error
	Capabilities(model catalog.Model) catalog.Capabilities
	IdempotencySupport(function string) bool
	Name() string
}

type Registry struct {
	adapters map[string]ProviderAdapter
	defaults ProviderAdapter
}

func NewRegistry() *Registry {
	registry := &Registry{
		adapters: make(map[string]ProviderAdapter),
		defaults: &OpenAICompatibleAdapter{},
	}
	registry.Register("cloudflare", &CloudflareAdapter{})
	return registry
}

func (r *Registry) Register(providerID string, adapter ProviderAdapter) {
	r.adapters[providerID] = adapter
}

func (r *Registry) Get(providerID string) ProviderAdapter {
	if adapter, ok := r.adapters[providerID]; ok {
		return adapter
	}
	return r.defaults
}

type OpenAICompatibleAdapter struct{}

func (a *OpenAICompatibleAdapter) Name() string {
	return "openai-compatible"
}

func (a *OpenAICompatibleAdapter) BuildRequest(req Request) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(req.Context, req.Method, req.Provider.APIEndpoint(req.Endpoint), nil)
	if err != nil {
		return nil, err
	}

	httpReq.Body = &nopCloser{data: req.Body}
	httpReq.Header.Set("Content-Type", req.ContentType)
	httpReq.Header.Set("User-Agent", "free-router/0.2")

	for key, value := range req.Provider.Headers {
		httpReq.Header.Set(key, value)
	}

	authHeaders := make(map[string]string)
	req.Provider.ApplyAuth(authHeaders)
	for key, value := range authHeaders {
		httpReq.Header.Set(key, value)
	}

	for key, value := range req.Headers {
		httpReq.Header.Set(key, value)
	}

	return httpReq, nil
}

func (a *OpenAICompatibleAdapter) NormalizeResponse(resp *http.Response) (Response, error) {
	return normalizeResponse(resp, a.ClassifyError)
}

func normalizeResponse(resp *http.Response, classify func(int, []byte) Error) (Response, error) {
	result := Response{
		StatusCode:  resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Headers:     resp.Header,
		Stream:      isStreamResponse(resp),
	}
	if resp.StatusCode < http.StatusBadRequest {
		return result, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Response{}, fmt.Errorf("read provider error response: %w", err)
	}
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	result.Body = body
	classified := classify(resp.StatusCode, body)
	result.Error = &classified
	return result, nil
}

func (a *OpenAICompatibleAdapter) ClassifyError(statusCode int, body []byte) Error {
	err := Error{StatusCode: statusCode}

	switch {
	case statusCode >= 500:
		err.Message = "server error"
		err.Retryable = true
	case statusCode == http.StatusGone:
		err.Message = a.parseErrorMessage(statusCode, body)
		err.Retryable = true
	case statusCode == http.StatusTooManyRequests:
		err.Message = "rate limited"
		err.Retryable = true
		err.RateLimit = true
	case statusCode == http.StatusRequestTimeout:
		err.Message = "request timeout"
		err.Retryable = true
	case statusCode >= 400:
		err.Message = a.parseErrorMessage(statusCode, body)
		err.Retryable = false
	default:
		err.Message = "unknown error"
		err.Retryable = false
	}

	return err
}

func (a *OpenAICompatibleAdapter) parseErrorMessage(statusCode int, body []byte) string {
	var result struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &result); err == nil && result.Error.Message != "" {
		return result.Error.Message
	}
	return fmt.Sprintf("HTTP %d", statusCode)
}

// CloudflareAdapter handles Workers AI's non-OpenAI error envelope while
// retaining the compatible request and successful response behavior.
type CloudflareAdapter struct {
	OpenAICompatibleAdapter
}

func (a *CloudflareAdapter) Name() string { return "cloudflare-workers-ai" }

func (a *CloudflareAdapter) NormalizeResponse(resp *http.Response) (Response, error) {
	return normalizeResponse(resp, a.ClassifyError)
}

func (a *CloudflareAdapter) ClassifyError(statusCode int, body []byte) Error {
	classified := a.OpenAICompatibleAdapter.ClassifyError(statusCode, body)
	var envelope struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &envelope) == nil && len(envelope.Errors) > 0 && envelope.Errors[0].Message != "" {
		classified.Message = envelope.Errors[0].Message
	}
	return classified
}

func (a *OpenAICompatibleAdapter) Capabilities(model catalog.Model) catalog.Capabilities {
	return model.Capabilities
}

func (a *OpenAICompatibleAdapter) IdempotencySupport(function string) bool {
	switch function {
	case "chat", "chat-tools", "embedding", "rerank", "moderation":
		return true
	default:
		return false
	}
}

func isStreamResponse(resp *http.Response) bool {
	contentType := resp.Header.Get("Content-Type")
	return resp.StatusCode == http.StatusOK && (contentType == "text/event-stream" || contains(contentType, "event-stream"))
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

type nopCloser struct {
	data []byte
	pos  int
}

func (n *nopCloser) Read(p []byte) (int, error) {
	if n.pos >= len(n.data) {
		return 0, io.EOF
	}
	copied := copy(p, n.data[n.pos:])
	n.pos += copied
	return copied, nil
}

func (n *nopCloser) Close() error {
	return nil
}
