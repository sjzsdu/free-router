package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/sjzsdu/free-router/internal/catalog"
)

// Phases of a routed request. A tools-bearing chat request first walks the
// chat-tools pool and may then replay against plain chat candidates with the
// tools field stripped; diagnostics keep the two passes distinguishable
// because the same provider can appear in both.
const (
	phaseStrict = "strict"
	phaseLoose  = "loose"
)

// AttemptRecord is one upstream attempt that failed while serving a single
// downstream request.
//
// Free providers fail for very different reasons — a revoked key, a spent
// daily quota, a transient 429, a dead host — and before this the caller only
// ever saw whichever candidate happened to fail last. That made an exhausted
// free-tier pool look like the caller's own credentials problem, which is the
// single most expensive misunderstanding for an OpenAI-compatible router. The
// records below let the final error explain every upstream that was tried.
type AttemptRecord struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Capability string `json:"capability,omitempty"`
	Phase      string `json:"phase,omitempty"`
	Status     int    `json:"status"`
	Message    string `json:"message,omitempty"`
	Retryable  bool   `json:"retryable"`
	RateLimit  bool   `json:"rate_limited,omitempty"`
	RetryAfter int    `json:"retry_after_seconds,omitempty"`
	// LatencyMS separates "this provider is wrong" from "this provider is
	// slow": an instant 401 is a configuration problem, a 30s hang before a
	// 504 is a network or overload problem. Without it every failure looks
	// the same in the error body.
	LatencyMS int64 `json:"latency_ms,omitempty"`
}

// SkippedRecord explains a configured candidate that was never attempted, for
// example because its health tracker had no in-flight slot left. Without it a
// "no acquirable models" response looks like a configuration mistake.
type SkippedRecord struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Reason   string `json:"reason"`
}

// FailureRecord is one request whose every candidate failed, kept in a bounded
// in-memory log so the admin UI can answer "why did that just fail?" without a
// caller having to quote a request id from their client logs. It is the same
// information the error body carries, retained for offline inspection.
type FailureRecord struct {
	At        time.Time       `json:"at"`
	RequestID string          `json:"request_id,omitempty"`
	Route     string          `json:"route,omitempty"`
	Status    int             `json:"status"`
	Code      string          `json:"code"`
	Message   string          `json:"message"`
	ElapsedMS int64           `json:"elapsed_ms,omitempty"`
	Attempts  []AttemptRecord `json:"attempts,omitempty"`
	Skipped   []SkippedRecord `json:"skipped,omitempty"`
}

// failureLog is a fixed-size ring of the most recent pool-wide failures. It
// intentionally lives in memory only: it is a diagnostic convenience, and the
// persisted statistics already cover the durable aggregates.
type failureLog struct {
	mu      sync.Mutex
	records []FailureRecord
	next    int
}

func newFailureLog(capacity int) *failureLog {
	if capacity <= 0 {
		capacity = defaultFailureLogCapacity
	}
	return &failureLog{records: make([]FailureRecord, capacity)}
}

const defaultFailureLogCapacity = 50

func (l *failureLog) add(record FailureRecord) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records[l.next] = record
	l.next = (l.next + 1) % len(l.records)
}

// List returns the recorded failures, newest first, skipping unused slots.
func (l *failureLog) List() []FailureRecord {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	result := make([]FailureRecord, 0, len(l.records))
	for offset := 1; offset <= len(l.records); offset++ {
		record := l.records[(l.next-offset+len(l.records))%len(l.records)]
		if record.At.IsZero() {
			continue
		}
		result = append(result, record)
	}
	return result
}

// requestDiagnostics accumulates what happened to one downstream request so
// the terminal error can be attributed instead of guessed.
type requestDiagnostics struct {
	RequestID string
	Route     string
	Attempts  []AttemptRecord
	Skipped   []SkippedRecord
	// StartedAt measures the whole request, so the caller can tell whether the
	// gateway spent its time trying providers or simply failed fast.
	StartedAt time.Time
	// sink, when set, retains the terminal diagnosis for the admin UI.
	sink *failureLog
}

func newRequestDiagnostics(requestID, route string, sink *failureLog) *requestDiagnostics {
	return &requestDiagnostics{RequestID: requestID, Route: route, StartedAt: time.Now(), sink: sink}
}

// recordAttempt keeps the failure carried by a non-terminal or terminal
// attempt result. Successful attempts are not recorded: the response is
// already being served and no diagnosis is needed.
func (d *requestDiagnostics) recordAttempt(model catalog.Model, capability, phase string, result AttemptResult) {
	if d == nil || result.LastError == nil {
		return
	}
	d.Attempts = append(d.Attempts, AttemptRecord{
		Provider:   model.Provider,
		Model:      model.UpstreamID,
		Capability: capability,
		Phase:      phase,
		Status:     result.LastError.StatusCode,
		Message:    result.LastError.Message,
		Retryable:  result.LastError.Retryable,
		RateLimit:  result.LastError.RateLimit,
		RetryAfter: result.RetryAfter,
		LatencyMS:  result.Latency.Milliseconds(),
	})
}

func (d *requestDiagnostics) skip(model catalog.Model, reason string) {
	if d == nil {
		return
	}
	d.Skipped = append(d.Skipped, SkippedRecord{Provider: model.Provider, Model: model.UpstreamID, Reason: reason})
}

// failureSummary is the single downstream-facing verdict for a request whose
// every candidate failed.
type failureSummary struct {
	Status     int
	Message    string
	Code       string
	RetryAfter int
}

// summarizeAttempts turns the per-provider failures of one request into the
// status code and message the caller should actually see. The rules are
// ordered so that the caller learns the most actionable thing first:
//
//  1. A single attempt is passed through verbatim, so a one-candidate
//     deployment keeps exactly the upstream status and wording it had before
//     diagnostics existed; the attempt list is purely additive.
//  2. A non-retryable 4xx (excluding 408/429) means the request itself is
//     malformed. Reporting it as a gateway failure would send the caller
//     hunting for a provider outage that does not exist, so it is passed
//     through even after other providers were tried.
//  3. Any rate limit means retrying is worthwhile: 429 plus the smallest
//     upstream Retry-After hint wins over a sibling's 5xx.
//  4. Account-only failures (401/402/403) are reported as 503, not 4xx: the
//     caller's own credentials are fine, the pool is simply unusable.
//  5. Every failure being a timeout/transport error becomes 504.
//  6. Anything else is a plain 502 upstream failure.
func summarizeAttempts(attempts []AttemptRecord) failureSummary {
	if len(attempts) == 0 {
		return failureSummary{Status: http.StatusServiceUnavailable, Message: "all configured free providers failed", Code: "upstream_failed"}
	}
	if len(attempts) == 1 {
		only := attempts[0]
		return failureSummary{Status: only.Status, Message: only.Message, Code: codeForAttempt(only), RetryAfter: only.RetryAfter}
	}
	for index := len(attempts) - 1; index >= 0; index-- {
		record := attempts[index]
		if isClientRepairable(record) {
			return failureSummary{Status: record.Status, Message: record.Message, Code: "client_error"}
		}
	}
	rateLimited := false
	accountUnavailable := false
	allTimeouts := true
	smallestRetryAfter := 0
	for _, record := range attempts {
		if record.RateLimit || record.Status == http.StatusTooManyRequests {
			rateLimited = true
			if record.RetryAfter > 0 && (smallestRetryAfter == 0 || record.RetryAfter < smallestRetryAfter) {
				smallestRetryAfter = record.RetryAfter
			}
		}
		if isAccountStatus(record.Status) {
			accountUnavailable = true
		}
		if !isTimeoutStatus(record.Status) {
			allTimeouts = false
		}
	}
	switch {
	case rateLimited:
		return failureSummary{
			Status:     http.StatusTooManyRequests,
			Message:    fmt.Sprintf("all %d configured free providers are rate limited; retry shortly", len(attempts)),
			Code:       "rate_limited",
			RetryAfter: smallestRetryAfter,
		}
	case accountUnavailable:
		return failureSummary{
			Status:  http.StatusServiceUnavailable,
			Message: fmt.Sprintf("all %d configured free provider accounts are unavailable; check provider credentials in the admin UI", len(attempts)),
			Code:    "accounts_unavailable",
		}
	case allTimeouts:
		return failureSummary{
			Status:  http.StatusGatewayTimeout,
			Message: fmt.Sprintf("all %d configured free providers timed out", len(attempts)),
			Code:    "upstream_timeout",
		}
	default:
		return failureSummary{
			Status:  http.StatusBadGateway,
			Message: fmt.Sprintf("all %d configured free providers failed", len(attempts)),
			Code:    "upstream_failed",
		}
	}
}

func isClientRepairable(record AttemptRecord) bool {
	if record.Retryable {
		return false
	}
	if record.Status < http.StatusBadRequest || record.Status >= http.StatusInternalServerError {
		return false
	}
	return record.Status != http.StatusRequestTimeout && record.Status != http.StatusTooManyRequests
}

func isAccountStatus(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusPaymentRequired || status == http.StatusForbidden
}

func isTimeoutStatus(status int) bool {
	return status == 0 || status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout
}

func codeForAttempt(record AttemptRecord) string {
	switch {
	case record.RateLimit || record.Status == http.StatusTooManyRequests:
		return "rate_limited"
	case isAccountStatus(record.Status):
		return "account_unavailable"
	case isTimeoutStatus(record.Status):
		return "upstream_timeout"
	case isClientRepairable(record):
		return "client_error"
	default:
		return "upstream_failed"
	}
}

// writeFailure renders the terminal error for a request whose candidates all
// failed. The envelope stays OpenAI-compatible (error.message/type/code);
// attempts, skipped and request_id are additive fields that let a caller —
// or the human reading its logs — see why the pool was unusable.
func (d *requestDiagnostics) writeFailure(w http.ResponseWriter, summary failureSummary) {
	if summary.RetryAfter > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", summary.RetryAfter))
	}
	// Retain the terminal diagnosis for the admin UI before writing it out: the
	// caller sees the body, the operator sees the same record later without
	// having to reproduce the failure.
	if d != nil && d.sink != nil {
		d.sink.add(FailureRecord{
			At:        time.Now(),
			RequestID: d.RequestID,
			Route:     d.Route,
			Status:    summary.Status,
			Code:      summary.Code,
			Message:   summary.Message,
			ElapsedMS: time.Since(d.StartedAt).Milliseconds(),
			Attempts:  append([]AttemptRecord(nil), d.Attempts...),
			Skipped:   append([]SkippedRecord(nil), d.Skipped...),
		})
	}
	d.write(w, summary.Status, summary.Message, summary.Code)
}

func (d *requestDiagnostics) write(w http.ResponseWriter, status int, message, code string) {
	body := map[string]any{
		"message": message,
		"type":    "free_router_error",
		"code":    code,
	}
	if d != nil {
		if d.RequestID != "" {
			body["request_id"] = d.RequestID
		}
		if d.Route != "" {
			body["route"] = d.Route
		}
		if len(d.Attempts) > 0 {
			body["attempts"] = d.Attempts
		}
		if len(d.Skipped) > 0 {
			body["skipped"] = d.Skipped
		}
		// Only report elapsed time for a request that actually reached the
		// pool; a body that failed validation has nothing to explain.
		if len(d.Attempts) > 0 && !d.StartedAt.IsZero() {
			body["elapsed_ms"] = time.Since(d.StartedAt).Milliseconds()
		}
	}
	writeJSON(w, status, map[string]any{"error": body})
}

// newRequestID returns a short random correlation id. An empty string means
// the entropy source failed; callers then simply omit the header instead of
// failing the request over a diagnostic nicety.
func newRequestID() string {
	var buffer [8]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(buffer[:])
}
