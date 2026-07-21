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
	discoverModelsForTest(t, store)
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
	if models := store.Models(); len(models) != 1 || models[0].UpstreamID != "free/b" {
		t.Fatalf("failed model was not removed from cache: %#v", models)
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
	if len(list.Data) != 1 || list.Data[0]["id"] != catalog.FunctionChat || list.Data[0]["owned_by"] != "free-router" {
		t.Fatalf("stable capability models are missing: %#v", list.Data)
	}
}

func TestModelsEndpointHidesFailedModels(t *testing.T) {
	clearBuiltinKeys(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"healthy"},{"id":"failed"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()
	registry, _ := provider.NewRegistry(`[{"id":"test","base_url":"` + upstream.URL + `","no_auth":true}]`)
	store := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), upstream.Client())
	discoverModelsForTest(t, store)
	tracker := health.New()
	tracker.Failure("test/failed", catalog.FunctionChat, 0, http.StatusBadGateway, "broken", 0)
	handler := New(store, registry, Config{Health: tracker}, upstream.Client())
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 1 || list.Data[0].ID != catalog.FunctionChat {
		t.Fatalf("unexpected discoverable models: %#v", list.Data)
	}
	for _, model := range list.Data {
		if strings.HasPrefix(model.ID, "test/") {
			t.Fatalf("physical model leaked through public catalog: %#v", list.Data)
		}
	}
}

func TestModelsEndpointOmitsCapabilityWithoutHealthyCandidates(t *testing.T) {
	clearBuiltinKeys(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"only-chat"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()
	registry, _ := provider.NewRegistry(`[{"id":"test","base_url":"` + upstream.URL + `","no_auth":true}]`)
	store := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), upstream.Client())
	discoverModelsForTest(t, store)
	tracker := health.New()
	tracker.Failure("test/only-chat", catalog.FunctionChat, 0, http.StatusBadGateway, "broken", 0)
	handler := New(store, registry, Config{Health: tracker}, upstream.Client())
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	var list struct {
		Data []any `json:"data"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 0 {
		t.Fatalf("failed capability remained discoverable: %#v", list.Data)
	}
}

func TestCapabilityFailureDoesNotDisableAnotherFunctionOnSameModel(t *testing.T) {
	clearBuiltinKeys(t)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"multimodal","architecture":{"input_modalities":["text","image"],"output_modalities":["text"]}}]}`))
		case "/chat/completions":
			calls.Add(1)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	registry, _ := provider.NewRegistry(`[{"id":"test","base_url":"` + upstream.URL + `","no_auth":true}]`)
	store := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), upstream.Client())
	discoverModelsForTest(t, store)
	routes, _ := routing.New(filepath.Join(t.TempDir(), "config.json"))
	tracker := health.New()
	tracker.Failure("test/multimodal", catalog.FunctionImageUnderstanding, 0, http.StatusBadRequest, "image failed", 0)
	handler := New(store, registry, Config{Routes: routes, Health: tracker, MaxAttempts: 2}, upstream.Client())

	chat := httptest.NewRecorder()
	handler.ServeHTTP(chat, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","messages":[]}`)))
	if chat.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("chat capability was incorrectly isolated: status=%d body=%s", chat.Code, chat.Body.String())
	}
	vision := httptest.NewRecorder()
	handler.ServeHTTP(vision, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"image-understanding","messages":[]}`)))
	if vision.Code != http.StatusServiceUnavailable || calls.Load() != 1 {
		t.Fatalf("failed image capability was routed: status=%d calls=%d body=%s", vision.Code, calls.Load(), vision.Body.String())
	}
}

func TestCapabilityAliasMustMatchOpenAIEndpoint(t *testing.T) {
	clearBuiltinKeys(t)
	registry, _ := provider.NewRegistryAllowEmpty("")
	store := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), http.DefaultClient)
	routes, _ := routing.New(filepath.Join(t.TempDir(), "config.json"))
	handler := New(store, registry, Config{Routes: routes}, http.DefaultClient)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"image-generation","messages":[]}`)))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "not compatible") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestNamedRouteFallsBackToRemainingModelAfterPriorityArray(t *testing.T) {
	clearBuiltinKeys(t)
	var calls []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"priority-a"},{"id":"priority-b"},{"id":"remaining"}]}`))
		case "/chat/completions":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			model, _ := body["model"].(string)
			calls = append(calls, model)
			if model != "remaining" {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"model": model})
		}
	}))
	defer upstream.Close()
	registry, _ := provider.NewRegistry(`[{"id":"test","base_url":"` + upstream.URL + `","api_key":"test"}]`)
	store := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), upstream.Client())
	discoverModelsForTest(t, store)
	routes, _ := routing.New(filepath.Join(t.TempDir(), "config.json"))
	config := routes.Config()
	route := config.Routes["chat"]
	route.Models = []string{"test/priority-a", "test/priority-b"}
	config.Routes["chat"] = route
	if err := routes.Update(config); err != nil {
		t.Fatal(err)
	}
	handler := New(store, registry, Config{MaxAttempts: 1, Routes: routes}, upstream.Client())
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","messages":[]}`)))
	if recorder.Code != http.StatusOK || strings.Join(calls, ",") != "priority-a,priority-b,remaining" {
		t.Fatalf("status=%d calls=%v body=%s", recorder.Code, calls, recorder.Body.String())
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
	discoverModelsForTest(t, store)
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
	if len(states) != 2 || states[0].Status != "failed" || states[1].Status != "healthy" {
		t.Fatalf("health states = %#v", states)
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","messages":[]}`)))
	if recorder.Code != http.StatusOK || strings.Join(calls, ",") != "first,second,second" {
		t.Fatalf("failed model was retried: status=%d calls=%v", recorder.Code, calls)
	}
}

func TestNamedRouteRoundRobinBalancesHealthyModels(t *testing.T) {
	clearBuiltinKeys(t)
	var calls []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"first"},{"id":"second"},{"id":"third"}]}`))
		case "/chat/completions":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			calls = append(calls, body["model"].(string))
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer upstream.Close()
	registry, _ := provider.NewRegistry(`[{"id":"test","base_url":"` + upstream.URL + `","api_key":"test"}]`)
	store := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), upstream.Client())
	discoverModelsForTest(t, store)
	routes, _ := routing.New(filepath.Join(t.TempDir(), "config.json"))
	config := routes.Config()
	route := config.Routes["chat"]
	route.Strategy = routing.StrategyRoundRobin
	route.Models = []string{"test/first", "test/second", "test/third"}
	config.Routes["chat"] = route
	if err := routes.Update(config); err != nil {
		t.Fatal(err)
	}
	handler := New(store, registry, Config{MaxAttempts: 3, Routes: routes}, upstream.Client())
	for range 4 {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","messages":[]}`)))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}
	if got := strings.Join(calls, ","); got != "first,second,third,first" {
		t.Fatalf("round-robin calls = %s", got)
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
	discoverModelsForTest(t, store)
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
	discoverModelsForTest(t, store)
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

func discoverModelsForTest(t *testing.T, store *catalog.Store) {
	t.Helper()
	models, failures := store.DiscoverFromProviders(context.Background())
	if len(failures) > 0 || len(models) == 0 {
		t.Fatalf("models=%d failures=%#v", len(models), failures)
	}
}
