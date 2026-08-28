package catalog

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjzsdu/free-router/internal/provider"
)

type recordingBuilder struct {
	baseURL      string
	called       bool
	lastEndpoint string
	lastFunction string
	lastBody     string
}

func (b *recordingBuilder) BuildProbeRequest(ctx context.Context, model Model, spec provider.Spec, method, endpoint, contentType, function string, body []byte) (*http.Request, error) {
	b.called = true
	b.lastEndpoint = endpoint
	b.lastFunction = function
	b.lastBody = string(body)
	req, err := http.NewRequestWithContext(ctx, method, b.baseURL+endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	return req, nil
}

func TestProbeRunnerRoutesThroughBuilder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	var b recordingBuilder
	b.baseURL = srv.URL

	runner := NewProbeRunner(http.DefaultClient).WithBuilder(&b)
	spec := provider.Spec{ID: "x", BaseURL: srv.URL}
	model := Model{ID: "x/chat", Provider: "x", UpstreamID: "chat", Functions: []string{FunctionChat}}

	if _, err := runner.Run(context.Background(), model, spec, FunctionChat); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !b.called {
		t.Fatal("probe did not use the configured builder")
	}
	if b.lastEndpoint != "/chat/completions" {
		t.Errorf("endpoint = %q, want /chat/completions", b.lastEndpoint)
	}
	if b.lastFunction != FunctionChat {
		t.Errorf("function = %q, want %q", b.lastFunction, FunctionChat)
	}
}

func TestProbeRunnerFallsBackWithoutBuilder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	var b recordingBuilder
	runner := NewProbeRunner(http.DefaultClient)
	spec := provider.Spec{ID: "x", BaseURL: srv.URL}
	model := Model{ID: "x/chat", Provider: "x", UpstreamID: "chat", Functions: []string{FunctionChat}}

	if _, err := runner.Run(context.Background(), model, spec, FunctionChat); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if b.called {
		t.Fatal("builder must not be used when none is configured")
	}
}
