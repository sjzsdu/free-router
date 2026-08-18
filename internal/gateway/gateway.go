package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjzsdu/free-router/internal/adapter"
	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/health"
	"github.com/sjzsdu/free-router/internal/provider"
	"github.com/sjzsdu/free-router/internal/routing"
	"github.com/sjzsdu/free-router/internal/transport"
)

type Config struct {
	MaxAttempts int
	Routes      *routing.Store
	Health      *health.Tracker
	APIToken    string
}

type Gateway struct {
	catalog    *catalog.Store
	registry   *provider.Registry
	adapterReg *adapter.Registry
	config     Config
	client     *http.Client
	next       atomic.Uint64
	routeNext  sync.Map
	tracker    *health.Tracker
	mux        *http.ServeMux
	apiToken   string
	limiters   sync.Map
	metrics    *Metrics
	// beforeCandidateAcquire is a deterministic test seam for exercising the
	// race between candidate snapshots and half-open probe acquisition.
	beforeCandidateAcquire func()
}

func New(store *catalog.Store, registry *provider.Registry, config Config, client *http.Client) *Gateway {
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = 3
	}
	if config.Health == nil {
		config.Health = health.New()
	}
	for _, verification := range store.CapabilityVerifications() {
		if !config.Health.HasState(verification.Model, verification.Capability) {
			config.Health.RestoreProbeSuccess(verification.Model, verification.Capability, verification.CheckedAt, verification.LatencyMS)
		}
	}
	gateway := &Gateway{catalog: store, registry: registry, adapterReg: adapter.NewRegistry(), config: config, client: client, tracker: config.Health, mux: http.NewServeMux(), apiToken: config.APIToken, metrics: NewMetrics()}
	gateway.mux.HandleFunc("GET /healthz", gateway.health)
	gateway.mux.HandleFunc("GET /livez", gateway.livez)
	gateway.mux.HandleFunc("GET /readyz", gateway.readyz)
	gateway.mux.HandleFunc("GET /metrics", gateway.metrics.Handler())
	gateway.mux.HandleFunc("GET /v1/models", gateway.models)
	gateway.mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		gateway.proxyJSON(w, r, "/chat/completions", "chat")
	})
	gateway.mux.HandleFunc("POST /v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		gateway.proxyJSON(w, r, "/embeddings", "embedding")
	})
	gateway.mux.HandleFunc("POST /v1/audio/speech", func(w http.ResponseWriter, r *http.Request) {
		gateway.proxyJSON(w, r, "/audio/speech", catalog.FunctionTextToSpeech)
	})
	gateway.mux.HandleFunc("POST /v1/audio/transcriptions", func(w http.ResponseWriter, r *http.Request) {
		gateway.proxyMultipart(w, r, "/audio/transcriptions", catalog.FunctionSpeechToText)
	})
	gateway.mux.HandleFunc("POST /v1/audio/translations", func(w http.ResponseWriter, r *http.Request) {
		gateway.proxyMultipart(w, r, "/audio/translations", catalog.FunctionSpeechToText)
	})
	gateway.mux.HandleFunc("POST /v1/images/generations", func(w http.ResponseWriter, r *http.Request) {
		gateway.proxyJSON(w, r, "/images/generations", catalog.FunctionImageGeneration)
	})
	gateway.mux.HandleFunc("POST /v1/images/edits", func(w http.ResponseWriter, r *http.Request) {
		gateway.proxyMultipart(w, r, "/images/edits", catalog.FunctionImageGeneration)
	})
	gateway.mux.HandleFunc("POST /v1/images/variations", func(w http.ResponseWriter, r *http.Request) {
		gateway.proxyMultipart(w, r, "/images/variations", catalog.FunctionImageGeneration)
	})
	gateway.mux.HandleFunc("POST /v1/videos/generations", func(w http.ResponseWriter, r *http.Request) {
		gateway.proxyJSON(w, r, "/videos/generations", catalog.FunctionVideoGeneration)
	})
	gateway.mux.HandleFunc("POST /v1/rerank", func(w http.ResponseWriter, r *http.Request) {
		gateway.proxyJSON(w, r, "/rerank", "rerank")
	})
	gateway.mux.HandleFunc("POST /v1/moderations", func(w http.ResponseWriter, r *http.Request) {
		gateway.proxyJSON(w, r, "/moderations", "moderation")
	})
	return gateway
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("panic serving request", "error", recovered, "stack", string(debug.Stack()))
			g.metrics.RecordFailure(0, http.StatusInternalServerError)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}
	}()
	if g.apiToken != "" && !g.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="free-router api"`)
		http.Error(w, "api authentication required", http.StatusUnauthorized)
		return
	}
	g.mux.ServeHTTP(w, r)
}

func (g *Gateway) authorized(r *http.Request) bool {
	if g.apiToken == "" {
		return true
	}
	if token := r.Header.Get("Authorization"); strings.HasPrefix(token, "Bearer ") {
		return strings.TrimPrefix(token, "Bearer ") == g.apiToken
	}
	return false
}

func (g *Gateway) Handle(pattern string, handler http.Handler) { g.mux.Handle(pattern, handler) }

func (g *Gateway) health(w http.ResponseWriter, _ *http.Request) {
	status := g.catalog.Status()
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "free_models": len(g.catalog.Models()), "providers": len(g.registry.All()),
		"catalog_count": status.Count, "requests": g.tracker.Summary(),
	})
}

func (g *Gateway) livez(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (g *Gateway) readyz(w http.ResponseWriter, _ *http.Request) {
	providers := len(g.registry.All())
	if providers == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"unready","reason":"no providers configured"}`))
		return
	}

	hasHealthyModel := false
	for _, model := range g.catalog.Models() {
		if g.tracker.Available(model.ID, "chat") {
			hasHealthyModel = true
			break
		}
	}

	if !hasHealthyModel {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"unready","reason":"no healthy models available"}`))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ready","providers":` + strconv.Itoa(providers) + `}`))
}

func (g *Gateway) models(w http.ResponseWriter, _ *http.Request) {
	data := make([]map[string]any, 0, len(catalog.AllFunctions()))
	if g.config.Routes != nil {
		config := g.config.Routes.Config()
		aliases := g.config.Routes.Aliases()
		for _, alias := range aliases {
			route := config.Routes[alias]
			if !g.routeAvailable(route) {
				continue
			}
			fallbackModels := make([]string, 0, len(route.Models))
			for _, modelID := range route.Models {
				if g.tracker.Available(modelID, route.Capability) {
					fallbackModels = append(fallbackModels, modelID)
				}
			}
			data = append(data, map[string]any{
				"id": alias, "object": "model", "owned_by": "free-router", "type": route.Capability,
				"free": true, "route": true, "strategy": route.Strategy, "fallback_models": fallbackModels,
				"capabilities": catalog.Capabilities{ToolCall: route.RequireTool, ToolCallKnown: route.RequireTool, Streaming: true},
			})
		}
	} else {
		for _, alias := range catalog.AllFunctions() {
			route := routing.DefaultConfig().Routes[alias]
			if !g.routeAvailable(route) {
				continue
			}
			data = append(data, map[string]any{"id": alias, "object": "model", "owned_by": "free-router", "type": alias, "free": true, "route": true})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (g *Gateway) routeAvailable(route routing.Route) bool {
	for _, source := range g.catalog.Models() {
		model := source
		if _, configured := g.registry.Get(model.Provider); !configured {
			continue
		}
		if g.config.Routes != nil {
			var enabled bool
			model, enabled = g.config.Routes.Apply(model)
			if !enabled {
				continue
			}
		}
		if routing.Accepts(route, model) && g.catalog.ModelCapabilityVerified(model, route.Capability) && g.tracker.Healthy(model.ID, route.Capability) {
			return true
		}
	}
	return false
}

func (g *Gateway) proxyJSON(w http.ResponseWriter, r *http.Request, endpoint, defaultAlias string) {
	g.metrics.RecordRequest()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "request body is too large or unreadable")
		g.metrics.RecordFailure(0, http.StatusBadRequest)
		return
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		g.metrics.RecordFailure(0, http.StatusBadRequest)
		return
	}
	if request == nil {
		request = map[string]any{}
	}
	requested, _ := request["model"].(string)
	if requested == "" {
		requested = defaultAlias
	}
	_, needsTools := request["tools"]
	if (requested == "auto" || requested == "free" || requested == catalog.FunctionChat) && defaultAlias == catalog.FunctionChat && needsTools {
		requested = "chat-tools"
	} else if requested == "auto" || requested == "free" {
		requested = defaultAlias
	}
	capability := g.requestCapability(requested, defaultAlias)
	if !endpointSupports(defaultAlias, capability) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("model capability %q is not compatible with this OpenAI endpoint", capability))
		g.metrics.RecordFailure(0, http.StatusBadRequest)
		return
	}
	candidates, fallback := g.candidates(requested, needsTools)
	if len(candidates) == 0 {
		if fallback {
			writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("route %q has no healthy models; inspect failed models in the admin UI", requested))
		} else {
			writeError(w, http.StatusNotFound, fmt.Sprintf("free model %q was not found", requested))
		}
		g.metrics.RecordFailure(0, http.StatusServiceUnavailable)
		return
	}
	g.runBeforeCandidateAcquire()

	for index, model := range candidates {
		if !g.tracker.TryAcquire(model.ID, capability) {
			continue
		}
		// Release the in-flight slot on every exit path (including panics)
		// so a half-open model can never wedge permanently.
		defer g.tracker.Release(model.ID, capability)
		request["model"] = model.UpstreamID
		payload, err := json.Marshal(request)
		if err != nil {
			writeError(w, http.StatusBadRequest, "could not encode request")
			g.metrics.RecordFailure(0, http.StatusBadRequest)
			return
		}
		if g.tryCandidate(w, r, model, capability, endpoint, payload, "application/json", index, candidates) {
			return
		}
	}
	g.writeCandidateUnavailable(w, requested)
}

func (g *Gateway) proxyMultipart(w http.ResponseWriter, r *http.Request, endpoint, defaultAlias string) {
	g.metrics.RecordRequest()
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		writeError(w, http.StatusBadRequest, "multipart/form-data with a boundary is required")
		g.metrics.RecordFailure(0, http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "request body is too large or unreadable")
		g.metrics.RecordFailure(0, http.StatusBadRequest)
		return
	}
	requested, err := multipartModel(body, params["boundary"])
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid multipart body")
		g.metrics.RecordFailure(0, http.StatusBadRequest)
		return
	}
	if requested == "" || requested == "auto" || requested == "free" {
		requested = defaultAlias
	}
	capability := g.requestCapability(requested, defaultAlias)
	if !endpointSupports(defaultAlias, capability) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("model capability %q is not compatible with this OpenAI endpoint", capability))
		g.metrics.RecordFailure(0, http.StatusBadRequest)
		return
	}
	candidates, fallback := g.candidates(requested, false)
	if len(candidates) == 0 {
		if fallback {
			writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("route %q has no healthy models; inspect failed models in the admin UI", requested))
		} else {
			writeError(w, http.StatusNotFound, fmt.Sprintf("free model %q was not found", requested))
		}
		g.metrics.RecordFailure(0, http.StatusServiceUnavailable)
		return
	}
	g.runBeforeCandidateAcquire()
	for index, model := range candidates {
		if !g.tracker.TryAcquire(model.ID, capability) {
			continue
		}
		// Release the in-flight slot on every exit path (including panics)
		// so a half-open model can never wedge permanently.
		defer g.tracker.Release(model.ID, capability)
		payload, contentType, err := rewriteMultipartModel(body, params["boundary"], model.UpstreamID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "could not encode multipart request")
			g.metrics.RecordFailure(0, http.StatusBadRequest)
			return
		}
		if g.tryCandidate(w, r, model, capability, endpoint, payload, contentType, index, candidates) {
			return
		}
	}
	g.writeCandidateUnavailable(w, requested)
}

// tryCandidate forwards a payload to a single candidate model and writes the
// upstream response back to the client. It returns true when the caller must
// stop (a terminal response was written); false means "try the next
// candidate". Health is recorded only after the body transfer completes, so a
// stream interrupted midway counts as a failure rather than a success.
func (g *Gateway) tryCandidate(w http.ResponseWriter, r *http.Request, model catalog.Model, capability, endpoint string, payload []byte, contentType string, index int, candidates []catalog.Model) bool {
	started := time.Now()
	resp, err := g.forward(r, model, payload, endpoint, contentType)
	if err != nil {
		g.tracker.Failure(model.ID, capability, time.Since(started), 0, err.Error(), 0)
		slog.Warn("provider request failed", "provider", model.Provider, "model", model.UpstreamID, "error", err)
		decision := g.shouldRetry(model, capability, 0, false, index)
		if decision.ShouldRetry && index+1 < len(candidates) {
			g.metrics.RecordFallback()
			// No backoff sleep on connection errors: fallback moves to a
			// different provider, and waiting would only delay recovery.
			return false
		}
		writeError(w, http.StatusBadGateway, "all configured free providers failed")
		g.metrics.RecordFailure(time.Since(started), 0)
		return true
	}
	providerAdapter := g.adapterReg.Get(model.Provider)
	decision := g.shouldRetry(model, capability, resp.StatusCode, resp.ContentLength > 0, index)
	if decision.ShouldRetry && index+1 < len(candidates) {
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		g.tracker.Failure(model.ID, capability, time.Since(started), resp.StatusCode, resp.Status, retryAfter)
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		slog.Info("free provider unavailable; trying next", "provider", model.Provider, "model", model.UpstreamID, "status", resp.StatusCode, "reason", decision.Reason)
		g.metrics.RecordFallback()
		if resp.StatusCode == http.StatusTooManyRequests {
			g.metrics.RecordRateLimit()
		}
		sleepBackoff(r.Context(), effectiveDelay(decision.Delay, retryAfter))
		return false
	}
	normalizedResp, err := providerAdapter.NormalizeResponse(resp)
	if err != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		g.tracker.Failure(model.ID, capability, time.Since(started), resp.StatusCode, err.Error(), 0)
		writeError(w, http.StatusBadGateway, err.Error())
		g.metrics.RecordFailure(time.Since(started), resp.StatusCode)
		return true
	}
	if normalizedResp.Error != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		g.tracker.Failure(model.ID, capability, time.Since(started), normalizedResp.Error.StatusCode, normalizedResp.Error.Message, 0)
		writeError(w, normalizedResp.Error.StatusCode, normalizedResp.Error.Message)
		g.metrics.RecordFailure(time.Since(started), normalizedResp.Error.StatusCode)
		return true
	}
	result := copyResponse(w, resp, model)
	if result.Complete && resp.StatusCode < http.StatusBadRequest {
		g.tracker.Success(model.ID, capability, time.Since(started), resp.StatusCode)
		g.metrics.RecordSuccess(time.Since(started))
	} else {
		message := resp.Status
		if result.Error != nil {
			message = result.Error.Error()
		}
		g.tracker.Failure(model.ID, capability, time.Since(started), resp.StatusCode, message, parseRetryAfter(resp.Header.Get("Retry-After")))
		g.metrics.RecordFailure(time.Since(started), resp.StatusCode)
	}
	return true
}

// maxFallbackDelay bounds how long a fallback waits before trying the next
// candidate. An upstream Retry-After beyond this is effectively a long outage;
// waiting for it would pin the handler goroutine for hours.
const maxFallbackDelay = 5 * time.Minute

// effectiveDelay combines the retry backoff with any Retry-After hint from
// the upstream, honouring the longer of the two, capped at maxFallbackDelay.
func effectiveDelay(backoff, retryAfter time.Duration) time.Duration {
	if retryAfter > backoff {
		backoff = retryAfter
	}
	if backoff > maxFallbackDelay {
		return maxFallbackDelay
	}
	return backoff
}

// sleepBackoff waits for the retry delay, aborting early when the client
// disconnects so a stale handler does not keep sleeping.
func sleepBackoff(ctx context.Context, delay time.Duration) {
	if delay <= 0 {
		return
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func (g *Gateway) runBeforeCandidateAcquire() {
	if g.beforeCandidateAcquire != nil {
		g.beforeCandidateAcquire()
	}
}

func (g *Gateway) writeCandidateUnavailable(w http.ResponseWriter, requested string) {
	writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("route %q has no currently acquirable models; retry shortly", requested))
	g.metrics.RecordFailure(0, http.StatusServiceUnavailable)
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
			return g.dynamicCandidates(route, needsTools), true
		}
	}
	if g.config.Routes != nil {
		if route, ok := g.config.Routes.Route(requested); ok {
			effectiveRoute := route
			effectiveRoute.RequireTool = effectiveRoute.RequireTool || needsTools
			if len(route.Models) > 0 {
				priority := make([]catalog.Model, 0, len(route.Models))
				configured := make(map[string]bool, len(route.Models))
				for _, id := range route.Models {
					configured[id] = true
					model, ok := g.catalog.Find(id)
					if ok {
						model, ok = g.config.Routes.Apply(model)
					}
					if !ok || !routing.Accepts(effectiveRoute, model) {
						continue
					}
					priority = append(priority, model)
				}
				result := g.strictlyAvailable(priority, effectiveRoute.Capability)
				if route.Strategy == routing.StrategyRoundRobin {
					result = rotateCandidates(result, g.nextForRoute(requested))
				}
				if remaining, ok := g.pickRemaining(effectiveRoute, configured, true); ok {
					result = append(result, remaining)
				}
				return result, true
			}
			return g.dynamicCandidates(effectiveRoute, false), true
		}
	}
	if model, ok := g.catalog.Find(requested); ok {
		if g.config.Routes != nil {
			model, ok = g.config.Routes.Apply(model)
			if !ok {
				return nil, false
			}
		}
		return []catalog.Model{model}, false
	}
	return nil, false
}

func (g *Gateway) nextForRoute(alias string) uint64 {
	counter, _ := g.routeNext.LoadOrStore(alias, &atomic.Uint64{})
	return counter.(*atomic.Uint64).Add(1) - 1
}

func (g *Gateway) requestCapability(requested, fallback string) string {
	if g.config.Routes != nil {
		if route, ok := g.config.Routes.Route(requested); ok {
			return route.Capability
		}
	} else if route, ok := routing.DefaultConfig().Routes[requested]; ok {
		return route.Capability
	}
	return fallback
}

func endpointSupports(endpointCapability, requestedCapability string) bool {
	if endpointCapability == catalog.FunctionChat {
		switch requestedCapability {
		case catalog.FunctionChat, catalog.FunctionChatTools, catalog.FunctionImageUnderstanding, catalog.FunctionVideoUnderstanding, catalog.FunctionAudioUnderstanding:
			return true
		}
	}
	return endpointCapability == requestedCapability
}

func rotateCandidates(models []catalog.Model, offset uint64) []catalog.Model {
	if len(models) < 2 {
		return models
	}
	start := int(offset % uint64(len(models)))
	result := make([]catalog.Model, 0, len(models))
	result = append(result, models[start:]...)
	result = append(result, models[:start]...)
	return result
}

func (g *Gateway) dynamicCandidates(route routing.Route, needsTools bool) []catalog.Model {
	route.RequireTool = route.RequireTool || needsTools
	groups := make(map[string][]catalog.Model)
	var preferred *catalog.Model
	for _, model := range g.catalog.Models() {
		if _, configured := g.registry.Get(model.Provider); !configured {
			continue
		}
		if g.config.Routes != nil {
			var enabled bool
			model, enabled = g.config.Routes.Apply(model)
			if !enabled {
				continue
			}
		}
		if !routing.Accepts(route, model) {
			continue
		}
		if !g.catalog.ModelCapabilityVerified(model, route.Capability) {
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
	result := make([]catalog.Model, 0)
	if preferred != nil {
		result = append(result, *preferred)
	}
	if len(providerIDs) == 0 || len(result) == g.config.MaxAttempts {
		return g.limitCandidates(g.availableCandidates(result, route.Capability))
	}
	seed := int(g.next.Add(1) - 1)
	for round := 0; ; round++ {
		added := false
		for offset := range providerIDs {
			providerID := providerIDs[(seed+offset)%len(providerIDs)]
			models := groups[providerID]
			if round >= len(models) {
				continue
			}
			result = append(result, models[(seed+round)%len(models)])
			added = true
		}
		if !added {
			break
		}
	}
	return g.limitCandidates(g.availableCandidates(result, route.Capability))
}

func (g *Gateway) strictlyAvailable(models []catalog.Model, capability string) []catalog.Model {
	result := make([]catalog.Model, 0, len(models))
	for _, model := range models {
		if g.catalog.ModelCapabilityVerified(model, capability) && g.tracker.Healthy(model.ID, capability) {
			result = append(result, model)
		}
	}
	return result
}

func (g *Gateway) pickRemaining(route routing.Route, excluded map[string]bool, healthyOnly bool) (catalog.Model, bool) {
	models := make([]catalog.Model, 0)
	for _, model := range g.catalog.Models() {
		if _, configured := g.registry.Get(model.Provider); !configured {
			continue
		}
		if excluded[model.ID] {
			continue
		}
		if g.config.Routes != nil {
			var enabled bool
			model, enabled = g.config.Routes.Apply(model)
			if !enabled {
				continue
			}
		}
		if !routing.Accepts(route, model) ||
			(healthyOnly && (!g.catalog.ModelCapabilityVerified(model, route.Capability) || !g.tracker.Healthy(model.ID, route.Capability))) {
			continue
		}
		models = append(models, model)
	}
	if len(models) == 0 {
		return catalog.Model{}, false
	}
	index := int(g.next.Add(1)-1) % len(models)
	return models[index], true
}

func (g *Gateway) availableCandidates(models []catalog.Model, capability string) []catalog.Model {
	available := make([]catalog.Model, 0, len(models))
	for _, model := range models {
		if g.catalog.ModelCapabilityVerified(model, capability) && g.tracker.Healthy(model.ID, capability) {
			available = append(available, model)
		}
	}
	return available
}

func (g *Gateway) limitCandidates(models []catalog.Model) []catalog.Model {
	if len(models) > g.config.MaxAttempts {
		return models[:g.config.MaxAttempts]
	}
	return models
}

func parseRetryAfter(value string) time.Duration {
	const maxRetryAfter = 24 * time.Hour
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		delay := time.Duration(seconds) * time.Second
		// A huge Retry-After can overflow int64 into a negative duration,
		// which would defeat rate-limit cooling entirely; clamp it.
		if delay < 0 || delay > maxRetryAfter {
			return maxRetryAfter
		}
		return delay
	}
	if deadline, err := http.ParseTime(value); err == nil {
		if duration := time.Until(deadline); duration > 0 {
			if duration > maxRetryAfter {
				return maxRetryAfter
			}
			return duration
		}
	}
	return 0
}

func (g *Gateway) forward(original *http.Request, model catalog.Model, body []byte, endpoint, contentType string) (*http.Response, error) {
	spec, ok := g.registry.Get(model.Provider)
	if !ok {
		return nil, fmt.Errorf("provider %s is not configured", model.Provider)
	}

	limiterAny, ok := g.limiters.Load(model.Provider)
	if !ok {
		// LoadOrStore only evaluates newLimiter when the provider is
		// missing, so we do not leak a discarded Limiter (and its
		// token-refill goroutine) on every request.
		limiterAny, _ = g.limiters.LoadOrStore(model.Provider, g.newLimiter(spec))
	}
	limiter := limiterAny.(*transport.Limiter)

	if err := limiter.Acquire(original.Context()); err != nil {
		return nil, err
	}
	defer limiter.Release()

	providerAdapter := g.adapterReg.Get(model.Provider)
	reqHeaders := make(map[string]string)
	if accept := original.Header.Get("Accept"); accept != "" {
		reqHeaders["Accept"] = accept
	}

	req, err := providerAdapter.BuildRequest(adapter.Request{
		Context:     original.Context(),
		Method:      http.MethodPost,
		Endpoint:    endpoint,
		Model:       model,
		Provider:    spec,
		Body:        body,
		ContentType: contentType,
		Headers:     reqHeaders,
		Stream:      original.Header.Get("Accept") == "text/event-stream" || strings.Contains(original.Header.Get("Accept"), "event-stream"),
		Function:    endpoint,
	})
	if err != nil {
		return nil, err
	}
	return g.client.Do(req)
}

func (g *Gateway) newLimiter(spec provider.Spec) *transport.Limiter {
	config := transport.NewRateLimitConfig()
	if spec.MaxConcurrent > 0 {
		config.MaxConcurrentRequests = spec.MaxConcurrent
	}
	if spec.RateLimitPerSecond > 0 {
		config.RateLimitPerSecond = spec.RateLimitPerSecond
	}
	if spec.QueueSize > 0 {
		config.QueueSize = spec.QueueSize
	}
	return transport.NewLimiter(config)
}

type StreamResult struct {
	Complete     bool
	BytesWritten int64
	Error        error
}

// stallTimeout bounds how long a body read may sit without producing data.
// The gateway client only sets ResponseHeaderTimeout, so an upstream that
// sends headers and then stalls would otherwise pin the connection, the
// handler goroutine and the metrics concurrency slot forever.
const stallTimeout = 5 * time.Minute

// stallTimeoutReader closes the underlying body when no data has been read
// for the configured interval. Closing from another goroutine unblocks a
// pending Read, so copyStream terminates instead of hanging.
type stallTimeoutReader struct {
	r     io.ReadCloser
	delay time.Duration
	timer *time.Timer
	mu    sync.Mutex
	done  bool
}

func newStallTimeoutReader(r io.ReadCloser, delay time.Duration) *stallTimeoutReader {
	s := &stallTimeoutReader{r: r, delay: delay}
	s.timer = time.AfterFunc(delay, s.timeout)
	return s
}

func (s *stallTimeoutReader) timeout() {
	s.mu.Lock()
	s.done = true
	s.mu.Unlock()
	_ = s.r.Close()
}

func (s *stallTimeoutReader) Read(p []byte) (int, error) {
	s.mu.Lock()
	if !s.done {
		if !s.timer.Stop() {
			// The timer already fired (or is about to): the timeout
			// goroutine is closing the body, so this Read will fail
			// shortly. Reset would schedule a second callback; do not.
			s.mu.Unlock()
			return s.r.Read(p)
		}
		s.timer.Reset(s.delay)
	}
	s.mu.Unlock()
	return s.r.Read(p)
}

func (s *stallTimeoutReader) Close() error {
	s.mu.Lock()
	s.done = true
	s.mu.Unlock()
	s.timer.Stop()
	return s.r.Close()
}

func copyResponse(w http.ResponseWriter, resp *http.Response, model catalog.Model) StreamResult {
	body := newStallTimeoutReader(resp.Body, stallTimeout)
	defer body.Close()
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
		return copyStream(w, body)
	}
	n, err := io.Copy(w, body)
	return StreamResult{
		Complete:     err == nil,
		BytesWritten: n,
		Error:        err,
	}
}

func copyStream(w http.ResponseWriter, body io.Reader) StreamResult {
	buffer := make([]byte, 32*1024)
	flusher, canFlush := w.(http.Flusher)
	var totalWritten int64
	for {
		read, err := body.Read(buffer)
		if read > 0 {
			n, writeErr := w.Write(buffer[:read])
			totalWritten += int64(n)
			if writeErr != nil {
				return StreamResult{
					Complete:     false,
					BytesWritten: totalWritten,
					Error:        writeErr,
				}
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			return StreamResult{
				Complete:     err == io.EOF,
				BytesWritten: totalWritten,
				Error:        err,
			}
		}
	}
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
