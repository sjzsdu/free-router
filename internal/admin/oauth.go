package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const oauthFlowTTL = 10 * time.Minute

type oauthFlow struct {
	verifier  string
	expiresAt time.Time
}

type oauthFlows struct {
	mu    sync.Mutex
	items map[string]oauthFlow
}

func newOAuthFlows() *oauthFlows {
	return &oauthFlows{items: make(map[string]oauthFlow)}
}

func (flows *oauthFlows) put(id, verifier string) {
	flows.mu.Lock()
	defer flows.mu.Unlock()
	now := time.Now()
	for key, flow := range flows.items {
		if now.After(flow.expiresAt) {
			delete(flows.items, key)
		}
	}
	flows.items[id] = oauthFlow{verifier: verifier, expiresAt: now.Add(oauthFlowTTL)}
}

func (flows *oauthFlows) consume(id string) (oauthFlow, bool) {
	flows.mu.Lock()
	defer flows.mu.Unlock()
	flow, ok := flows.items[id]
	delete(flows.items, id)
	return flow, ok && time.Now().Before(flow.expiresAt)
}

func randomOAuthToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func (h *Handler) startOpenRouterOAuth(w http.ResponseWriter, r *http.Request) {
	callbackBase, err := localOAuthCallback(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	flowID, err := randomOAuthToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot start OAuth login")
		return
	}
	verifier, err := randomOAuthToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot start OAuth login")
		return
	}
	h.oauthFlows.put(flowID, verifier)
	challengeHash := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeHash[:])
	callbackURL := callbackBase + "/" + flowID

	authorizationURL, err := url.Parse(h.openRouterAuthURL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "invalid OpenRouter OAuth endpoint")
		return
	}
	query := authorizationURL.Query()
	query.Set("callback_url", callbackURL)
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")
	authorizationURL.RawQuery = query.Encode()
	writeJSON(w, http.StatusOK, map[string]any{"provider": "openrouter", "authorization_url": authorizationURL.String()})
}

func localOAuthCallback(r *http.Request) (string, error) {
	host := r.Host
	if host == "" {
		return "", fmt.Errorf("OAuth login requires a localhost address")
	}
	hostname := host
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		hostname = parsed
	}
	hostname = strings.Trim(hostname, "[]")
	ip := net.ParseIP(hostname)
	if !strings.EqualFold(hostname, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return "", fmt.Errorf("OAuth login is only available from localhost")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + host + "/admin/oauth/openrouter/callback", nil
}

func (h *Handler) finishOpenRouterOAuth(w http.ResponseWriter, r *http.Request, flowID string) {
	flow, ok := h.oauthFlows.consume(flowID)
	if !ok {
		h.oauthRedirect(w, r, false, "授权已过期，请重新登录")
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		h.oauthRedirect(w, r, false, "OpenRouter 未返回授权码")
		return
	}
	key, err := h.exchangeOpenRouterCode(r, code, flow.verifier)
	if err != nil {
		h.oauthRedirect(w, r, false, err.Error())
		return
	}
	h.updateMu.Lock()
	defer h.updateMu.Unlock()
	oldKey, _ := h.vault.Get("openrouter")
	if _, err := h.vault.Set("openrouter", key); err != nil {
		h.oauthRedirect(w, r, false, "无法安全保存 OpenRouter 凭据")
		return
	}
	if h.reload != nil {
		rollback, err := h.reload(h.routes.Config().ProviderEnv)
		if err != nil {
			if oldKey != "" {
				_, _ = h.vault.Set("openrouter", oldKey)
			} else {
				_ = h.vault.Delete("openrouter")
			}
			if rollback != nil {
				rollback()
			}
			h.oauthRedirect(w, r, false, "凭据已保存，但 Provider 热加载失败")
			return
		}
	}
	h.oauthRedirect(w, r, true, "")
}

func (h *Handler) exchangeOpenRouterCode(r *http.Request, code, verifier string) (string, error) {
	body, err := json.Marshal(map[string]string{
		"code": code, "code_verifier": verifier, "code_challenge_method": "S256",
	})
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, h.openRouterTokenURL, strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := h.oauthHTTPClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("无法连接 OpenRouter 授权服务")
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return "", fmt.Errorf("无法读取 OpenRouter 授权响应")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("OpenRouter 授权失败 (%s)", response.Status)
	}
	var result struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(content, &result); err != nil || strings.TrimSpace(result.Key) == "" {
		return "", fmt.Errorf("OpenRouter 授权响应缺少 API Key")
	}
	return result.Key, nil
}

func (h *Handler) oauthRedirect(w http.ResponseWriter, r *http.Request, success bool, message string) {
	query := url.Values{"oauth_provider": {"openrouter"}}
	if success {
		query.Set("oauth_status", "success")
	} else {
		query.Set("oauth_status", "error")
		query.Set("oauth_message", message)
	}
	http.Redirect(w, r, "/admin/?"+query.Encode(), http.StatusSeeOther)
}
