package admin

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/credentials"
	"github.com/sjzsdu/free-router/internal/health"
	"github.com/sjzsdu/free-router/internal/provider"
	"github.com/sjzsdu/free-router/internal/routing"
)

//go:embed dist
var assets embed.FS

type Handler struct {
	routes             *routing.Store
	catalog            *catalog.Store
	vault              *credentials.Vault
	config             Config
	reload             func() error
	health             *health.Tracker
	probes             *probeManager
	static             http.Handler
	started            time.Time
	oauthFlows         *oauthFlows
	oauthHTTPClient    *http.Client
	openRouterAuthURL  string
	openRouterTokenURL string
}

type Config struct {
	AllowRemote        bool
	Token              string
	Version            string
	FreeModels         string
	OAuthHTTPClient    *http.Client
	OpenRouterAuthURL  string
	OpenRouterTokenURL string
}

func New(routes *routing.Store, models *catalog.Store, vault *credentials.Vault, tracker *health.Tracker, config Config, reload func() error) *Handler {
	staticFS, _ := fs.Sub(assets, "dist")
	client := config.OAuthHTTPClient
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	authURL := config.OpenRouterAuthURL
	if authURL == "" {
		authURL = "https://openrouter.ai/auth"
	}
	tokenURL := config.OpenRouterTokenURL
	if tokenURL == "" {
		tokenURL = "https://openrouter.ai/api/v1/auth/keys"
	}
	return &Handler{routes: routes, catalog: models, vault: vault, health: tracker, probes: newProbeManager(), config: config, reload: reload, static: http.FileServer(http.FS(staticFS)), started: time.Now(), oauthFlows: newOAuthFlows(), oauthHTTPClient: client, openRouterAuthURL: authURL, openRouterTokenURL: tokenURL}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	secureHeaders(w)
	loopback := isLoopback(r.RemoteAddr)
	if !loopback && !h.config.AllowRemote {
		http.Error(w, "admin UI is restricted to localhost", http.StatusForbidden)
		return
	}
	if h.config.Token != "" && !h.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="free-router admin", charset="UTF-8"`)
		http.Error(w, "admin authentication required", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
		http.Error(w, "cross-origin admin request rejected", http.StatusForbidden)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/admin")
	switch {
	case r.Method == http.MethodGet && path == "/api/state":
		h.state(w)
	case r.Method == http.MethodGet && path == "/api/runtime":
		h.runtime(w)
	case r.Method == http.MethodPut && path == "/api/config":
		h.updateConfig(w, r)
	case r.Method == http.MethodPost && path == "/api/refresh":
		h.refresh(w, r)
	case r.Method == http.MethodPost && path == "/api/health/reset":
		h.resetHealth(w, r)
	case r.Method == http.MethodPost && path == "/api/health/probe":
		h.startHealthProbe(w, r)
	case r.Method == http.MethodPost && path == "/api/health/probe/model":
		h.startModelHealthProbe(w, r)
	case r.Method == http.MethodPost && path == "/api/oauth/openrouter/start":
		h.startOpenRouterOAuth(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/oauth/openrouter/callback/"):
		h.finishOpenRouterOAuth(w, r, strings.TrimPrefix(path, "/oauth/openrouter/callback/"))
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/providers/") && strings.HasSuffix(path, "/test"):
		h.testProvider(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/api/providers/"), "/test"))
	case r.Method == http.MethodPost && path == "/api/credentials":
		h.saveCredential(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/api/credentials/"):
		h.deleteCredential(w, r, strings.TrimPrefix(path, "/api/credentials/"))
	case (r.Method == http.MethodGet || r.Method == http.MethodHead) && (path == "" || path == "/"):
		h.serveIndex(w, r)
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		r.URL.Path = "/" + strings.TrimPrefix(path, "/")
		h.static.ServeHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) authorized(r *http.Request) bool {
	provided := ""
	if username, password, ok := r.BasicAuth(); ok && username == "admin" {
		provided = password
	} else if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		provided = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(h.config.Token)) == 1
}

func (h *Handler) state(w http.ResponseWriter) {
	entries, _ := h.vault.List()
	writeJSON(w, http.StatusOK, map[string]any{
		"config": h.routes.Config(), "config_path": h.routes.Path(), "models": h.catalog.Models(),
		"catalog": h.catalog.Status(), "providers": provider.BuiltinStatusWithManifest(provider.EnvMap(h.routes.Config().ProviderEnv), h.config.FreeModels, h.vault.Get), "credentials": entries,
		"health": h.health.Snapshot(), "summary": h.health.Summary(), "health_probe": h.probes.Snapshot(), "runtime": h.runtimeState(),
	})
}

func (h *Handler) runtime(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, h.runtimeState())
}

func (h *Handler) runtimeState() map[string]any {
	manager := strings.TrimSpace(os.Getenv("FREE_ROUTER_SERVICE_MANAGER"))
	if manager == "" {
		manager = "manual"
	}
	return map[string]any{
		"status": "running", "pid": os.Getpid(), "version": h.config.Version,
		"started_at": h.started, "uptime_seconds": int64(time.Since(h.started).Seconds()),
		"service_manager": manager, "models": len(h.catalog.Models()),
		"requests": h.health.Summary().Requests, "failed": h.health.Summary().Failed,
	}
}

func (h *Handler) updateConfig(w http.ResponseWriter, r *http.Request) {
	var config routing.Config
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&config); err != nil {
		writeError(w, http.StatusBadRequest, "invalid configuration")
		return
	}

	previousConfig := h.routes.Config()

	validateFunc := func(newConfig routing.Config) error {
		if !reflect.DeepEqual(previousConfig.ProviderEnv, newConfig.ProviderEnv) {
			if err := h.reloadProviders(); err != nil {
				return err
			}
		}
		return nil
	}

	if err := h.routes.UpdateTransactional(config, validateFunc); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	if !reflect.DeepEqual(previousConfig.ProviderEnv, h.routes.Config().ProviderEnv) {
		h.refreshAllAsync()
	}
	writeJSON(w, http.StatusOK, map[string]any{"saved": true, "config": h.routes.Config()})
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	if err := h.catalog.Refresh(r.Context()); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"refreshed": true, "models": len(h.catalog.Models())})
}

func (h *Handler) resetHealth(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Model      string `json:"model"`
		Capability string `json:"capability"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&input); err != nil || strings.TrimSpace(input.Model) == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	if err := h.catalog.RestoreModel(input.Model); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	h.health.Reset(input.Model, input.Capability)
	writeJSON(w, http.StatusOK, map[string]any{"reset": true, "model": input.Model, "capability": input.Capability})
}

func (h *Handler) testProvider(w http.ResponseWriter, r *http.Request, escapedID string) {
	providerID, err := url.PathUnescape(escapedID)
	if err != nil || providerID == "" {
		writeError(w, http.StatusBadRequest, "invalid provider")
		return
	}
	started := time.Now()
	count, err := h.catalog.Probe(r.Context(), providerID)
	if err != nil {
		message := err.Error()
		if providerID == "groq" && strings.Contains(message, "403") {
			message += "; Groq 403 表示账户、组织或项目权限受限（无效 Key 通常返回 401），并非模型目录地址错误；请确认 Key 所属项目可用，并检查 Organization / Project 的 Model Permissions，必要时重新创建 Key 或联系 Groq"
		}
		writeError(w, http.StatusBadGateway, message)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "provider": providerID, "formula_models": count, "latency_ms": time.Since(started).Milliseconds()})
}

func (h *Handler) saveCredential(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Provider string `json:"provider"`
		APIKey   string `json:"api_key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid credential")
		return
	}
	backend, err := h.vault.Set(input.Provider, input.APIKey)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.reloadProviders(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"saved": true, "backend": backend, "models": len(h.catalog.Models())})
}

func (h *Handler) deleteCredential(w http.ResponseWriter, r *http.Request, providerID string) {
	if providerID == "" {
		writeError(w, http.StatusBadRequest, "provider is required")
		return
	}
	if err := h.vault.Delete(providerID); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err := h.reloadProviders(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": true, "models": len(h.catalog.Models())})
}

func (h *Handler) reloadProviders() error {
	if h.reload != nil {
		if err := h.reload(); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) refreshAllAsync() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := h.catalog.Refresh(ctx); err != nil {
			slog.Warn("background model catalog refresh failed", "error", err)
		}
	}()
}

func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	content, err := assets.ReadFile("dist/index.html")
	if err != nil {
		http.Error(w, "admin UI unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(content)
}

func isLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && strings.EqualFold(parsed.Host, r.Host)
}

func secureHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
