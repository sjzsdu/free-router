package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/health"
	"github.com/sjzsdu/free-router/internal/provider"
)

// syncBuffer is a log sink safe for the gateway's background goroutines: a
// plain bytes.Buffer would race with catalog refreshes that outlive the
// request under test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogs redirects slog's default logger into a buffer for the duration
// of one test. Access logging is deliberately a process-global concern, so a
// test that asserts on it must own the global too.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	sink := &syncBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return sink
}

// requestLogLine returns the access log line, or fails: exactly one line per
// request is part of the contract.
func requestLogLine(t *testing.T, sink *syncBuffer) string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(sink.String(), "\n") {
		if strings.Contains(line, "msg=request ") || strings.HasSuffix(line, "msg=request") {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one access log line, got %d:\n%s", len(found), sink.String())
	}
	return found[0]
}

func accessLogGateway(t *testing.T, client *http.Client) *Gateway {
	t.Helper()
	registry, err := provider.NewRegistry(`[{"id":"test","base_url":"https://test.invalid","api_key":"test"}]`)
	if err != nil {
		t.Fatal(err)
	}
	store := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), client)
	discoverModelsForTest(t, store)
	return New(store, registry, Config{MaxAttempts: 2, Health: health.New()}, client)
}

func TestAccessLogNamesServedProviderAndUsage(t *testing.T) {
	clearBuiltinKeys(t)
	sink := captureLogs(t)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/models":
			return testResponse(http.StatusOK, `{"data":[{"id":"chat-a","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]}}]}`), nil
		case "/chat/completions":
			resp := testResponse(http.StatusOK, `{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":6,"total_tokens":17}}`)
			resp.Header.Set("Content-Type", "application/json")
			return resp, nil
		default:
			return testResponse(http.StatusNotFound, "not found"), nil
		}
	})}
	handler := accessLogGateway(t, client)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","messages":[]}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	line := requestLogLine(t, sink)
	for _, want := range []string{
		"level=INFO",
		"request_id=",
		"path=/v1/chat/completions",
		"route=chat",
		"status=200",
		"attempts=1",
		"provider=test",
		"model=chat-a",
		"input_tokens=11",
		"output_tokens=6",
		"bytes=",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("access log missing %q:\n%s", want, line)
		}
	}
}

func TestAccessLogCountsFallbackAttempts(t *testing.T) {
	clearBuiltinKeys(t)
	sink := captureLogs(t)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/models":
			return testResponse(http.StatusOK, `{"data":[
				{"id":"chat-a","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]}},
				{"id":"chat-b","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]}}
			]}`), nil
		case "/chat/completions":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["model"] == "chat-a" {
				return testResponse(http.StatusInternalServerError, `{"error":{"message":"boom"}}`), nil
			}
			return testResponse(http.StatusOK, `{"choices":[]}`), nil
		default:
			return testResponse(http.StatusNotFound, "not found"), nil
		}
	})}
	handler := accessLogGateway(t, client)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","messages":[]}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	line := requestLogLine(t, sink)
	for _, want := range []string{"status=200", "attempts=2", "model=chat-b"} {
		if !strings.Contains(line, want) {
			t.Fatalf("access log missing %q:\n%s", want, line)
		}
	}
}

func TestAccessLogExplainsTerminalFailure(t *testing.T) {
	clearBuiltinKeys(t)
	sink := captureLogs(t)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/models":
			return testResponse(http.StatusOK, `{"data":[{"id":"chat-a","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]}}]}`), nil
		case "/chat/completions":
			return testResponse(http.StatusInternalServerError, `{"error":{"message":"boom"}}`), nil
		default:
			return testResponse(http.StatusNotFound, "not found"), nil
		}
	})}
	handler := accessLogGateway(t, client)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","messages":[]}`)))
	// A single candidate keeps its upstream status verbatim; the pool-wide
	// remapping only applies when several providers were tried.
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	line := requestLogLine(t, sink)
	for _, want := range []string{"level=ERROR", "status=500", "code=upstream_failed", "attempts=1"} {
		if !strings.Contains(line, want) {
			t.Fatalf("access log missing %q:\n%s", want, line)
		}
	}
}

// TestAccessLogKeepsStreamingFlushes is the regression guard for wrapping the
// ResponseWriter: if responseRecorder stopped satisfying http.Flusher, every
// SSE answer would arrive as one buffered burst instead of a token stream.
func TestAccessLogKeepsStreamingFlushes(t *testing.T) {
	clearBuiltinKeys(t)
	sink := captureLogs(t)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/models":
			return testResponse(http.StatusOK, `{"data":[{"id":"chat-a","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]}}]}`), nil
		case "/chat/completions":
			resp := testResponse(http.StatusOK, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
			resp.Header.Set("Content-Type", "text/event-stream")
			return resp, nil
		default:
			return testResponse(http.StatusNotFound, "not found"), nil
		}
	})}
	handler := accessLogGateway(t, client)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","messages":[],"stream":true}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !recorder.Flushed {
		t.Fatal("streaming response was not flushed through the access-log wrapper")
	}
	line := requestLogLine(t, sink)
	if !strings.Contains(line, "stream=true") {
		t.Fatalf("access log missing stream marker:\n%s", line)
	}
}

func TestResponseRecorderCapturesImplicitStatusAndBytes(t *testing.T) {
	recorder := newResponseRecorder(httptest.NewRecorder())
	if _, err := io.WriteString(recorder, "hello"); err != nil {
		t.Fatal(err)
	}
	if got := recorder.Status(); got != http.StatusOK {
		t.Fatalf("implicit status = %d, want 200", got)
	}
	if recorder.bytes != int64(len("hello")) {
		t.Fatalf("bytes = %d, want %d", recorder.bytes, len("hello"))
	}
	// A second WriteHeader is a no-op on http.ResponseWriter; the recorder
	// must keep reporting the status the caller actually received.
	recorder.WriteHeader(http.StatusTeapot)
	if got := recorder.Status(); got != http.StatusOK {
		t.Fatalf("status after duplicate WriteHeader = %d, want 200", got)
	}
	explicit := newResponseRecorder(httptest.NewRecorder())
	explicit.WriteHeader(http.StatusTeapot)
	if got := explicit.Status(); got != http.StatusTeapot {
		t.Fatalf("explicit status = %d, want %d", got, http.StatusTeapot)
	}
}
