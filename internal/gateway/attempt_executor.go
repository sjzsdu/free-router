package gateway

import (
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/statistics"
)

// AttemptResult describes whether an upstream attempt produced a terminal
// downstream response or whether fallback should continue.
type AttemptResult struct {
	Terminal bool
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

func (e *AttemptExecutor) Execute(w http.ResponseWriter, r *http.Request, model catalog.Model, capability, endpoint string, payload []byte, contentType string, index int, candidates []catalog.Model) AttemptResult {

	started := time.Now()
	resp, err := e.gateway.forward(r, model, payload, endpoint, contentType)
	if err != nil {
		if status, message, local := localLimitError(err); local {
			// Local backpressure (queue full / rate-limit timeout) is
			// reported as-is instead of falling back to another provider:
			// this gateway's own limits must not amplify load across
			// providers. Upstream failures still fall back.
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
		if decision.ShouldRetry && index+1 < len(candidates) {
			e.record(model, capability, false, 0, time.Since(started), nil, false)
			e.gateway.metrics.RecordFallback()
			// No backoff sleep on connection errors: fallback moves to a
			// different provider, and waiting would only delay recovery.
			return AttemptResult{Terminal: false}
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
		writeError(w, http.StatusBadGateway, err.Error())
		e.gateway.metrics.RecordFailure(time.Since(started), resp.StatusCode)
		e.record(model, capability, false, resp.StatusCode, time.Since(started), nil, false)
		return AttemptResult{Terminal: true}
	}
	decision := RetryDecision{}
	if normalizedResp.Error != nil && normalizedResp.Error.Retryable {
		decision = e.gateway.shouldRetry(model, capability, normalizedResp.Error.StatusCode, len(normalizedResp.Body) > 0, index)
	}
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
		sleepBackoff(r.Context(), effectiveDelay(decision.Delay, retryAfter))
		return AttemptResult{Terminal: false}
	}
	if normalizedResp.Error != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		e.gateway.tracker.Failure(model.ID, capability, time.Since(started), normalizedResp.Error.StatusCode, normalizedResp.Error.Message, 0)
		writeError(w, normalizedResp.Error.StatusCode, normalizedResp.Error.Message)
		e.gateway.metrics.RecordFailure(time.Since(started), normalizedResp.Error.StatusCode)
		e.record(model, capability, false, normalizedResp.Error.StatusCode, time.Since(started), nil, false)
		return AttemptResult{Terminal: true}
	}
	result := copyResponse(w, resp, model, started)
	total := time.Since(started)
	canceled := r.Context().Err() != nil || result.DownstreamError
	if canceled {
		e.gateway.metrics.RecordTransfer(false, total, result.TTFB, result.BytesWritten, 0, true)
		e.record(model, capability, false, 0, total, result.Usage, false)
		return AttemptResult{Terminal: true}
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
	return AttemptResult{Terminal: true}

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
