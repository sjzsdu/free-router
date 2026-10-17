package gateway

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/sjzsdu/free-router/internal/statistics"
)

// ServedRecord describes the upstream attempt that produced a downstream
// response. It is the successful counterpart of AttemptRecord: failures were
// already explained per attempt, but a request that worked left no trace of
// which free provider answered it.
type ServedRecord struct {
	Provider   string
	Model      string
	Capability string
	// Phase distinguishes the strict chat-tools pass from the loose plain-chat
	// replay; the same provider can legitimately serve either.
	Phase string
	// Status is the upstream status. The access log reports the downstream
	// status separately, because error paths write their own body.
	Status  int
	TTFB    time.Duration
	Latency time.Duration
	Bytes   int64
	Stream  bool
	Usage   *statistics.Usage
	// UsageMissing is true when the upstream response should have reported
	// token usage but did not, which is the difference between a provider
	// with no accounting and one that answered without a body.
	UsageMissing bool
}

// responseRecorder captures the status code and body size the caller actually
// received, so the access log reports the downstream result rather than an
// upstream intention.
//
// It deliberately forwards Flush: SSE streaming is the gateway's primary mode,
// and a wrapper that stopped satisfying http.Flusher would buffer every stream
// and turn a token-by-token answer into one late burst.
type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func newResponseRecorder(w http.ResponseWriter) *responseRecorder {
	return &responseRecorder{ResponseWriter: w}
}

func (r *responseRecorder) WriteHeader(status int) {
	if !r.wrote {
		r.status = status
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(p []byte) (int, error) {
	if !r.wrote {
		r.status = http.StatusOK
		r.wrote = true
	}
	written, err := r.ResponseWriter.Write(p)
	r.bytes += int64(written)
	return written, err
}

func (r *responseRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap exposes the underlying writer to http.ResponseController so future
// per-response options keep working through the wrapper.
func (r *responseRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// Status is the downstream status code, defaulting to 200 for a handler that
// wrote a body without an explicit header.
func (r *responseRecorder) Status() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

// logRequest emits exactly one line per inference request. Before this the
// gateway logged only failures and debug-level internals, so "which provider
// answered, how many did it fall back through, how long did it take" could
// only be reconstructed from error bodies and /metrics aggregates. The fields
// are chosen so a single `grep request_id=<id>` explains one call end to end.
func (g *Gateway) logRequest(r *http.Request, diagnostics *requestDiagnostics, started time.Time, recorder *responseRecorder) {
	if diagnostics == nil || recorder == nil {
		return
	}
	status := recorder.Status()
	level := slog.LevelInfo
	switch {
	case status >= http.StatusInternalServerError:
		level = slog.LevelError
	case status >= http.StatusBadRequest:
		level = slog.LevelWarn
	}

	// A served attempt is itself an upstream attempt; failures are counted
	// from the records collected along the way.
	attempts := len(diagnostics.Attempts)
	if diagnostics.Served != nil {
		attempts++
	}

	attrs := []any{
		"request_id", diagnostics.RequestID,
		"method", r.Method,
		"path", r.URL.Path,
		"route", diagnostics.Route,
		"status", status,
		"attempts", attempts,
		"duration_ms", time.Since(started).Milliseconds(),
		"bytes", recorder.bytes,
	}
	if served := diagnostics.Served; served != nil {
		attrs = append(attrs,
			"provider", served.Provider,
			"model", served.Model,
			"capability", served.Capability,
			"ttfb_ms", served.TTFB.Milliseconds(),
		)
		if served.Phase == phaseLoose {
			// A loose replay dropped tool definitions to get an answer; that
			// is a correctness-relevant fact, not a detail.
			attrs = append(attrs, "phase", served.Phase)
		}
		if served.Stream {
			attrs = append(attrs, "stream", true)
		}
		if served.Usage != nil {
			attrs = append(attrs,
				"input_tokens", served.Usage.InputTokens,
				"output_tokens", served.Usage.OutputTokens,
				"total_tokens", served.Usage.TotalTokens,
			)
		} else if served.UsageMissing {
			attrs = append(attrs, "usage_missing", true)
		}
	} else if diagnostics.code != "" {
		attrs = append(attrs, "code", diagnostics.code)
	}
	slog.Log(r.Context(), level, "request", attrs...)
}
