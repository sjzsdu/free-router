package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/textproto"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/provider"
)

type Config struct {
	MaxAttempts int
}

type Gateway struct {
	catalog  *catalog.Store
	registry *provider.Registry
	config   Config
	client   *http.Client
	next     atomic.Uint64
	mux      *http.ServeMux
}

func New(store *catalog.Store, registry *provider.Registry, config Config, client *http.Client) *Gateway {
	gateway := &Gateway{catalog: store, registry: registry, config: config, client: client, mux: http.NewServeMux()}
	gateway.mux.HandleFunc("GET /healthz", gateway.health)
	gateway.mux.HandleFunc("GET /v1/models", gateway.models)
	gateway.mux.HandleFunc("POST /v1/chat/completions", gateway.chat)
	return gateway
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) { g.mux.ServeHTTP(w, r) }

func (g *Gateway) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "free_models": len(g.catalog.Models()), "providers": len(g.registry.All()),
	})
}

func (g *Gateway) models(w http.ResponseWriter, _ *http.Request) {
	models := g.catalog.Models()
	data := make([]map[string]any, 0, len(models)+1)
	data = append(data, map[string]any{
		"id": "auto", "object": "model", "owned_by": "free-router", "type": "normal", "free": true,
		"capabilities": catalog.Capabilities{
			ToolCall: true, ToolCallKnown: true, Reasoning: true, ReasoningKnown: true,
			Vision: true, VisionKnown: true, Streaming: true,
		},
	})
	for _, model := range models {
		data = append(data, map[string]any{
			"id": model.ID, "object": "model", "owned_by": model.Provider, "provider": model.Provider,
			"upstream_id": model.UpstreamID, "upstream_owned_by": model.OwnedBy,
			"name": model.Name, "description": model.Description, "created": model.Created,
			"type": model.Type, "free": model.Free, "tier": model.Tier,
			"context_length": model.ContextLength, "max_output_tokens": model.MaxOutputTokens,
			"input_modalities": model.InputModalities, "output_modalities": model.OutputModalities,
			"capabilities": model.Capabilities, "supported_parameters": model.SupportedParameters,
			"supported_endpoints": model.SupportedEndpoints, "pricing": model.Pricing,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (g *Gateway) chat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "request body is too large or unreadable")
		return
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	requested, _ := request["model"].(string)
	if requested == "" {
		requested = "auto"
	}
	_, needsTools := request["tools"]
	candidates := g.candidates(requested, needsTools)
	if len(candidates) == 0 {
		writeError(w, http.StatusNotFound, fmt.Sprintf("free model %q was not found", requested))
		return
	}

	for index, model := range candidates {
		request["model"] = model.UpstreamID
		payload, err := json.Marshal(request)
		if err != nil {
			writeError(w, http.StatusBadRequest, "could not encode request")
			return
		}
		resp, err := g.forward(r, model, payload)
		if err != nil {
			slog.Warn("provider request failed", "provider", model.Provider, "model", model.UpstreamID, "error", err)
			if index+1 < len(candidates) {
				continue
			}
			writeError(w, http.StatusBadGateway, "all configured free providers failed")
			return
		}
		if retryable(resp.StatusCode, requested == "auto" || requested == "free") && index+1 < len(candidates) {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			slog.Info("free provider unavailable; trying next", "provider", model.Provider, "model", model.UpstreamID, "status", resp.StatusCode)
			continue
		}
		copyResponse(w, resp, model)
		return
	}
	writeError(w, http.StatusBadGateway, "all configured free providers failed")
}

func (g *Gateway) candidates(requested string, needsTools bool) []catalog.Model {
	if requested != "auto" && requested != "free" {
		if model, ok := g.catalog.Find(requested); ok {
			return []catalog.Model{model}
		}
		return nil
	}
	groups := make(map[string][]catalog.Model)
	var preferred *catalog.Model
	for _, model := range g.catalog.Models() {
		if !model.IsTextChat() || (needsTools && !model.Supports("tools")) {
			continue
		}
		if model.Provider == "openrouter" && model.UpstreamID == "openrouter/free" {
			copy := model
			preferred = &copy
			continue
		}
		groups[model.Provider] = append(groups[model.Provider], model)
	}
	providerIDs := make([]string, 0, len(groups))
	for id := range groups {
		providerIDs = append(providerIDs, id)
	}
	sort.Strings(providerIDs)
	if len(providerIDs) == 0 && preferred == nil {
		return nil
	}
	result := make([]catalog.Model, 0, g.config.MaxAttempts)
	if preferred != nil {
		result = append(result, *preferred)
	}
	if len(providerIDs) == 0 || len(result) == g.config.MaxAttempts {
		return result
	}
	seed := int(g.next.Add(1) - 1)
	for round := 0; len(result) < g.config.MaxAttempts; round++ {
		added := false
		for offset := range providerIDs {
			providerID := providerIDs[(seed+offset)%len(providerIDs)]
			models := groups[providerID]
			if round >= len(models) {
				continue
			}
			result = append(result, models[(seed+round)%len(models)])
			added = true
			if len(result) == g.config.MaxAttempts {
				break
			}
		}
		if !added {
			break
		}
	}
	return result
}

func (g *Gateway) forward(original *http.Request, model catalog.Model, body []byte) (*http.Response, error) {
	spec, ok := g.registry.Get(model.Provider)
	if !ok {
		return nil, fmt.Errorf("provider %s is not configured", model.Provider)
	}
	req, err := http.NewRequestWithContext(original.Context(), http.MethodPost, spec.ChatEndpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	headers := make(map[string]string, len(spec.Headers)+4)
	for key, value := range spec.Headers {
		headers[key] = value
	}
	spec.ApplyAuth(headers)
	headers["Content-Type"] = "application/json"
	headers["User-Agent"] = "free-router/0.2"
	if accept := original.Header.Get("Accept"); accept != "" {
		headers["Accept"] = accept
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	return g.client.Do(req)
}

func copyResponse(w http.ResponseWriter, resp *http.Response, model catalog.Model) {
	defer resp.Body.Close()
	for key, values := range resp.Header {
		if isHopByHop(key) {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("X-Free-Router-Provider", model.Provider)
	w.Header().Set("X-Free-Router-Model", model.UpstreamID)
	w.WriteHeader(resp.StatusCode)
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		copyStream(w, resp.Body)
		return
	}
	_, _ = io.Copy(w, resp.Body)
}

func copyStream(w http.ResponseWriter, body io.Reader) {
	buffer := make([]byte, 32*1024)
	flusher, canFlush := w.(http.Flusher)
	for {
		read, err := body.Read(buffer)
		if read > 0 {
			_, _ = w.Write(buffer[:read])
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func retryable(status int, auto bool) bool {
	return (auto && (status == http.StatusBadRequest || status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusPaymentRequired || status == http.StatusNotFound || status == http.StatusUnprocessableEntity)) || status == http.StatusRequestTimeout || status == http.StatusConflict || status == http.StatusTooManyRequests || status >= 500
}

func isHopByHop(header string) bool {
	switch textproto.CanonicalMIMEHeaderKey(header) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	default:
		return false
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message, "type": "free_router_error"}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
