package admin

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/credentials"
	"github.com/sjzsdu/free-router/internal/health"
	"github.com/sjzsdu/free-router/internal/provider"
	"github.com/sjzsdu/free-router/internal/routing"
)

func TestOpenRouterOAuthPKCEStoresKeyAndReloadsProvider(t *testing.T) {
	for _, key := range provider.SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
	var expectedChallenge string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			var input struct {
				Code                string `json:"code"`
				CodeVerifier        string `json:"code_verifier"`
				CodeChallengeMethod string `json:"code_challenge_method"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256([]byte(input.CodeVerifier))
			challenge := base64.RawURLEncoding.EncodeToString(digest[:])
			if input.Code != "authorized-code" || input.CodeChallengeMethod != "S256" || challenge != expectedChallenge {
				t.Fatalf("invalid token exchange: %#v challenge=%q", input, challenge)
			}
			_, _ = w.Write([]byte(`{"key":"oauth-secret"}`))
		case "/models":
			if r.Header.Get("Authorization") != "Bearer oauth-secret" {
				t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"free-model"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	vault := credentials.NewFileOnly(filepath.Join(t.TempDir(), "credentials.json"))
	custom := `[{"id":"openrouter","base_url":"` + upstream.URL + `"}]`
	registry, err := provider.NewRegistryAllowEmpty(custom, vault.Get)
	if err != nil {
		t.Fatal(err)
	}
	models := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), upstream.Client())
	routes, _ := routing.New(filepath.Join(t.TempDir(), "config.json"))
	handler := New(routes, models, vault, health.New(), Config{
		OAuthHTTPClient:    upstream.Client(),
		OpenRouterAuthURL:  "https://auth.example/authorize",
		OpenRouterTokenURL: upstream.URL + "/token",
	}, func() error { return registry.Reload(custom, vault.Get) })

	start := httptest.NewRequest(http.MethodPost, "/admin/api/oauth/openrouter/start", nil)
	start.Host = "localhost:1314"
	start.RemoteAddr = "127.0.0.1:1234"
	startRecorder := httptest.NewRecorder()
	handler.ServeHTTP(startRecorder, start)
	if startRecorder.Code != http.StatusOK {
		t.Fatalf("start status=%d body=%s", startRecorder.Code, startRecorder.Body.String())
	}
	var startResult struct {
		AuthorizationURL string `json:"authorization_url"`
	}
	if err := json.Unmarshal(startRecorder.Body.Bytes(), &startResult); err != nil {
		t.Fatal(err)
	}
	authorizationURL, err := url.Parse(startResult.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	expectedChallenge = authorizationURL.Query().Get("code_challenge")
	if authorizationURL.Query().Get("code_challenge_method") != "S256" || expectedChallenge == "" {
		t.Fatalf("authorization URL=%s", authorizationURL)
	}
	callbackURL, err := url.Parse(authorizationURL.Query().Get("callback_url"))
	if err != nil {
		t.Fatal(err)
	}
	callbackURL.RawQuery = "code=authorized-code"
	callback := httptest.NewRequest(http.MethodGet, callbackURL.RequestURI(), nil)
	callback.Host = "localhost:1314"
	callback.RemoteAddr = "127.0.0.1:1234"
	callbackRecorder := httptest.NewRecorder()
	handler.ServeHTTP(callbackRecorder, callback)
	if callbackRecorder.Code != http.StatusSeeOther || !strings.Contains(callbackRecorder.Header().Get("Location"), "oauth_status=success") {
		t.Fatalf("callback status=%d location=%q body=%s", callbackRecorder.Code, callbackRecorder.Header().Get("Location"), callbackRecorder.Body.String())
	}
	if key, ok := vault.Get("openrouter"); !ok || key != "oauth-secret" {
		t.Fatalf("saved key=%q ok=%v", key, ok)
	}
	if _, ok := registry.Get("openrouter"); !ok || len(models.Models()) != 1 {
		t.Fatalf("provider enabled=%v models=%d", ok, len(models.Models()))
	}

	replayRecorder := httptest.NewRecorder()
	handler.ServeHTTP(replayRecorder, callback)
	if replayRecorder.Code != http.StatusSeeOther || !strings.Contains(replayRecorder.Header().Get("Location"), "oauth_status=error") {
		t.Fatalf("replay status=%d location=%q", replayRecorder.Code, replayRecorder.Header().Get("Location"))
	}
}

func TestOpenRouterOAuthRequiresLocalhostHost(t *testing.T) {
	routes, _ := routing.New(filepath.Join(t.TempDir(), "config.json"))
	registry, _ := provider.NewRegistryAllowEmpty("")
	models := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), http.DefaultClient)
	vault := credentials.NewFileOnly(filepath.Join(t.TempDir(), "credentials.json"))
	handler := New(routes, models, vault, health.New(), Config{}, nil)
	request := httptest.NewRequest(http.MethodPost, "/admin/api/oauth/openrouter/start", nil)
	request.Host = "attacker.example"
	request.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
