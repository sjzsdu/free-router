package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/provider"
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
