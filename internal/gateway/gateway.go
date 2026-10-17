package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"mime"
	"net/http"
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
	// FailureLogCapacity bounds how many pool-wide failures are retained for
	// the admin UI. Zero uses the default.
	FailureLogCapacity int
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
	// failures keeps the most recent pool-wide failures for the admin UI.
	failures *failureLog
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
	gateway := &Gateway{catalog: store, registry: registry, adapters: config.Adapters, config: config, client: client, tracker: config.Health, mux: http.NewServeMux(), apiToken: config.APIToken, metrics: NewMetrics(), stats: config.Statistics, failures: newFailureLog(config.FailureLogCapacity)}
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

// RecentFailures returns the most recent pool-wide failures, newest first, so
// the admin UI can explain a failure the caller already saw.
func (g *Gateway) RecentFailures() []FailureRecord {
	return g.failures.List()
}

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
	// Pair the concurrency gauge with the request itself, not with each
	// upstream attempt: a fallback would otherwise leak a slot per retry.
	defer g.metrics.RecordDone()
	requestID := beginRequest(w)
	diagnostics := newRequestDiagnostics(requestID, "", g.failures)
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil {
		diagnostics.write(w, http.StatusBadRequest, "request body is too large or unreadable", "invalid_request")
		g.metrics.RecordFailure(0, http.StatusBadRequest)
		return
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		diagnostics.write(w, http.StatusBadRequest, "invalid JSON body", "invalid_request")
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
	diagnostics.Route = requested
	capability := g.requestCapability(requested, defaultAlias)
	if defaultAlias == catalog.FunctionChat && needsTools {
		capability = catalog.FunctionChatTools
	}
	if !endpointSupports(defaultAlias, capability) {
		diagnostics.write(w, http.StatusBadRequest, fmt.Sprintf("model capability %q is not compatible with this OpenAI endpoint", capability), "unsupported_capability")
		g.metrics.RecordFailure(0, http.StatusBadRequest)
		return
	}
	// A stable conversation seed keeps all requests in the same multi-turn
	// dialog on the same primary model. Tool calls/results must be interpreted
	// by the same upstream model across the dialog or schema mismatches and
	// 4xx errors multiply and collapse the chat-tools pool.
	sessionSeed := sessionSeedFromRequest(request)
	candidates, fallback := g.candidates(requested, capability, needsTools, sessionSeed)
	slog.Debug("proxyJSON candidates", "requested", requested, "capability", capability, "needsTools", needsTools, "fallback", fallback, "sessionSeed", sessionSeed, "candidates", func() []string {
		var cands []string
		for _, m := range candidates {
			cands = append(cands, fmt.Sprintf("%s/%s", m.Provider, m.UpstreamID))
		}
		return cands
	}())
	g.runBeforeCandidateAcquire()

	for index, model := range candidates {
		if !g.tracker.TryAcquire(model.ID, capability) {
			diagnostics.skip(model, "no acquirable in-flight slot; the model is cooling down after recent failures")
			continue
		}
		// Release the in-flight slot on every exit path (including panics)
		// so a half-open model can never wedge permanently.
		defer g.tracker.Release(model.ID, capability)
		request["model"] = model.UpstreamID
		payload, err := json.Marshal(request)
		if err != nil {
			diagnostics.write(w, http.StatusBadRequest, "could not encode request", "invalid_request")
			g.metrics.RecordFailure(0, http.StatusBadRequest)
			return
		}
		// Defer the error write on the tool-aliased route so the loose
		// fallback can run, and on the final candidate so the request-level
		// diagnostics — not the last provider to answer — decide what the
		// caller is told.
		result := g.tryCandidate(w, r, model, capability, endpoint, payload, "application/json", index, candidates, deferOnLast(needsTools && fallback, index, candidates))
		diagnostics.recordAttempt(model, capability, phaseStrict, result)
		if result.Terminal {
			return
		}
	}

	// Loose fallback: when a route-aliased tool request has exhausted the
	// chat-tools pool, retry against plain chat candidates with the tools
	// field stripped. Returning a normal chat completion is preferable to
	// leaking the upstream 4xx ("usage_limit_reached" etc.) to the caller,
	// which downstream agents misread as their own quota event.
	if needsTools && fallback {
		slog.Debug("entering loose fallback (chat-tools -> plain chat)", "originalCandidates", len(candidates))
		looseCandidates, _ := g.candidates(catalog.FunctionChat, catalog.FunctionChat, false, sessionSeed)
		slog.Debug("loose fallback candidates", "count", len(looseCandidates), "candidates", func() []string {
			var cands []string
			for _, m := range looseCandidates {
				cands = append(cands, fmt.Sprintf("%s/%s", m.Provider, m.UpstreamID))
			}
			return cands
		}())
		if len(looseCandidates) > 0 {
			looseRequest := stripTools(request)
			looseCapability := catalog.FunctionChat
			for index, model := range looseCandidates {
				if !g.tracker.TryAcquire(model.ID, looseCapability) {
					diagnostics.skip(model, "no acquirable in-flight slot for the plain-chat fallback")
					continue
				}
				defer g.tracker.Release(model.ID, looseCapability)
				looseRequest["model"] = model.UpstreamID
				payload, err := json.Marshal(looseRequest)
				if err != nil {
					diagnostics.write(w, http.StatusBadRequest, "could not encode request", "invalid_request")
					g.metrics.RecordFailure(0, http.StatusBadRequest)
					return
				}
				// Continue deferring so the loose loop can try every plain
				// chat candidate; the final error (if any) is written below.
				result := g.tryCandidate(w, r, model, looseCapability, endpoint, payload, "application/json", index, looseCandidates, true)
				diagnostics.recordAttempt(model, looseCapability, phaseLoose, result)
				if result.Terminal {
					return
				}
			}
		}
	}

	// Every candidate failed. Attribute the failure across the whole pool
	// instead of echoing whichever provider happened to speak last: an
	// exhausted free tier and a bad client request must not look alike.
	if len(diagnostics.Attempts) > 0 {
		diagnostics.writeFailure(w, summarizeAttempts(diagnostics.Attempts))
		return
	}

	if len(candidates) == 0 {
		if fallback {
			diagnostics.write(w, http.StatusServiceUnavailable, fmt.Sprintf("route %q has no healthy models; inspect failed models in the admin UI", requested), "no_healthy_models")
		} else {
			diagnostics.write(w, http.StatusNotFound, fmt.Sprintf("free model %q was not found", requested), "unknown_route")
		}
		g.metrics.RecordFailure(0, http.StatusServiceUnavailable)
		return
	}
	g.writeCandidateUnavailable(w, requested, diagnostics)
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
	// See proxyJSON: the gauge tracks downstream requests, not attempts.
	defer g.metrics.RecordDone()
	requestID := beginRequest(w)
	diagnostics := newRequestDiagnostics(requestID, "", g.failures)
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		diagnostics.write(w, http.StatusBadRequest, "multipart/form-data with a boundary is required", "invalid_request")
		g.metrics.RecordFailure(0, http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
	if err != nil {
		diagnostics.write(w, http.StatusBadRequest, "request body is too large or unreadable", "invalid_request")
		g.metrics.RecordFailure(0, http.StatusBadRequest)
		return
	}
	requested, err := multipartModel(body, params["boundary"])
	if err != nil {
		diagnostics.write(w, http.StatusBadRequest, "invalid multipart body", "invalid_request")
		g.metrics.RecordFailure(0, http.StatusBadRequest)
		return
	}
	if requested == "" || requested == "auto" || requested == "free" {
		requested = defaultAlias
	}
	diagnostics.Route = requested
	capability := g.requestCapability(requested, defaultAlias)
	if !endpointSupports(defaultAlias, capability) {
		diagnostics.write(w, http.StatusBadRequest, fmt.Sprintf("model capability %q is not compatible with this OpenAI endpoint", capability), "unsupported_capability")
		g.metrics.RecordFailure(0, http.StatusBadRequest)
		return
	}
	candidates, fallback := g.candidates(requested, capability, false, 0)
	if len(candidates) == 0 {
		if fallback {
			diagnostics.write(w, http.StatusServiceUnavailable, fmt.Sprintf("route %q has no healthy models; inspect failed models in the admin UI", requested), "no_healthy_models")
		} else {
			diagnostics.write(w, http.StatusNotFound, fmt.Sprintf("free model %q was not found", requested), "unknown_route")
		}
		g.metrics.RecordFailure(0, http.StatusServiceUnavailable)
		return
	}
	g.runBeforeCandidateAcquire()
	for index, model := range candidates {
		if !g.tracker.TryAcquire(model.ID, capability) {
			diagnostics.skip(model, "no acquirable in-flight slot; the model is cooling down after recent failures")
			continue
		}
		// Release the in-flight slot on every exit path (including panics)
		// so a half-open model can never wedge permanently.
		defer g.tracker.Release(model.ID, capability)
		payload, contentType, err := rewriteMultipartModel(body, params["boundary"], model.UpstreamID)
		if err != nil {
			diagnostics.write(w, http.StatusBadRequest, "could not encode multipart request", "invalid_request")
			g.metrics.RecordFailure(0, http.StatusBadRequest)
			return
		}
		// Multipart endpoints (images/audio/etc.) have no loose fallback, but
		// the final candidate still defers so the pool-wide failure can be
		// attributed across every provider that was tried.
		result := g.tryCandidate(w, r, model, capability, endpoint, payload, contentType, index, candidates, deferOnLast(false, index, candidates))
		diagnostics.recordAttempt(model, capability, phaseStrict, result)
		if result.Terminal {
			return
		}
	}
	// Every multipart candidate failed; report the pool-wide reason rather
	// than whichever upstream answered last.
	if len(diagnostics.Attempts) > 0 {
		diagnostics.writeFailure(w, summarizeAttempts(diagnostics.Attempts))
		return
	}
	g.writeCandidateUnavailable(w, requested, diagnostics)
}

// deferOnLast reports whether an attempt should leave the terminal error write
// to the caller. Doing this for the final candidate is what lets one request
// end with a pool-wide diagnosis; `always` keeps the chat-tools behaviour where
// a higher layer (the loose fallback) still has candidates to try.
func deferOnLast(always bool, index int, candidates []catalog.Model) bool {
	return always || index == len(candidates)-1
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

// sameProviderFallbackDelay bounds the pause before retrying a *sibling model
// of the provider that just failed. Two models of one provider share the same
// API key and therefore the same quota bucket, so a short pause is useful
// there.
const sameProviderFallbackDelay = 2 * time.Second

// fallbackDelay decides how long to wait before the next candidate.
//
// A fallback normally switches provider, i.e. to a different account and a
// different free quota. The failed provider's Retry-After describes *its*
// limit; honouring it on the way to another provider buys nothing upstream and
// adds the whole hint to the caller's latency — a 60s Retry-After turns a
// 200ms fallback into a minute of waiting, which defeats the point of routing
// across free sources. The failed model is already cooling down in the health
// tracker, so it will not be selected again while its hint lasts.
//
// Only when the next candidate is another model of the *same* provider does a
// pause still protect the shared quota, and even then it is capped so a long
// hint cannot pin the handler goroutine.
func fallbackDelay(candidates []catalog.Model, index int, backoff, retryAfter time.Duration) time.Duration {
	if index+1 >= len(candidates) || candidates[index+1].Provider != candidates[index].Provider {
		return 0
	}
	delay := retryAfter
	if backoff > delay {
		delay = backoff
	}
	if delay > sameProviderFallbackDelay {
		return sameProviderFallbackDelay
	}
	return delay
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

func (g *Gateway) writeCandidateUnavailable(w http.ResponseWriter, requested string, diagnostics *requestDiagnostics) {
	diagnostics.write(w, http.StatusServiceUnavailable, fmt.Sprintf("route %q has no currently acquirable models; retry shortly", requested), "no_acquirable_models")
	g.metrics.RecordFailure(0, http.StatusServiceUnavailable)
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
