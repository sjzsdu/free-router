package gateway

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sjzsdu/free-router/internal/adapter"
	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/health"
	"github.com/sjzsdu/free-router/internal/provider"
	"github.com/sjzsdu/free-router/internal/routing"
	"github.com/sjzsdu/free-router/internal/statistics"
	"github.com/sjzsdu/free-router/internal/transport"
)

type Config struct {
	MaxAttempts int
	Routes      *routing.Store
	Health      *health.Tracker
	APIToken    string
	Adapters    adapter.Resolver
	Statistics  *statistics.Store
}

type Gateway struct {
	catalog  *catalog.Store
	registry *provider.Registry
	adapters adapter.Resolver
	config   Config
	client   *http.Client
	planner  *CandidatePlanner
	executor *AttemptExecutor
	tracker  *health.Tracker
	mux      *http.ServeMux
	apiToken string
	limiters sync.Map
	metrics  *Metrics
	stats    *statistics.Store
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
	if config.Adapters == nil {
		config.Adapters = adapter.NewRegistry()
	}
	if config.Statistics == nil {
		config.Statistics = statistics.NewMemory()
	}
	gateway := &Gateway{catalog: store, registry: registry, adapters: config.Adapters, config: config, client: client, tracker: config.Health, mux: http.NewServeMux(), apiToken: config.APIToken, metrics: NewMetrics(), stats: config.Statistics}
	gateway.planner = NewCandidatePlanner(store, registry, config.Routes, config.Health, config.MaxAttempts)
	gateway.executor = NewAttemptExecutor(gateway)
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
	if g.requiresAPIAuthentication(r) && !g.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="free-router api"`)
		http.Error(w, "api authentication required", http.StatusUnauthorized)
		return
	}
	g.mux.ServeHTTP(w, r)
}

func (g *Gateway) requiresAPIAuthentication(r *http.Request) bool {
	return g.apiToken != "" && strings.HasPrefix(r.URL.Path, "/v1/")
}

func (g *Gateway) authorized(r *http.Request) bool {
	if g.apiToken == "" {
		return true
	}
	if scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " "); ok && scheme == "Bearer" {
		return subtle.ConstantTimeCompare([]byte(token), []byte(g.apiToken)) == 1
	}
	return false
}

func (g *Gateway) Handle(pattern string, handler http.Handler) { g.mux.Handle(pattern, handler) }

func (g *Gateway) health(w http.ResponseWriter, _ *http.Request) {
	status := g.catalog.Status()
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "free_models": len(g.catalog.ConfiguredModels()), "providers": len(g.registry.All()),
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

	capabilities := g.readyCapabilities()
	hasHealthyModel := false
	for _, model := range g.catalog.ConfiguredModels() {
		for _, capability := range capabilities {
			if model.SupportsFunction(capability) && g.tracker.Available(model.ID, capability) {
				hasHealthyModel = true
				break
			}
		}
		if hasHealthyModel {
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

// readyCapabilities returns the capability set the gateway must be ready for:
// the capabilities of configured routes, falling back to text chat when no
// routes are configured. A pure embedding/image/audio deployment must not be
// reported unready just because no chat model is healthy.
func (g *Gateway) readyCapabilities() []string {
	seen := make(map[string]bool)
	var capabilities []string
	if g.config.Routes != nil {
		for _, route := range g.config.Routes.Config().Routes {
			if route.Capability != "" && !seen[route.Capability] {
				seen[route.Capability] = true
				capabilities = append(capabilities, route.Capability)
			}
		}
	}
	if len(capabilities) == 0 {
		capabilities = append(capabilities, catalog.FunctionChat)
	}
	return capabilities
}

func (g *Gateway) models(w http.ResponseWriter, _ *http.Request) {
	data := make([]map[string]any, 0, len(catalog.AllFunctions()))
	view := g.planner.snapshot()
	if g.config.Routes != nil {
		config := g.config.Routes.Config()
		aliases := g.config.Routes.Aliases()
		for _, alias := range aliases {
			route := config.Routes[alias]
			if !g.planner.routeAvailable(view, route) {
				continue
			}
			fallbackModels := make([]string, 0, len(route.Models))
			for _, modelID := range route.Models {
				model, ok := view.Find(modelID)
				if ok {
					_, reason := view.Eligible(model, route, true, true)
					if reason == "" {
						fallbackModels = append(fallbackModels, modelID)
					}
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
			if !g.planner.routeAvailable(view, route) {
				continue
			}
			data = append(data, map[string]any{"id": alias, "object": "model", "owned_by": "free-router", "type": alias, "free": true, "route": true})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (g *Gateway) routeAvailable(route routing.Route) bool {
	return g.planner.RouteAvailable(route)
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
	if defaultAlias == catalog.FunctionChat && needsTools {
		capability = catalog.FunctionChatTools
	}
	if !endpointSupports(defaultAlias, capability) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("model capability %q is not compatible with this OpenAI endpoint", capability))
		g.metrics.RecordFailure(0, http.StatusBadRequest)
		return
	}
	// A stable conversation seed keeps all requests in the same multi-turn
	// dialog on the same primary model. Tool calls/results must be interpreted
	// by the same upstream model across the dialog or schema mismatches and
	// 4xx errors multiply and collapse the chat-tools pool.
	sessionSeed := sessionSeedFromRequest(request)
	candidates, fallback := g.candidates(requested, capability, needsTools, sessionSeed)
	g.runBeforeCandidateAcquire()

	// lastError accumulates the most recent deferred upstream error so the
	// gateway can surface it after every fallback path has been exhausted
	// (instead of a generic 503). It is only populated when deferErrorWrite
	// asked Execute to skip writing the response itself.
	var lastError *adapter.Error

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
		// Defer the final 4xx/5xx write only on the tool-aliased route: a
		// plain chat request has no higher-level fallback and should keep
		// the original behaviour of writing the upstream error immediately.
		result := g.tryCandidate(w, r, model, capability, endpoint, payload, "application/json", index, candidates, needsTools && fallback)
		if result.Terminal {
			return
		}
		if result.LastError != nil {
			lastError = result.LastError
		}
	}

	// Loose fallback: when a route-aliased tool request has exhausted the
	// chat-tools pool, retry against plain chat candidates with the tools
	// field stripped. Returning a normal chat completion is preferable to
	// leaking the upstream 4xx ("usage_limit_reached" etc.) to the caller,
	// which downstream agents misread as their own quota event.
	if needsTools && fallback {
		looseCandidates, _ := g.candidates(catalog.FunctionChat, catalog.FunctionChat, false, sessionSeed)
		if len(looseCandidates) > 0 {
			looseRequest := stripTools(request)
			looseCapability := catalog.FunctionChat
			for index, model := range looseCandidates {
				if !g.tracker.TryAcquire(model.ID, looseCapability) {
					continue
				}
				defer g.tracker.Release(model.ID, looseCapability)
				looseRequest["model"] = model.UpstreamID
				payload, err := json.Marshal(looseRequest)
				if err != nil {
					writeError(w, http.StatusBadRequest, "could not encode request")
					g.metrics.RecordFailure(0, http.StatusBadRequest)
					return
				}
				// Continue deferring so the loose loop can try every plain
				// chat candidate; the final error (if any) is written below.
				result := g.tryCandidate(w, r, model, looseCapability, endpoint, payload, "application/json", index, looseCandidates, true)
				if result.Terminal {
					return
				}
				if result.LastError != nil {
					lastError = result.LastError
				}
			}
		}
	}

	// Surface the last deferred upstream error verbatim now that every
	// fallback path has been tried. Without this the caller would see a
	// generic "no acquirable models" 503 even though an upstream did speak
	// (e.g. "provider account unavailable"), which hides the real reason.
	if lastError != nil {
		writeError(w, lastError.StatusCode, lastError.Message)
		return
	}

	if len(candidates) == 0 {
		if fallback {
			writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("route %q has no healthy models; inspect failed models in the admin UI", requested))
		} else {
			writeError(w, http.StatusNotFound, fmt.Sprintf("free model %q was not found", requested))
		}
		g.metrics.RecordFailure(0, http.StatusServiceUnavailable)
		return
	}
	g.writeCandidateUnavailable(w, requested)
}

// sessionSeedFromRequest derives a stable 64-bit seed from the conversation's
// first few system/user messages. All requests in the same multi-turn dialog
// hash to the same seed, so the planner rotates them to the same starting
// model. We restrict to system/user content because assistant and tool turns
// vary across requests while the conversation identity must stay stable.
// Returns 0 when no system/user content is present, which falls back to the
// global round-robin counter.
func sessionSeedFromRequest(request map[string]any) uint64 {
	messages, _ := request["messages"].([]any)
	if len(messages) == 0 {
		return 0
	}
	hasher := fnv.New64a()
	var wrote bool
	for i, message := range messages {
		if i >= 6 {
			break
		}
		entry, ok := message.(map[string]any)
		if !ok {
			continue
		}
		role, _ := entry["role"].(string)
		if role != "system" && role != "user" {
			continue
		}
		content, _ := entry["content"].(string)
		if content == "" {
			continue
		}
		_, _ = io.WriteString(hasher, role)
		hasher.Write([]byte{0})
		_, _ = io.WriteString(hasher, content)
		hasher.Write([]byte{0})
		wrote = true
	}
	if !wrote {
		return 0
	}
	return hasher.Sum64()
}

// stripTools returns a shallow copy of request with the tools and tool_choice
// fields removed. It is used by the loose fallback path so plain chat models
// can answer when no chat-tools candidate remains.
func stripTools(request map[string]any) map[string]any {
	stripped := make(map[string]any, len(request))
	for key, value := range request {
		if key == "tools" || key == "tool_choice" {
			continue
		}
		stripped[key] = value
	}
	return stripped
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
	candidates, fallback := g.candidates(requested, capability, false, 0)
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
		// Multipart endpoints (images/audio/etc.) have no higher-level
		// fallback, so the executor writes the upstream error itself.
		if g.tryCandidate(w, r, model, capability, endpoint, payload, contentType, index, candidates, false).Terminal {
			return
		}
	}
	g.writeCandidateUnavailable(w, requested)
}

// tryCandidate preserves the internal compatibility seam while delegating all
// upstream execution policy to AttemptExecutor. deferErrorWrite is forwarded
// to Execute so callers running a higher-level fallback (e.g. chat-tools ->
// plain chat) can defer the final 4xx/5xx write and inspect LastError.
func (g *Gateway) tryCandidate(w http.ResponseWriter, r *http.Request, model catalog.Model, capability, endpoint string, payload []byte, contentType string, index int, candidates []catalog.Model, deferErrorWrite bool) AttemptResult {
	return g.executor.Execute(w, r, model, capability, endpoint, payload, contentType, index, candidates, deferErrorWrite)
}

func localLimitError(err error) (int, string, bool) {
	switch {
	case errors.Is(err, transport.ErrRateLimited):
		return http.StatusTooManyRequests, "provider rate limit queue timed out", true
	case errors.Is(err, transport.ErrOverloaded):
		return http.StatusServiceUnavailable, "provider request queue is full", true
	default:
		return 0, "", false
	}
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

func (g *Gateway) candidates(requested, capability string, needsTools bool, sessionSeed uint64) ([]catalog.Model, bool) {
	return g.planner.CandidatesSeeded(requested, capability, needsTools, sessionSeed)
}

func (g *Gateway) requestCapability(requested, fallback string) string {
	return g.planner.RequestCapability(requested, fallback)
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

	providerAdapter := g.adapters.Resolve(spec)
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
	Complete        bool
	BytesWritten    int64
	Error           error
	DownstreamError bool
	TTFB            time.Duration
	Usage           *statistics.Usage
	UsageExpected   bool
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

func copyResponse(w http.ResponseWriter, resp *http.Response, model catalog.Model, started time.Time) StreamResult {
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
	contentType := resp.Header.Get("Content-Type")
	eventStream := strings.Contains(contentType, "text/event-stream")
	result := copyBody(w, body, started, eventStream)
	result.UsageExpected = responseMayReportUsage(contentType)
	return result
}

func copyBody(w http.ResponseWriter, body io.Reader, started time.Time, flush bool) StreamResult {
	buffer := make([]byte, 32*1024)
	flusher, canFlush := w.(http.Flusher)
	var totalWritten int64
	var ttfb time.Duration
	collector := &usageCollector{}
	headerTTFB := time.Since(started)
	for {
		read, err := body.Read(buffer)
		if read > 0 {
			collector.Write(buffer[:read])
			if ttfb == 0 {
				ttfb = time.Since(started)
			}
			n, writeErr := w.Write(buffer[:read])
			totalWritten += int64(n)
			if writeErr != nil {
				return StreamResult{
					Complete:        false,
					BytesWritten:    totalWritten,
					Error:           writeErr,
					DownstreamError: true,
					TTFB:            ttfb,
					Usage:           collector.Usage(flush),
				}
			}
			if flush && canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			if ttfb == 0 {
				ttfb = headerTTFB
			}
			return StreamResult{
				Complete:     err == io.EOF,
				BytesWritten: totalWritten,
				Error:        err,
				TTFB:         ttfb,
				Usage:        collector.Usage(flush),
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
