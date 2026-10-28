package gateway

import (
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/sjzsdu/free-router/internal/adapter"
	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/statistics"
)

// AttemptResult describes whether an upstream attempt produced a terminal
// downstream response or whether fallback should continue.
type AttemptResult struct {
	Terminal bool
	// LastError is the adapter-classified error from the final attempt when
	// Terminal is false and the caller asked to defer writing it (so a higher
	// layer can run a loose fallback before giving up).
	LastError *adapter.Error
	// RetryAfter is the upstream Retry-After hint (seconds) that arrived with
	// LastError, so request-level diagnostics can forward it to the caller.
	RetryAfter int
	// Latency is how long this attempt took, so request-level diagnostics can
	// report *when* a provider failed, not only that it did: an instant 401
	// and a 30s timeout call for completely different remedies.
	Latency time.Duration
	// Served describes the attempt that produced the downstream response. It
	// is nil for every attempt that failed and for deferred ones, so the
	// access log can name the provider that actually answered.
	Served *ServedRecord
}

// AttemptExecutor owns one normalized provider attempt: transport execution,
// adapter response normalization, retry classification, stream copy, health,
// and metrics recording.
type AttemptExecutor struct {
	gateway *Gateway
}

func NewAttemptExecutor(gateway *Gateway) *AttemptExecutor {
	return &AttemptExecutor{gateway: gateway}
}

// Execute runs one upstream attempt. When deferErrorWrite is true the executor
// does not write a final 4xx/5xx response itself; instead it returns
// AttemptResult{Terminal:false, LastError:...} so the caller can run a
// higher-level fallback (e.g. chat-tools -> plain chat with tools stripped)
// before deciding what to send downstream.
func (e *AttemptExecutor) Execute(w http.ResponseWriter, r *http.Request, model catalog.Model, capability, endpoint string, payload []byte, contentType string, index int, candidates []catalog.Model, deferErrorWrite bool) (attempt AttemptResult) {

	started := time.Now()
	// Every exit path, including terminal ones, carries the attempt latency so
	// request-level diagnostics can show how long each provider took to fail.
	defer func() { attempt.Latency = time.Since(started) }()
	slog.Debug("attempt start", "provider", model.Provider, "model", model.UpstreamID, "index", index, "total_candidates", len(candidates), "capability", capability, "deferErrorWrite", deferErrorWrite)
	resp, err := e.gateway.forward(r, model, payload, endpoint, contentType)
	if err != nil {
		if status, message, local := localLimitError(err); local {
			// Local backpressure (queue full / rate-limit timeout) is
			// reported as-is instead of falling back to another provider:
			// this gateway's own limits must not amplify load across
			// providers. Upstream failures still fall back.
			slog.Debug("local limit error, not falling back", "provider", model.Provider, "model", model.UpstreamID, "status", status, "message", message)
			writeError(w, status, message)
			if status == http.StatusTooManyRequests {
				e.gateway.metrics.RecordRateLimit()
			}
			e.gateway.metrics.RecordFailure(time.Since(started), status)
			e.record(model, capability, false, status, time.Since(started), nil, false)
			return AttemptResult{Terminal: true}
		}
		e.gateway.tracker.Failure(model.ID, capability, time.Since(started), 0, err.Error(), 0)
		slog.Warn("provider request failed", "provider", model.Provider, "model", model.UpstreamID, "error", err)
		decision := e.gateway.shouldRetry(model, capability, 0, false, index)
		// A non-idempotent request is normally never replayed, because the
		// upstream may already have accepted it and generating an image twice
		// is not free. A connect-phase failure (refused connection, DNS
		// failure, unreachable host) is the exception the policy cannot see:
		// the request body never left this machine, so another provider can
		// serve it without any risk of duplicate work. This turns the most
		// common generation failure — one dead provider — into a success.
		if !decision.ShouldRetry && index+1 < len(candidates) && requestNeverReachedUpstream(err) {
			decision = RetryDecision{ShouldRetry: true, Reason: "request never reached the upstream"}
			slog.Info("non-idempotent request was never sent; safe to fall back", "provider", model.Provider, "model", model.UpstreamID, "nextIndex", index+1)
		}
		slog.Debug("connection error retry decision", "provider", model.Provider, "model", model.UpstreamID, "shouldRetry", decision.ShouldRetry, "reason", decision.Reason, "index", index, "candidatesLeft", len(candidates)-index-1)
		if decision.ShouldRetry && index+1 < len(candidates) {
			e.record(model, capability, false, 0, time.Since(started), nil, false)
			e.gateway.metrics.RecordFallback()
			slog.Info("fallback after connection error", "provider", model.Provider, "model", model.UpstreamID, "nextIndex", index+1)
			classified := adapter.Error{StatusCode: http.StatusBadGateway, Message: err.Error(), Retryable: true}
			return AttemptResult{Terminal: false, LastError: &classified}
		}
		if deferErrorWrite {
			classified := adapter.Error{StatusCode: http.StatusBadGateway, Message: err.Error(), Retryable: true}
			e.gateway.metrics.RecordFailure(time.Since(started), 0)
			e.record(model, capability, false, 0, time.Since(started), nil, false)
			slog.Debug("defer error write after connection error", "provider", model.Provider, "model", model.UpstreamID)
			return AttemptResult{Terminal: false, LastError: &classified}
		}
		writeError(w, http.StatusBadGateway, "all configured free providers failed")
		e.gateway.metrics.RecordFailure(time.Since(started), 0)
		e.record(model, capability, false, 0, time.Since(started), nil, false)
		return AttemptResult{Terminal: true}
	}
	spec, _ := e.gateway.registry.Get(model.Provider)
	providerAdapter := e.gateway.adapters.Resolve(spec)
	normalizedResp, err := providerAdapter.NormalizeResponse(resp)
	if err != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		e.gateway.tracker.Failure(model.ID, capability, time.Since(started), resp.StatusCode, err.Error(), 0)
		slog.Debug("normalize response failed", "provider", model.Provider, "model", model.UpstreamID, "status", resp.StatusCode, "error", err)
		e.gateway.metrics.RecordFailure(time.Since(started), resp.StatusCode)
		e.record(model, capability, false, resp.StatusCode, time.Since(started), nil, false)
		// An unreadable upstream response is an upstream fault, so it is
		// fallback-eligible instead of terminal: another free provider may
		// still answer the same request.
		classified := adapter.Error{StatusCode: http.StatusBadGateway, Message: err.Error(), Retryable: true}
		if deferErrorWrite {
			return AttemptResult{Terminal: false, LastError: &classified}
		}
		writeError(w, classified.StatusCode, classified.Message)
		return AttemptResult{Terminal: true}
	}
	slog.Debug("normalized response", "provider", model.Provider, "model", model.UpstreamID, "status", normalizedResp.StatusCode, "hasError", normalizedResp.Error != nil, "errorMsg", func() string {
		if normalizedResp.Error != nil {
			return normalizedResp.Error.Message
		} else {
			return ""
		}
	}(), "errorRetryable", func() bool {
		if normalizedResp.Error != nil {
			return normalizedResp.Error.Retryable
		} else {
			return false
		}
	}(), "errorRateLimit", func() bool {
		if normalizedResp.Error != nil {
			return normalizedResp.Error.RateLimit
		} else {
			return false
		}
	}(), "bodyLen", len(normalizedResp.Body))
	decision := RetryDecision{}
	if normalizedResp.Error != nil && normalizedResp.Error.Retryable {
		decision = e.gateway.shouldRetry(model, capability, normalizedResp.Error.StatusCode, len(normalizedResp.Body) > 0, index)
	}
	slog.Debug("retry decision", "provider", model.Provider, "model", model.UpstreamID, "shouldRetry", decision.ShouldRetry, "reason", decision.Reason, "errorRetryable", func() bool {
		if normalizedResp.Error != nil {
			return normalizedResp.Error.Retryable
		} else {
			return false
		}
	}(), "index", index, "candidatesLeft", len(candidates)-index-1)
	if decision.ShouldRetry && index+1 < len(candidates) {
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		e.gateway.tracker.Failure(model.ID, capability, time.Since(started), resp.StatusCode, resp.Status, retryAfter)
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		slog.Info("free provider unavailable; trying next", "provider", model.Provider, "model", model.UpstreamID, "status", resp.StatusCode, "reason", decision.Reason)
		e.gateway.metrics.RecordFallback()
		e.record(model, capability, false, normalizedResp.Error.StatusCode, time.Since(started), nil, false)
		if resp.StatusCode == http.StatusTooManyRequests {
			e.gateway.metrics.RecordRateLimit()
		}
		sleepBackoff(r.Context(), fallbackDelay(candidates, index, decision.Delay, retryAfter))
		// Report the failure even though the request continues: request-level
		// diagnostics must be able to name every provider that was tried, not
		// only the one that happened to fail last.
		classified := *normalizedResp.Error
		return AttemptResult{Terminal: false, LastError: &classified, RetryAfter: int(retryAfter / time.Second)}
	}
	if normalizedResp.Error != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		e.gateway.tracker.Failure(model.ID, capability, time.Since(started), normalizedResp.Error.StatusCode, normalizedResp.Error.Message, 0)
		e.gateway.metrics.RecordFailure(time.Since(started), normalizedResp.Error.StatusCode)
		e.record(model, capability, false, normalizedResp.Error.StatusCode, time.Since(started), nil, false)
		slog.Debug("non-retryable error, deferErrorWrite=%v", "provider", model.Provider, "model", model.UpstreamID, "status", normalizedResp.Error.StatusCode, "message", normalizedResp.Error.Message, "deferErrorWrite", deferErrorWrite)
		if deferErrorWrite {
			classified := *normalizedResp.Error
			return AttemptResult{Terminal: false, LastError: &classified, RetryAfter: int(parseRetryAfter(resp.Header.Get("Retry-After")) / time.Second)}
		}
		writeError(w, normalizedResp.Error.StatusCode, normalizedResp.Error.Message)
		return AttemptResult{Terminal: true}
	}
	result := copyResponse(w, resp, model, started)
	total := time.Since(started)
	served := newServedRecord(model, capability, resp.StatusCode, total, result)
	canceled := r.Context().Err() != nil || result.DownstreamError
	if canceled {
		e.gateway.metrics.RecordTransfer(false, total, result.TTFB, result.BytesWritten, 0, true)
		e.record(model, capability, false, 0, total, result.Usage, false)
		return AttemptResult{Terminal: true, Served: served}
	}
	if len(result.ToolCallAnomalies) > 0 {
		// The body was forwarded unchanged; without this line the caller's
		// parse failure would have no counterpart on the gateway side.
		e.gateway.metrics.RecordToolCallAnomaly()
		slog.Warn("upstream tool_calls payload is not OpenAI-shaped",
			"provider", model.Provider, "model", model.UpstreamID, "capability", capability,
			"status", resp.StatusCode, "stream", result.Stream, "issues", result.ToolCallAnomalies,
			"request_id", w.Header().Get("X-Request-Id"))
	}
	if result.Complete && resp.StatusCode < http.StatusBadRequest {
		e.gateway.tracker.Success(model.ID, capability, total, resp.StatusCode)
		e.gateway.metrics.RecordTransfer(true, total, result.TTFB, result.BytesWritten, resp.StatusCode, false)
		e.record(model, capability, true, resp.StatusCode, total, result.Usage, result.UsageExpected && result.Usage == nil)
	} else {
		message := resp.Status
		if result.Error != nil {
			message = result.Error.Error()
		}
		e.gateway.tracker.Failure(model.ID, capability, total, resp.StatusCode, message, parseRetryAfter(resp.Header.Get("Retry-After")))
		e.gateway.metrics.RecordTransfer(false, total, result.TTFB, result.BytesWritten, resp.StatusCode, false)
		e.record(model, capability, false, resp.StatusCode, total, result.Usage, false)
	}
	return AttemptResult{Terminal: true, Served: served}
}

// newServedRecord captures the attempt that produced the downstream response,
// so the access log can name the provider and model behind a stable capability
// alias.
func newServedRecord(model catalog.Model, capability string, status int, latency time.Duration, result StreamResult) *ServedRecord {
	return &ServedRecord{
		Provider:     model.Provider,
		Model:        model.UpstreamID,
		Capability:   capability,
		Status:       status,
		TTFB:         result.TTFB,
		Latency:      latency,
		Bytes:        result.BytesWritten,
		Stream:       result.Stream,
		Usage:        result.Usage,
		UsageMissing: result.UsageExpected && result.Usage == nil,
	}
}

func (e *AttemptExecutor) record(model catalog.Model, capability string, success bool, status int, latency time.Duration, usage *statistics.Usage, missingUsage bool) {
	if err := e.gateway.stats.Record(statistics.Attempt{
		Model: model.ID, Provider: model.Provider, Capability: capability,
		Success: success, StatusCode: status, Latency: latency, Usage: usage,
		MissingUsage: missingUsage,
	}); err != nil {
		slog.Error("persist model statistics", "model", model.ID, "error", err)
	}
}
