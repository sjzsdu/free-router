package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/health"
	"github.com/sjzsdu/free-router/internal/provider"
	"github.com/sjzsdu/free-router/internal/routing"
)

func TestAutoRetriesNextFreeModel(t *testing.T) {
	clearBuiltinKeys(t)
	var chatCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_, _ = w.Write([]byte(`{"data":[
				{"id":"free/a","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]}},
				{"id":"free/b","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]}}
			]}`))
		case "/chat/completions":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if chatCalls.Add(1) == 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"code":20012,"message":"Model does not exist. Please check it carefully.","data":null}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"model": body["model"]})
		}
	}))
	defer upstream.Close()

	registry, err := provider.NewRegistry(`[{"id":"test","base_url":"` + upstream.URL + `","api_key":"test"}]`)
	if err != nil {
		t.Fatal(err)
	}
	store := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), upstream.Client())
	if err := store.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	handler := New(store, registry, Config{MaxAttempts: 2}, upstream.Client())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[]}`))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if chatCalls.Load() != 2 {
		t.Fatalf("expected 2 calls, got %d", chatCalls.Load())
	}

	modelsRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	modelsRecorder := httptest.NewRecorder()
	handler.ServeHTTP(modelsRecorder, modelsRequest)
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(modelsRecorder.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) < 2 || list.Data[1]["type"] != "normal" || list.Data[1]["capabilities"] == nil {
		t.Fatalf("model metadata is missing: %#v", list.Data)
	}
}

func TestNamedRouteUsesConfiguredFallbackOrder(t *testing.T) {
	clearBuiltinKeys(t)
	var calls []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"first"},{"id":"second"}]}`))
		case "/chat/completions":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			model, _ := body["model"].(string)
			calls = append(calls, model)
			if model == "first" {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"model": model})
		}
	}))
	defer upstream.Close()
	registry, err := provider.NewRegistry(`[{"id":"test","base_url":"` + upstream.URL + `","api_key":"test"}]`)
	if err != nil {
		t.Fatal(err)
	}
	store := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), upstream.Client())
	if err := store.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	routes, err := routing.New(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	config := routes.Config()
	route := config.Routes["chat"]
	route.Models = []string{"test/first", "test/second"}
	config.Routes["chat"] = route
	if err := routes.Update(config); err != nil {
		t.Fatal(err)
	}
	tracker := health.New()
	handler := New(store, registry, Config{MaxAttempts: 3, Routes: routes, Health: tracker}, upstream.Client())
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","messages":[]}`)))
	if recorder.Code != http.StatusOK || strings.Join(calls, ",") != "first,second" {
		t.Fatalf("status=%d calls=%v body=%s", recorder.Code, calls, recorder.Body.String())
	}
	states := tracker.Snapshot()
	if len(states) != 2 || states[0].Status != "cooling" || states[1].Status != "healthy" {
		t.Fatalf("health states = %#v", states)
	}
}

func TestEmbeddingAliasUsesCachedEmbeddingModel(t *testing.T) {
	clearBuiltinKeys(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"BAAI/bge-m3"}]}`))
		case "/embeddings":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(map[string]any{"model": body["model"]})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	registry, _ := provider.NewRegistry(`[{"id":"test","base_url":"` + upstream.URL + `","api_key":"test"}]`)
	store := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), upstream.Client())
	if err := store.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	routes, _ := routing.New(filepath.Join(t.TempDir(), "config.json"))
	handler := New(store, registry, Config{MaxAttempts: 2, Routes: routes}, upstream.Client())
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"embedding","input":"hello"}`)))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "BAAI/bge-m3") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestRewriteMultipartModelPreservesFile(t *testing.T) {
	var input bytes.Buffer
	writer := multipart.NewWriter(&input)
	file, _ := writer.CreateFormFile("file", "voice.wav")
	_, _ = io.WriteString(file, "audio-data")
	_ = writer.WriteField("model", "audio")
	boundary := writer.Boundary()
	_ = writer.Close()

	output, _, err := rewriteMultipartModel(input.Bytes(), boundary, "whisper-large-v3")
	if err != nil {
		t.Fatal(err)
	}
	reader := multipart.NewReader(bytes.NewReader(output), boundary)
	values := map[string]string{}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(part)
		values[part.FormName()] = string(data)
	}
	if values["model"] != "whisper-large-v3" || values["file"] != "audio-data" {
		t.Fatalf("multipart values = %#v", values)
	}
}

func TestAutoFallsBackAcrossProviders(t *testing.T) {
	clearBuiltinKeys(t)
	first := testProviderServer(t, http.StatusTooManyRequests, "a-model")
	second := testProviderServer(t, http.StatusOK, "b-model")

	custom := `[
		{"id":"a","base_url":"` + first.URL + `","api_key":"a-key"},
		{"id":"b","base_url":"` + second.URL + `","api_key":"b-key"}
	]`
	registry, err := provider.NewRegistry(custom)
	if err != nil {
		t.Fatal(err)
	}
	store := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), first.Client())
	if err := store.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	handler := New(store, registry, Config{MaxAttempts: 2}, first.Client())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[]}`))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("X-Free-Router-Provider"); got != "b" {
		t.Fatalf("expected provider b, got %q", got)
	}
}

func testProviderServer(t *testing.T, chatStatus int, model string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": model}}})
		case "/chat/completions":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				http.Error(w, "missing auth", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(chatStatus)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func clearBuiltinKeys(t *testing.T) {
	t.Helper()
	for _, key := range provider.SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
}
