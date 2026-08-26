package admin

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/credentials"
	"github.com/sjzsdu/free-router/internal/eligibility"
	"github.com/sjzsdu/free-router/internal/health"
	"github.com/sjzsdu/free-router/internal/provider"
	"github.com/sjzsdu/free-router/internal/routing"
)

//go:embed dist
var assets embed.FS

type Handler struct {
	updateMu           sync.Mutex
	routes             *routing.Store
	catalog            *catalog.Store
	vault              *credentials.Vault
	config             Config
	reload             func(map[string][]string) (func(), error)
	health             *health.Tracker
	probes             *probeManager
	providerProbes     *providerProbeStore
	static             http.Handler
	started            time.Time
	oauthFlows         *oauthFlows
	oauthHTTPClient    *http.Client
	openRouterAuthURL  string
	openRouterTokenURL string
	configService      *ConfigService
	credentialService  *CredentialService
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

func New(routes *routing.Store, models *catalog.Store, vault *credentials.Vault, tracker *health.Tracker, config Config, reload func(map[string][]string) (func(), error)) *Handler {
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
	handler := &Handler{routes: routes, catalog: models, vault: vault, health: tracker, probes: newProbeManager(filepath.Dir(routes.Path())), providerProbes: newProviderProbeStore(), config: config, reload: reload, static: http.FileServer(http.FS(staticFS)), started: time.Now(), oauthFlows: newOAuthFlows(), oauthHTTPClient: client, openRouterAuthURL: authURL, openRouterTokenURL: tokenURL}
	handler.configService = &ConfigService{mu: &handler.updateMu, routes: routes, catalog: models, health: tracker, reload: handler.reloadProviders, refreshAsync: handler.refreshAllAsync}
	handler.credentialService = &CredentialService{
		mu: &handler.updateMu, vault: vault, routes: routes, catalog: models, reload: handler.reloadProviders,
		providerProbes: handler.providerProbes, markProviderFailed: handler.markProviderModelsFailed,
		resetProvider: handler.resetProviderModelHealth,
	}
	handler.syncVerifiedHealth()
	return handler
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
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/providers/"):
		h.providerDetails(w, strings.TrimPrefix(path, "/api/providers/"))
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

func (h *Handler) providerDetails(w http.ResponseWriter, escapedID string) {
	providerID, err := url.PathUnescape(escapedID)
	if err != nil || strings.TrimSpace(providerID) == "" || strings.Contains(providerID, "/") {
		writeError(w, http.StatusBadRequest, "invalid provider")
		return
	}
	details, err := provider.FreeProviderDetailsWithManifest(providerID, h.config.FreeModels)
	if errors.Is(err, provider.ErrProviderNotFound) {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, details)
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
	providers := provider.BuiltinStatusWithManifest(provider.EnvMap(h.routes.Config().ProviderEnv), h.config.FreeModels, h.vault.Get)
	h.providerProbes.decorate(providers)
	view := eligibility.New(h.catalog, h.routes, h.health)
	statuses := make([]ModelEligibility, 0)
	models := h.catalog.ConfiguredModels()
	for _, model := range models {
		for _, capability := range model.Functions {
			_, reason := view.Eligible(model, routing.Route{Capability: capability, RequireTool: capability == catalog.FunctionChatTools}, true, true)
			statuses = append(statuses, ModelEligibility{Model: model.ID, Capability: capability, Eligible: reason == "", Reason: reason})
		}
	}
	catalogStatus := h.catalog.Status()
	catalogStatus.Count = len(models)
	writeJSON(w, http.StatusOK, StateResponse{
		Config: h.routes.Config(), ConfigPath: h.routes.Path(), Models: models,
		Catalog: catalogStatus, Providers: providers, Credentials: entries,
		Health: h.health.Snapshot(), ProviderHealth: h.health.ProviderSnapshot(), Summary: h.health.Summary(),
		HealthProbe: h.probes.Snapshot(), Runtime: h.runtimeState(), Eligibility: statuses,
	})
}

func (h *Handler) runtime(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, h.runtimeState())
}

func (h *Handler) runtimeState() RuntimeState {
	manager := strings.TrimSpace(os.Getenv("FREE_ROUTER_SERVICE_MANAGER"))
	if manager == "" {
		manager = "manual"
	}
	summary := h.health.Summary()
	return RuntimeState{
		Status: "running", PID: os.Getpid(), Version: h.config.Version,
		StartedAt: h.started, UptimeSeconds: int64(time.Since(h.started).Seconds()),
		ServiceManager: manager, Models: len(h.catalog.ConfiguredModels()),
		Requests: summary.Requests, Failed: summary.Failed,
	}
}

func (h *Handler) updateConfig(w http.ResponseWriter, r *http.Request) {
	var config routing.Config
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&config); err != nil {
		writeError(w, http.StatusBadRequest, "invalid configuration")
		return
	}

	result, err := h.configService.Update(config)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, routing.ErrConfigConflict) {
			status = http.StatusConflict
		} else {
			var serviceErr *ServiceError
			if errors.As(err, &serviceErr) && serviceErr.Stage == "reset-model-verification" {
				status = http.StatusInternalServerError
			}
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	if err := h.catalog.Refresh(r.Context()); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	h.syncVerifiedHealth()
	writeJSON(w, http.StatusOK, map[string]any{"refreshed": true, "models": len(h.catalog.ConfiguredModels())})
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
	if err := h.catalog.ResetCapabilityVerification(input.Model, input.Capability); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
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
	latency := time.Since(started)
	if err != nil {
		status, message := providerProbeFailure(providerID, err)
		h.providerProbes.failure(providerID, status, message, latency)
		h.markProviderModelsFailed(providerID, status, message, latency)
		writeError(w, http.StatusBadGateway, message)
		return
	}
	h.providerProbes.success(providerID, count, latency)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "provider": providerID, "formula_models": count, "latency_ms": latency.Milliseconds()})
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
	result, err := h.credentialService.Save(r.Context(), input.Provider, input.APIKey)
	if err != nil {
		status := http.StatusInternalServerError
		var serviceErr *ServiceError
		if errors.As(err, &serviceErr) && serviceErr.Stage == "save-credential" {
			status = http.StatusBadRequest
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) deleteCredential(w http.ResponseWriter, r *http.Request, providerID string) {
	if providerID == "" {
		writeError(w, http.StatusBadRequest, "provider is required")
		return
	}
	result, err := h.credentialService.Delete(providerID)
	if err != nil {
		status := http.StatusInternalServerError
		var serviceErr *ServiceError
		if errors.As(err, &serviceErr) && serviceErr.Stage == "delete-credential" {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func providerProbeFailure(providerID string, err error) (int, string) {
	status := 0
	var probeError *catalog.ProviderProbeError
	if errors.As(err, &probeError) {
		status = probeError.Status
	}
	message := err.Error()
	if providerID == "groq" && strings.Contains(message, "403") {
		message += "; Groq 403 表示账户、组织或项目权限受限（无效 Key 通常返回 401），并非模型目录地址错误；请确认 Key 所属项目可用，并检查 Organization / Project 的 Model Permissions，必要时重新创建 Key 或联系 Groq"
	}
	return status, message
}

func (h *Handler) reloadProviders(providerEnv map[string][]string) (func(), error) {
	if h.reload != nil {
		return h.reload(providerEnv)
	}
	return nil, nil
}

func (h *Handler) refreshAllAsync() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := h.catalog.Refresh(ctx); err != nil {
			slog.Warn("background model catalog refresh failed", "error", err)
			return
		}
		h.syncVerifiedHealth()
	}()
}

func (h *Handler) syncVerifiedHealth() {
	models := make(map[string]catalog.Model, len(h.catalog.Models()))
	for _, model := range h.catalog.Models() {
		if effective, enabled := h.routes.Apply(model); enabled {
			models[model.ID] = effective
		}
	}
	existing := make(map[string]bool)
	for _, state := range h.health.Snapshot() {
		key := state.Model + "\x00" + state.Capability
		if !state.Verified {
			continue
		}
		existing[key] = true
		model, ok := models[state.Model]
		if !ok || !h.catalog.ModelCapabilityVerified(model, state.Capability) {
			h.health.Reset(state.Model, state.Capability)
			delete(existing, key)
		}
	}
	for _, verification := range h.catalog.CapabilityVerifications() {
		key := verification.Model + "\x00" + verification.Capability
		if existing[key] {
			continue
		}
		model, ok := models[verification.Model]
		if !ok || !h.catalog.ModelCapabilityVerified(model, verification.Capability) {
			continue
		}
		h.health.RestoreProbeSuccess(verification.Model, verification.Capability, verification.CheckedAt, verification.LatencyMS)
	}
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
