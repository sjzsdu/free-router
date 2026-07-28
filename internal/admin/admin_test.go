package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/credentials"
	"github.com/sjzsdu/free-router/internal/health"
	"github.com/sjzsdu/free-router/internal/provider"
	"github.com/sjzsdu/free-router/internal/routing"
)

func TestAdminUpdatesRouteConfiguration(t *testing.T) {
	for _, key := range provider.SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"chat-a"}]}`))
	}))
	defer upstream.Close()
	registry, _ := provider.NewRegistry(`[{"id":"test","base_url":"` + upstream.URL + `","api_key":"key"}]`)
	models := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), upstream.Client())
	discoverModelsForTest(t, models)
	routes, _ := routing.New(filepath.Join(t.TempDir(), "config.json"))
	vault := credentials.NewFileOnly(filepath.Join(t.TempDir(), "credentials.json"))
	handler := New(routes, models, vault, health.New(), Config{}, nil)

	config := routes.Config()
	route := config.Routes["chat"]
	route.Models = []string{"test/chat-a"}
	config.Routes["chat"] = route
	body, _ := json.Marshal(config)
	request := httptest.NewRequest(http.MethodPut, "/admin/api/config", bytes.NewReader(body))
	request.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	saved, _ := routes.Route("chat")
	if len(saved.Models) != 1 || saved.Models[0] != "test/chat-a" {
		t.Fatalf("saved route = %#v", saved)
	}
}

func TestAdminRejectsRemoteAccessByDefault(t *testing.T) {
	routes, _ := routing.New(filepath.Join(t.TempDir(), "config.json"))
	registry, _ := provider.NewRegistry("")
	models := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), http.DefaultClient)
	vault := credentials.NewFileOnly(filepath.Join(t.TempDir(), "credentials.json"))
	handler := New(routes, models, vault, health.New(), Config{}, nil)
	request := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	request.RemoteAddr = "192.0.2.10:1234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status=%d", recorder.Code)
	}
}

func TestAdminServesEmbeddedReactApp(t *testing.T) {
	routes, _ := routing.New(filepath.Join(t.TempDir(), "config.json"))
	registry, _ := provider.NewRegistry("")
	models := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), http.DefaultClient)
	vault := credentials.NewFileOnly(filepath.Join(t.TempDir(), "credentials.json"))
	handler := New(routes, models, vault, health.New(), Config{}, nil)

	request := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !bytes.Contains(recorder.Body.Bytes(), []byte(`<div id="root"></div>`)) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	asset := regexp.MustCompile(`src="(/admin/assets/[^"]+\.js)"`).FindSubmatch(recorder.Body.Bytes())
	if len(asset) != 2 {
		t.Fatalf("compiled script not found in body=%s", recorder.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, string(asset[1]), nil)
	request.RemoteAddr = "127.0.0.1:1234"
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("asset status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestCredentialSaveEnablesProviderWithoutDiscoveringModels(t *testing.T) {
	for _, key := range provider.SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"chat-a"}]}`))
	}))
	defer upstream.Close()
	custom := `[{"id":"test","base_url":"` + upstream.URL + `"}]`
	vault := credentials.NewFileOnly(filepath.Join(t.TempDir(), "credentials.json"))
	registry, err := provider.NewRegistry(custom, vault.Get)
	if err != nil {
		t.Fatal(err)
	}
	models := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), upstream.Client())
	routes, _ := routing.New(filepath.Join(t.TempDir(), "config.json"))
	handler := New(routes, models, vault, health.New(), Config{}, func(map[string][]string) (func(), error) { return nil, registry.Reload(custom, vault.Get) })
	request := httptest.NewRequest(http.MethodPost, "/admin/api/credentials", bytes.NewBufferString(`{"provider":"test","api_key":"secret"}`))
	request.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if _, ok := registry.Get("test"); !ok {
		t.Fatal("provider was not enabled immediately")
	}
	if len(models.Models()) != 0 {
		t.Fatalf("credential save bypassed Formula inventory: models=%d", len(models.Models()))
	}
}

func TestCredentialSaveValidatesProviderAndUpdatesModelHealth(t *testing.T) {
	for _, key := range provider.SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
	var modelCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "missing saved credential", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"chat-a"}]}`))
		case "/chat/completions":
			modelCalls.Add(1)
			var input struct {
				Tools []json.RawMessage `json:"tools"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			if len(input.Tools) > 0 {
				_, _ = w.Write([]byte(`{"choices":[{"message":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"ping","arguments":"{}"}}]}}]}`))
			} else {
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "free-models.json")
	manifest := `{"schema_version":2,"generated_at":"test","providers":{"groq":{"source_urls":["https://example.com/models"],"models":[{"id":"chat-a","functions":["chat"]}]}}}`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	custom := `[{"id":"groq","base_url":"` + upstream.URL + `"}]`
	vault := credentials.NewFileOnly(filepath.Join(t.TempDir(), "credentials.json"))
	registry, err := provider.NewRegistryWithManifest(custom, provider.DefaultEnvMap(), manifestPath, vault.Get)
	if err != nil {
		t.Fatal(err)
	}
	models := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), upstream.Client())
	if err := models.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	routes, _ := routing.New(filepath.Join(t.TempDir(), "config.json"))
	tracker := health.New()
	handler := New(routes, models, vault, tracker, Config{FreeModels: manifestPath}, func(map[string][]string) (func(), error) {
		return nil, registry.ReloadWithManifest(custom, provider.DefaultEnvMap(), manifestPath, vault.Get)
	})
	request := httptest.NewRequest(http.MethodPost, "/admin/api/credentials", bytes.NewBufferString(`{"provider":"groq","api_key":"secret"}`))
	request.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		Validation struct {
			OK            bool `json:"ok"`
			FormulaModels int  `json:"formula_models"`
		} `json:"validation"`
		ModelProbeStarted bool `json:"model_probe_started"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Validation.OK || result.Validation.FormulaModels != 1 || !result.ModelProbeStarted {
		t.Fatalf("save result=%#v body=%s", result, recorder.Body.String())
	}
	deadline := time.Now().Add(time.Second)
	for handler.probes.Snapshot().Status == "running" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	states := tracker.Snapshot()
	model, ok := models.Find("groq/chat-a")
	if modelCalls.Load() != 2 || len(states) != 2 || !ok || !model.Capabilities.ToolCallKnown || !model.Capabilities.ToolCall || !model.SupportsFunction(catalog.FunctionChatTools) {
		t.Fatalf("model_calls=%d health=%#v", modelCalls.Load(), states)
	}

	stateRequest := httptest.NewRequest(http.MethodGet, "/admin/api/state", nil)
	stateRequest.RemoteAddr = "127.0.0.1:1234"
	stateRecorder := httptest.NewRecorder()
	handler.ServeHTTP(stateRecorder, stateRequest)
	if stateRecorder.Code != http.StatusOK || !strings.Contains(stateRecorder.Body.String(), `"connection_status":"healthy"`) {
		t.Fatalf("provider state was not updated: status=%d body=%s", stateRecorder.Code, stateRecorder.Body.String())
	}
}

func TestCredentialSaveKeepsInvalidKeyAndMarksProviderModelsFailed(t *testing.T) {
	for _, key := range provider.SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"message":"invalid key"}}`, http.StatusUnauthorized)
	}))
	defer upstream.Close()
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "free-models.json")
	manifest := `{"schema_version":2,"generated_at":"test","providers":{"groq":{"source_urls":["https://example.com/models"],"models":[{"id":"chat-a","functions":["chat"]}]}}}`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	custom := `[{"id":"groq","base_url":"` + upstream.URL + `"}]`
	vault := credentials.NewFileOnly(filepath.Join(dir, "credentials.json"))
	registry, err := provider.NewRegistryWithManifest(custom, provider.DefaultEnvMap(), manifestPath, vault.Get)
	if err != nil {
		t.Fatal(err)
	}
	models := catalog.New(registry, filepath.Join(dir, "models.json"), upstream.Client())
	if err := models.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	routes, _ := routing.New(filepath.Join(dir, "config.json"))
	tracker := health.New()
	handler := New(routes, models, vault, tracker, Config{FreeModels: manifestPath}, func(map[string][]string) (func(), error) {
		return nil, registry.ReloadWithManifest(custom, provider.DefaultEnvMap(), manifestPath, vault.Get)
	})
	request := httptest.NewRequest(http.MethodPost, "/admin/api/credentials", bytes.NewBufferString(`{"provider":"groq","api_key":"bad-key"}`))
	request.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"ok":false`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if saved, _ := vault.Get("groq"); saved != "bad-key" {
		t.Fatalf("saved key=%q", saved)
	}
	states := tracker.Snapshot()
	if len(states) != 1 || states[0].Status != health.StatusOpen || states[0].LastStatus != http.StatusUnauthorized {
		t.Fatalf("health=%#v", states)
	}
}

func TestAdminTokenAuthentication(t *testing.T) {
	routes, _ := routing.New(filepath.Join(t.TempDir(), "config.json"))
	registry, _ := provider.NewRegistry("")
	models := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), http.DefaultClient)
	vault := credentials.NewFileOnly(filepath.Join(t.TempDir(), "credentials.json"))
	handler := New(routes, models, vault, health.New(), Config{Token: "secret-token"}, nil)

	request := httptest.NewRequest(http.MethodGet, "/admin/api/state", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", recorder.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/admin/api/state", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	request.SetBasicAuth("admin", "secret-token")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("authenticated status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestRuntimeStatus(t *testing.T) {
	t.Setenv("FREE_ROUTER_SERVICE_MANAGER", "launchd")
	routes, _ := routing.New(filepath.Join(t.TempDir(), "config.json"))
	registry, _ := provider.NewRegistry("")
	models := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), http.DefaultClient)
	vault := credentials.NewFileOnly(filepath.Join(t.TempDir(), "credentials.json"))
	handler := New(routes, models, vault, health.New(), Config{Version: "0.1.0"}, nil)
	request := httptest.NewRequest(http.MethodGet, "/admin/api/runtime", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var runtimeStatus struct {
		Status         string `json:"status"`
		Version        string `json:"version"`
		ServiceManager string `json:"service_manager"`
		PID            int    `json:"pid"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &runtimeStatus); err != nil {
		t.Fatal(err)
	}
	if runtimeStatus.Status != "running" || runtimeStatus.Version != "0.1.0" || runtimeStatus.ServiceManager != "launchd" || runtimeStatus.PID < 1 {
		t.Fatalf("runtime status = %#v", runtimeStatus)
	}
}

func TestProviderConnectionProbe(t *testing.T) {
	for _, key := range provider.SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"chat-a"}]}`))
	}))
	defer upstream.Close()
	registry, _ := provider.NewRegistry(`[{"id":"test","base_url":"` + upstream.URL + `","no_auth":true}]`)
	models := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), upstream.Client())
	routes, _ := routing.New(filepath.Join(t.TempDir(), "config.json"))
	vault := credentials.NewFileOnly(filepath.Join(t.TempDir(), "credentials.json"))
	handler := New(routes, models, vault, health.New(), Config{}, nil)
	request := httptest.NewRequest(http.MethodPost, "/admin/api/providers/test/test", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestGroqForbiddenProbeExplainsPermissionRestriction(t *testing.T) {
	for _, key := range provider.SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"message":"Forbidden"}}`, http.StatusForbidden)
	}))
	defer upstream.Close()
	registry, _ := provider.NewRegistry(`[{"id":"groq","base_url":"` + upstream.URL + `","no_auth":true}]`)
	models := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), upstream.Client())
	routes, _ := routing.New(filepath.Join(t.TempDir(), "config.json"))
	vault := credentials.NewFileOnly(filepath.Join(t.TempDir(), "credentials.json"))
	handler := New(routes, models, vault, health.New(), Config{}, nil)
	request := httptest.NewRequest(http.MethodPost, "/admin/api/providers/groq/test", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "Model Permissions") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestAdminRejectsCrossOriginMutation(t *testing.T) {
	routes, _ := routing.New(filepath.Join(t.TempDir(), "config.json"))
	registry, _ := provider.NewRegistry("")
	models := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), http.DefaultClient)
	vault := credentials.NewFileOnly(filepath.Join(t.TempDir(), "credentials.json"))
	handler := New(routes, models, vault, health.New(), Config{}, nil)
	request := httptest.NewRequest(http.MethodPost, "/admin/api/refresh", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Origin", "https://evil.example")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status=%d", recorder.Code)
	}
}

func TestAdminResetsFailedModelHealth(t *testing.T) {
	routes, _ := routing.New(filepath.Join(t.TempDir(), "config.json"))
	registry, _ := provider.NewRegistry("")
	models := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), http.DefaultClient)
	vault := credentials.NewFileOnly(filepath.Join(t.TempDir(), "credentials.json"))
	tracker := health.New()
	tracker.Failure("provider/model", catalog.FunctionChat, 0, http.StatusTooManyRequests, "rate limited", 0)
	handler := New(routes, models, vault, tracker, Config{}, nil)
	request := httptest.NewRequest(http.MethodPost, "/admin/api/health/reset", strings.NewReader(`{"model":"provider/model"}`))
	request.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !tracker.Available("provider/model", catalog.FunctionChat) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestModelHealthProbeUses24HourCacheAndSupportsForce(t *testing.T) {
	for _, key := range provider.SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
	var chatCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"chat-a"}]}`))
		case "/chat/completions":
			chatCalls.Add(1)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	registry, _ := provider.NewRegistry(`[{"id":"test","base_url":"` + upstream.URL + `","no_auth":true}]`)
	models := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), upstream.Client())
	discoverModelsForTest(t, models)
	routes, _ := routing.New(filepath.Join(t.TempDir(), "config.json"))
	vault := credentials.NewFileOnly(filepath.Join(t.TempDir(), "credentials.json"))
	tracker := health.New()
	handler := New(routes, models, vault, tracker, Config{}, nil)

	probe := func(force bool) {
		request := httptest.NewRequest(http.MethodPost, "/admin/api/health/probe", strings.NewReader(fmt.Sprintf(`{"force":%t}`, force)))
		request.RemoteAddr = "127.0.0.1:1234"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusAccepted && recorder.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		deadline := time.Now().Add(time.Second)
		for handler.probes.Snapshot().Status == "running" && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if handler.probes.Snapshot().Status != "completed" {
			t.Fatal("health probe did not complete")
		}
	}

	probe(false)
	if chatCalls.Load() != 1 || tracker.Snapshot()[0].Status != "healthy" {
		t.Fatalf("chat_calls=%d health=%#v", chatCalls.Load(), tracker.Snapshot())
	}
	probe(false)
	if chatCalls.Load() != 1 {
		t.Fatalf("fresh cached model was probed again: calls=%d", chatCalls.Load())
	}
	probe(true)
	if chatCalls.Load() != 2 {
		t.Fatalf("forced probe did not run: calls=%d", chatCalls.Load())
	}
}

func TestHealthProbeRunsEightProvidersConcurrently(t *testing.T) {
	for _, key := range provider.SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
	var active atomic.Int64
	var maximum atomic.Int64
	allStarted := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = w.Write([]byte(`{"data":[{"id":"chat-model"}]}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			current := active.Add(1)
			for {
				previous := maximum.Load()
				if current <= previous || maximum.CompareAndSwap(previous, current) {
					break
				}
			}
			if current == healthProbeConcurrency {
				close(allStarted)
			}
			<-release
			active.Add(-1)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	customProviders := make([]map[string]any, 0, healthProbeConcurrency)
	for index := range healthProbeConcurrency {
		customProviders = append(customProviders, map[string]any{
			"id":       fmt.Sprintf("provider-%d", index),
			"base_url": fmt.Sprintf("%s/provider-%d", upstream.URL, index),
			"no_auth":  true,
		})
	}
	customJSON, err := json.Marshal(customProviders)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := provider.NewRegistry(string(customJSON))
	if err != nil {
		t.Fatal(err)
	}
	models := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), upstream.Client())
	discoverModelsForTest(t, models)
	routes, _ := routing.New(filepath.Join(t.TempDir(), "config.json"))
	handler := New(routes, models, credentials.NewFileOnly(filepath.Join(t.TempDir(), "credentials.json")), health.New(), Config{}, nil)

	status, started := handler.probes.Start(handler, true)
	if !started || status.Total != healthProbeConcurrency {
		t.Fatalf("started=%t status=%#v", started, status)
	}
	select {
	case <-allStarted:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatalf("only %d probes ran concurrently, want %d", maximum.Load(), healthProbeConcurrency)
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for handler.probes.Snapshot().Status == "running" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := maximum.Load(); got != healthProbeConcurrency {
		t.Fatalf("maximum concurrent probes=%d, want %d", got, healthProbeConcurrency)
	}
}

func TestExpensiveModelProbeRequiresConfirmation(t *testing.T) {
	for _, key := range provider.SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
	var imageCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"image-test","type":"image"}]}`))
		case "/images/generations":
			imageCalls.Add(1)
			_, _ = w.Write([]byte(`{"data":[{"url":"https://example.invalid/probe.png"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	registry, _ := provider.NewRegistry(`[{"id":"test","base_url":"` + upstream.URL + `","no_auth":true}]`)
	models := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), upstream.Client())
	discoverModelsForTest(t, models)
	routes, _ := routing.New(filepath.Join(t.TempDir(), "config.json"))
	vault := credentials.NewFileOnly(filepath.Join(t.TempDir(), "credentials.json"))
	tracker := health.New()
	handler := New(routes, models, vault, tracker, Config{}, nil)

	request := httptest.NewRequest(http.MethodPost, "/admin/api/health/probe/model", strings.NewReader(`{"model":"test/image-test"}`))
	request.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || imageCalls.Load() != 0 {
		t.Fatalf("unconfirmed status=%d calls=%d", recorder.Code, imageCalls.Load())
	}

	request = httptest.NewRequest(http.MethodPost, "/admin/api/health/probe/model", strings.NewReader(`{"model":"test/image-test","allow_expensive":true}`))
	request.RemoteAddr = "127.0.0.1:1234"
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("confirmed status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	deadline := time.Now().Add(time.Second)
	for handler.probes.Snapshot().Status == "running" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if imageCalls.Load() != 1 || tracker.Snapshot()[0].Status != "healthy" {
		t.Fatalf("calls=%d health=%#v", imageCalls.Load(), tracker.Snapshot())
	}
}

func TestAutomaticHealthProbeSkipsExpensiveCapabilities(t *testing.T) {
	for _, key := range provider.SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
	var imageCalls atomic.Int64
	var videoCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"image-test","type":"image"},{"id":"video-test","type":"video"}]}`))
		case "/images/generations":
			imageCalls.Add(1)
			_, _ = w.Write([]byte(`{"data":[{"url":"https://example.invalid/probe.png"}]}`))
		case "/videos/generations":
			videoCalls.Add(1)
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	registry, _ := provider.NewRegistry(`[{"id":"test","base_url":"` + upstream.URL + `","no_auth":true}]`)
	models := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), upstream.Client())
	discoverModelsForTest(t, models)
	routes, _ := routing.New(filepath.Join(t.TempDir(), "config.json"))
	vault := credentials.NewFileOnly(filepath.Join(t.TempDir(), "credentials.json"))
	tracker := health.New()
	handler := New(routes, models, vault, tracker, Config{}, nil)

	status, started := handler.probes.Start(handler, false)
	if started {
		t.Fatalf("automatic probe should not start when only expensive capabilities exist: started=%t status=%#v", started, status)
	}
	if imageCalls.Load() != 0 || videoCalls.Load() != 0 {
		t.Fatalf("expensive probes should not be triggered automatically: image_calls=%d video_calls=%d", imageCalls.Load(), videoCalls.Load())
	}
}

func TestFailedHealthProbeDoesNotRemoveModelFromCatalog(t *testing.T) {
	for _, key := range provider.SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"broken"}]}`))
		case "/chat/completions":
			http.Error(w, "broken", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	registry, _ := provider.NewRegistry(`[{"id":"test","base_url":"` + upstream.URL + `","no_auth":true}]`)
	models := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), upstream.Client())
	discoverModelsForTest(t, models)
	routes, _ := routing.New(filepath.Join(t.TempDir(), "config.json"))
	handler := New(routes, models, credentials.NewFileOnly(filepath.Join(t.TempDir(), "credentials.json")), health.New(), Config{}, nil)
	status, started := handler.probes.Start(handler, true)
	if !started || status.Total != 1 {
		t.Fatalf("started=%t status=%#v", started, status)
	}
	deadline := time.Now().Add(time.Second)
	for handler.probes.Snapshot().Status == "running" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(models.Models()) != 1 {
		t.Fatalf("failed model should remain in catalog: models=%#v", models.Models())
	}
	if handler.probes.Snapshot().Failed != 1 {
		t.Fatalf("probe should report failure: probe=%#v", handler.probes.Snapshot())
	}
}

func discoverModelsForTest(t *testing.T, store *catalog.Store) {
	t.Helper()
	models, failures := store.DiscoverFromProviders(context.Background())
	if len(failures) > 0 || len(models) == 0 {
		t.Fatalf("models=%d failures=%#v", len(models), failures)
	}
}
