package admin

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"strings"

	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/credentials"
	"github.com/sjzsdu/free-router/internal/provider"
	"github.com/sjzsdu/free-router/internal/routing"
)

//go:embed static/*
var assets embed.FS

type Handler struct {
	routes      *routing.Store
	catalog     *catalog.Store
	vault       *credentials.Vault
	allowRemote bool
	reload      func() error
	static      http.Handler
}

func New(routes *routing.Store, models *catalog.Store, vault *credentials.Vault, allowRemote bool, reload func() error) *Handler {
	staticFS, _ := fs.Sub(assets, "static")
	return &Handler{routes: routes, catalog: models, vault: vault, allowRemote: allowRemote, reload: reload, static: http.FileServer(http.FS(staticFS))}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.allowRemote && !isLoopback(r.RemoteAddr) {
		http.Error(w, "admin UI is restricted to localhost", http.StatusForbidden)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/admin")
	switch {
	case r.Method == http.MethodGet && path == "/api/state":
		h.state(w)
	case r.Method == http.MethodPut && path == "/api/config":
		h.updateConfig(w, r)
	case r.Method == http.MethodPost && path == "/api/refresh":
		h.refresh(w, r)
	case r.Method == http.MethodPost && path == "/api/credentials":
		h.saveCredential(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/api/credentials/"):
		h.deleteCredential(w, r, strings.TrimPrefix(path, "/api/credentials/"))
	case r.Method == http.MethodGet && (path == "" || path == "/"):
		h.serveIndex(w, r)
	case r.Method == http.MethodGet:
		r.URL.Path = "/" + strings.TrimPrefix(path, "/")
		h.static.ServeHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) state(w http.ResponseWriter) {
	entries, _ := h.vault.List()
	writeJSON(w, http.StatusOK, map[string]any{
		"config": h.routes.Config(), "config_path": h.routes.Path(), "models": h.catalog.Models(),
		"catalog": h.catalog.Status(), "providers": provider.BuiltinStatus(h.vault.Get), "credentials": entries,
	})
}

func (h *Handler) updateConfig(w http.ResponseWriter, r *http.Request) {
	var config routing.Config
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&config); err != nil {
		writeError(w, http.StatusBadRequest, "invalid configuration")
		return
	}
	if err := h.routes.Update(config); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
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
	if err := h.reloadProviders(r); err != nil {
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
	if err := h.reloadProviders(r); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": true, "models": len(h.catalog.Models())})
}

func (h *Handler) reloadProviders(r *http.Request) error {
	if h.reload != nil {
		if err := h.reload(); err != nil {
			return err
		}
	}
	return h.catalog.Refresh(r.Context())
}

func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	content, err := assets.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "admin UI unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
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

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
