package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

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
	if err := models.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
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
	registry, _ := provider.NewRegistryAllowEmpty("")
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

func TestCredentialSaveHotReloadsProviderAndCatalog(t *testing.T) {
	for _, key := range provider.SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"chat-a"}]}`))
	}))
	defer upstream.Close()
	custom := `[{"id":"test","base_url":"` + upstream.URL + `"}]`
	vault := credentials.NewFileOnly(filepath.Join(t.TempDir(), "credentials.json"))
	registry, err := provider.NewRegistryAllowEmpty(custom, vault.Get)
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
	if _, ok := registry.Get("test"); !ok || len(models.Models()) != 1 {
		t.Fatalf("provider enabled=%v models=%d", ok, len(models.Models()))
	}
}

func TestAdminTokenAuthentication(t *testing.T) {
	routes, _ := routing.New(filepath.Join(t.TempDir(), "config.json"))
	registry, _ := provider.NewRegistryAllowEmpty("")
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

func TestAdminRejectsCrossOriginMutation(t *testing.T) {
	routes, _ := routing.New(filepath.Join(t.TempDir(), "config.json"))
	registry, _ := provider.NewRegistryAllowEmpty("")
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
