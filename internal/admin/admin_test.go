package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	handler := New(routes, models, vault, health.New(), Config{}, func() error { return registry.Reload(custom, vault.Get) })
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

func TestCredentialSaveDoesNotContactProviderCatalog(t *testing.T) {
	for _, key := range provider.SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
	started := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = w.Write([]byte(`{"data":[{"id":"chat-a"}]}`))
	}))
	defer upstream.Close()
	custom := `[{"id":"test","base_url":"` + upstream.URL + `"}]`
	vault := credentials.NewFileOnly(filepath.Join(t.TempDir(), "credentials.json"))
	registry, _ := provider.NewRegistry(custom, vault.Get)
	models := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), upstream.Client())
	routes, _ := routing.New(filepath.Join(t.TempDir(), "config.json"))
	handler := New(routes, models, vault, health.New(), Config{}, func() error { return registry.Reload(custom, vault.Get) })
	request := httptest.NewRequest(http.MethodPost, "/admin/api/credentials", bytes.NewBufferString(`{"provider":"test","api_key":"secret"}`))
	request.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		close(release)
		t.Fatalf("save waited for or failed with provider network: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	select {
	case <-started:
		close(release)
		t.Fatal("credential save contacted the Provider model catalog")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if len(models.Models()) != 0 {
		t.Fatal("models were discovered outside the Formula")
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

func TestAutomaticHealthProbeIncludesImageAndVideo(t *testing.T) {
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
	if !started || status.Total != 2 {
		t.Fatalf("started=%t status=%#v", started, status)
	}
	deadline := time.Now().Add(time.Second)
	for handler.probes.Snapshot().Status == "running" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	status = handler.probes.Snapshot()
	if status.Status != "completed" || status.Healthy != 2 || imageCalls.Load() != 1 || videoCalls.Load() != 1 {
		t.Fatalf("status=%#v image_calls=%d video_calls=%d health=%#v", status, imageCalls.Load(), videoCalls.Load(), tracker.Snapshot())
	}
}

func TestFailedHealthProbeRemovesModelFromCache(t *testing.T) {
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
	if len(models.Models()) != 0 || handler.probes.Snapshot().Failed != 1 {
		t.Fatalf("failed model remained: models=%#v probe=%#v", models.Models(), handler.probes.Snapshot())
	}
}

func discoverModelsForTest(t *testing.T, store *catalog.Store) {
	t.Helper()
	models, failures := store.DiscoverFromProviders(context.Background())
	if len(failures) > 0 || len(models) == 0 {
		t.Fatalf("models=%d failures=%#v", len(models), failures)
	}
}
