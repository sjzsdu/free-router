package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/provider"
	"github.com/sjzsdu/free-router/internal/routing"
)

type Config struct {
	MaxAttempts int
	Routes      *routing.Store
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
	gateway.mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		gateway.proxyJSON(w, r, "/chat/completions", "chat")
	})
	gateway.mux.HandleFunc("POST /v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		gateway.proxyJSON(w, r, "/embeddings", "embedding")
	})
	gateway.mux.HandleFunc("POST /v1/audio/speech", func(w http.ResponseWriter, r *http.Request) {
		gateway.proxyJSON(w, r, "/audio/speech", "audio")
	})
	gateway.mux.HandleFunc("POST /v1/audio/transcriptions", func(w http.ResponseWriter, r *http.Request) {
		gateway.proxyMultipart(w, r, "/audio/transcriptions", "audio")
	})
	gateway.mux.HandleFunc("POST /v1/audio/translations", func(w http.ResponseWriter, r *http.Request) {
		gateway.proxyMultipart(w, r, "/audio/translations", "audio")
	})
	gateway.mux.HandleFunc("POST /v1/images/generations", func(w http.ResponseWriter, r *http.Request) {
		gateway.proxyJSON(w, r, "/images/generations", "image")
	})
	gateway.mux.HandleFunc("POST /v1/videos/generations", func(w http.ResponseWriter, r *http.Request) {
		gateway.proxyJSON(w, r, "/videos/generations", "video")
	})
	gateway.mux.HandleFunc("POST /v1/rerank", func(w http.ResponseWriter, r *http.Request) {
		gateway.proxyJSON(w, r, "/rerank", "rerank")
	})
	gateway.mux.HandleFunc("POST /v1/moderations", func(w http.ResponseWriter, r *http.Request) {
		gateway.proxyJSON(w, r, "/moderations", "moderation")
	})
	return gateway
}

func (g *Gateway) Handle(pattern string, handler http.Handler) { g.mux.Handle(pattern, handler) }

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) { g.mux.ServeHTTP(w, r) }

func (g *Gateway) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "free_models": len(g.catalog.Models()), "providers": len(g.registry.All()),
	})
}

func (g *Gateway) models(w http.ResponseWriter, _ *http.Request) {
	models := g.catalog.Models()
	data := make([]map[string]any, 0, len(models)+10)
	if g.config.Routes != nil {
		config := g.config.Routes.Config()
		aliases := g.config.Routes.Aliases()
		for _, alias := range aliases {
			route := config.Routes[alias]
			data = append(data, map[string]any{
				"id": alias, "object": "model", "owned_by": "free-router", "type": route.Type,
				"free": true, "route": true, "fallback_models": route.Models,
				"capabilities": catalog.Capabilities{ToolCall: route.RequireTool, ToolCallKnown: route.RequireTool, Streaming: true},
			})
		}
	} else {
		data = append(data, map[string]any{"id": "auto", "object": "model", "owned_by": "free-router", "type": "normal", "free": true})
	}
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

func (g *Gateway) proxyJSON(w http.ResponseWriter, r *http.Request, endpoint, defaultAlias string) {
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
		requested = defaultAlias
	}
	_, needsTools := request["tools"]
	if (requested == "auto" || requested == "free") && defaultAlias == "chat" && needsTools {
		requested = "chat-tools"
	} else if requested == "auto" || requested == "free" {
		requested = defaultAlias
	}
	candidates, fallback := g.candidates(requested, needsTools)
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
		resp, err := g.forward(r, model, payload, endpoint, "application/json")
		if err != nil {
			slog.Warn("provider request failed", "provider", model.Provider, "model", model.UpstreamID, "error", err)
			if index+1 < len(candidates) {
				continue
			}
			writeError(w, http.StatusBadGateway, "all configured free providers failed")
			return
		}
		if retryable(resp.StatusCode, fallback) && index+1 < len(candidates) {
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

func (g *Gateway) proxyMultipart(w http.ResponseWriter, r *http.Request, endpoint, defaultAlias string) {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		writeError(w, http.StatusBadRequest, "multipart/form-data with a boundary is required")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "request body is too large or unreadable")
		return
	}
	requested, err := multipartModel(body, params["boundary"])
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid multipart body")
		return
	}
	if requested == "" || requested == "auto" || requested == "free" {
		requested = defaultAlias
	}
	candidates, fallback := g.candidates(requested, false)
	if len(candidates) == 0 {
		writeError(w, http.StatusNotFound, fmt.Sprintf("free model %q was not found", requested))
		return
	}
	for index, model := range candidates {
		payload, contentType, err := rewriteMultipartModel(body, params["boundary"], model.UpstreamID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "could not encode multipart request")
			return
		}
		resp, err := g.forward(r, model, payload, endpoint, contentType)
		if err != nil {
			if index+1 < len(candidates) {
				continue
			}
			writeError(w, http.StatusBadGateway, "all configured free providers failed")
			return
		}
		if retryable(resp.StatusCode, fallback) && index+1 < len(candidates) {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			continue
		}
		copyResponse(w, resp, model)
		return
	}
}

func multipartModel(body []byte, boundary string) (string, error) {
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if part.FormName() == "model" {
			value, err := io.ReadAll(io.LimitReader(part, 16<<10))
			part.Close()
			return strings.TrimSpace(string(value)), err
		}
		part.Close()
	}
}

func rewriteMultipartModel(body []byte, boundary, model string) ([]byte, string, error) {
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var output bytes.Buffer
	writer := multipart.NewWriter(&output)
	if err := writer.SetBoundary(boundary); err != nil {
		return nil, "", err
	}
	found := false
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", err
		}
		target, err := writer.CreatePart(part.Header)
		if err != nil {
			part.Close()
			return nil, "", err
		}
		if part.FormName() == "model" {
			_, err = io.WriteString(target, model)
			found = true
		} else {
			_, err = io.Copy(target, part)
		}
		part.Close()
		if err != nil {
			return nil, "", err
		}
	}
	if !found {
		if err := writer.WriteField("model", model); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return output.Bytes(), writer.FormDataContentType(), nil
}

func (g *Gateway) candidates(requested string, needsTools bool) ([]catalog.Model, bool) {
	if g.config.Routes == nil {
		if route, ok := routing.DefaultConfig().Routes[requested]; ok {
			return g.dynamicCandidates(route.Type, needsTools || route.RequireTool), true
		}
	}
	if g.config.Routes != nil {
		if route, ok := g.config.Routes.Route(requested); ok {
			if route.RequireTool {
				needsTools = true
			}
			if len(route.Models) > 0 {
				result := make([]catalog.Model, 0, min(len(route.Models), g.config.MaxAttempts))
				for _, id := range route.Models {
					model, ok := g.catalog.Find(id)
					if !ok || model.Type != route.Type || (needsTools && !model.Supports("tools")) {
						continue
					}
					result = append(result, model)
					if len(result) == g.config.MaxAttempts {
						break
					}
				}
				return result, true
			}
			return g.dynamicCandidates(route.Type, needsTools), true
		}
	}
	if model, ok := g.catalog.Find(requested); ok {
		return []catalog.Model{model}, false
	}
	return nil, false
}

func (g *Gateway) dynamicCandidates(modelType string, needsTools bool) []catalog.Model {
	groups := make(map[string][]catalog.Model)
	var preferred *catalog.Model
	for _, model := range g.catalog.Models() {
		if model.Type != modelType || (needsTools && !model.Supports("tools")) {
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

func (g *Gateway) forward(original *http.Request, model catalog.Model, body []byte, endpoint, contentType string) (*http.Response, error) {
	spec, ok := g.registry.Get(model.Provider)
	if !ok {
		return nil, fmt.Errorf("provider %s is not configured", model.Provider)
	}
	req, err := http.NewRequestWithContext(original.Context(), http.MethodPost, spec.APIEndpoint(endpoint), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	headers := make(map[string]string, len(spec.Headers)+4)
	for key, value := range spec.Headers {
		headers[key] = value
	}
	spec.ApplyAuth(headers)
	headers["Content-Type"] = contentType
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
