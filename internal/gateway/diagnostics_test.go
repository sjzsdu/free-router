package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/health"
	"github.com/sjzsdu/free-router/internal/provider"
)

// failingPoolGateway builds a gateway backed by two separate free providers,
// each offering one text model, and answers every chat call with respond.
//
// Two providers — not two models on one provider — is the topology that
// matters here: a hard failure such as 401 puts the whole provider into
// cooldown, so a second model on the same provider is never tried. The pool
// behaviour under test is what a real deployment looks like: several
// independent free services failing for different reasons.
func failingPoolGateway(t *testing.T, respond func(provider, model string) *http.Response) *Gateway {
	t.Helper()
	clearBuiltinKeys(t)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		name := poolProviderFor(r.URL.Host)
		switch r.URL.Path {
		case "/models":
			model := "free/" + name
			return testResponse(http.StatusOK, fmt.Sprintf(`{"data":[
				{"id":%q,"pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]}}
			]}`, model)), nil
		case "/chat/completions":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			model, _ := body["model"].(string)
			return respond(name, model), nil
		default:
			return testResponse(http.StatusNotFound, "not found"), nil
		}
	})}
	registry, err := provider.NewRegistry(`[
		{"id":"alpha","base_url":"https://alpha.invalid","api_key":"alpha-key"},
		{"id":"beta","base_url":"https://beta.invalid","api_key":"beta-key"}
	]`)
	if err != nil {
		t.Fatal(err)
	}
	store := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), client)
	discoverModelsForTest(t, store)
	return New(store, registry, Config{MaxAttempts: 2, Health: health.New()}, client)
}

// poolProviderFor maps a fake upstream host back to its provider id.
func poolProviderFor(host string) string {
	if strings.HasPrefix(host, "beta.") {
		return "beta"
	}
	return "alpha"
}

func chatCompletion(t *testing.T, handler http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[]}`))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeErrorBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("invalid error body %q: %v", recorder.Body.String(), err)
	}
	if payload.Error == nil {
		t.Fatalf("error body has no error envelope: %q", recorder.Body.String())
	}
	return payload.Error
}

func attemptList(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["attempts"].([]any)
	if !ok {
		t.Fatalf("error body has no attempts list: %#v", body)
	}
	attempts := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		record, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("attempt is not an object: %#v", item)
		}
		attempts = append(attempts, record)
	}
	return attempts
}

// A whole pool that is merely rate limited must not look like a client error,
// and the caller must see every provider that was tried plus when to come back.
func TestPoolWideRateLimitIsAttributed(t *testing.T) {
	handler := failingPoolGateway(t, func(string, string) *http.Response {
		response := testResponse(http.StatusTooManyRequests, `{"error":{"message":"rate limit reached"}}`)
		response.Header.Set("Retry-After", "1")
		return response
	})

	recorder := chatCompletion(t, handler)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d (body=%s)", recorder.Code, http.StatusTooManyRequests, recorder.Body.String())
	}
	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want %q", got, "1")
	}
	body := decodeErrorBody(t, recorder)
	if code, _ := body["code"].(string); code != "rate_limited" {
		t.Errorf("code = %q, want rate_limited", code)
	}
	attempts := attemptList(t, body)
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want 2 (each provider must be named): %#v", len(attempts), attempts)
	}
	seen := map[string]bool{}
	for _, attempt := range attempts {
		providerName, _ := attempt["provider"].(string)
		seen[providerName] = true
		if model, _ := attempt["model"].(string); model == "" {
			t.Errorf("attempt is missing its upstream model: %#v", attempt)
		}
		if status, _ := attempt["status"].(float64); int(status) != http.StatusTooManyRequests {
			t.Errorf("attempt status = %v, want 429", attempt["status"])
		}
		if rateLimited, _ := attempt["rate_limited"].(bool); !rateLimited {
			t.Errorf("attempt should be flagged as rate limited: %#v", attempt)
		}
	}
	if !seen["alpha"] || !seen["beta"] {
		t.Errorf("attempts must name both providers, got %#v", seen)
	}
	if requestID, _ := body["request_id"].(string); requestID == "" || requestID != recorder.Header().Get("X-Request-Id") {
		t.Errorf("request_id = %q, header = %q; they must match and be non-empty", requestID, recorder.Header().Get("X-Request-Id"))
	}
}

// Exhausted free accounts are a pool problem, not a caller credential problem:
// reporting the last upstream's 401 would send every downstream agent hunting
// for a key that is actually fine.
func TestAccountFailuresReportServerSideNotClientSide(t *testing.T) {
	handler := failingPoolGateway(t, func(string, string) *http.Response {
		return testResponse(http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`)
	})

	recorder := chatCompletion(t, handler)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (body=%s)", recorder.Code, http.StatusServiceUnavailable, recorder.Body.String())
	}
	body := decodeErrorBody(t, recorder)
	if code, _ := body["code"].(string); code != "accounts_unavailable" {
		t.Errorf("code = %q, want accounts_unavailable", code)
	}
	if message, _ := body["message"].(string); !strings.Contains(message, "unavailable") {
		t.Errorf("message = %q, want an account-unavailable explanation", message)
	}
	if attempts := attemptList(t, body); len(attempts) != 2 {
		t.Errorf("attempts = %d, want 2: %#v", len(attempts), attempts)
	}
}

// A single-candidate deployment keeps the exact upstream status and wording it
// had before diagnostics existed; the attempt list is purely additive.
func TestSingleCandidateErrorIsPassedThroughVerbatim(t *testing.T) {
	clearBuiltinKeys(t)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/models":
			return testResponse(http.StatusOK, `{"data":[
				{"id":"free/a","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]}}
			]}`), nil
		case "/chat/completions":
			return testResponse(http.StatusBadRequest, `{"error":{"message":"missing required field: messages"}}`), nil
		default:
			return testResponse(http.StatusNotFound, "not found"), nil
		}
	})}
	registry, err := provider.NewRegistry(`[{"id":"test","base_url":"https://test.invalid","api_key":"test"}]`)
	if err != nil {
		t.Fatal(err)
	}
	store := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), client)
	discoverModelsForTest(t, store)
	handler := New(store, registry, Config{MaxAttempts: 1, Health: health.New()}, client)

	recorder := chatCompletion(t, handler)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body=%s)", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	body := decodeErrorBody(t, recorder)
	if message, _ := body["message"].(string); message != "missing required field: messages" {
		t.Errorf("message = %q, want the upstream wording unchanged", message)
	}
	if code, _ := body["code"].(string); code != "client_error" {
		t.Errorf("code = %q, want client_error", code)
	}
	if attempts := attemptList(t, body); len(attempts) != 1 {
		t.Errorf("attempts = %d, want 1", len(attempts))
	}
}

// A pool that only timed out should read as a gateway timeout rather than a
// generic bad gateway.
func TestTimeoutOnlyPoolReportsGatewayTimeout(t *testing.T) {
	handler := failingPoolGateway(t, func(string, string) *http.Response {
		return testResponse(http.StatusGatewayTimeout, `{"error":{"message":"upstream timed out"}}`)
	})

	recorder := chatCompletion(t, handler)

	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d (body=%s)", recorder.Code, http.StatusGatewayTimeout, recorder.Body.String())
	}
	body := decodeErrorBody(t, recorder)
	if code, _ := body["code"].(string); code != "upstream_timeout" {
		t.Errorf("code = %q, want upstream_timeout", code)
	}
}

// The ordering rules live in one pure function; keeping them unit-tested means
// the candidate rotation in the planner cannot make these assertions flaky.
func TestSummarizeAttemptsRules(t *testing.T) {
	rateLimited := attemptRecord{Provider: "alpha", Model: "m", Status: http.StatusTooManyRequests, Message: "rate limit reached", Retryable: true, RateLimit: true, RetryAfter: 7}
	slowerRateLimit := attemptRecord{Provider: "beta", Model: "m", Status: http.StatusTooManyRequests, Message: "rate limit reached", Retryable: true, RateLimit: true, RetryAfter: 3}
	account := attemptRecord{Provider: "beta", Model: "m", Status: http.StatusUnauthorized, Message: "provider account unavailable", Retryable: true}
	timeout := attemptRecord{Provider: "beta", Model: "m", Status: http.StatusGatewayTimeout, Message: "upstream timed out", Retryable: true}
	serverError := attemptRecord{Provider: "beta", Model: "m", Status: http.StatusInternalServerError, Message: "server error", Retryable: true}
	clientError := attemptRecord{Provider: "beta", Model: "m", Status: http.StatusBadRequest, Message: "invalid parameter 'temperature'", Retryable: false}

	tests := []struct {
		name     string
		attempts []attemptRecord
		status   int
		code     string
		message  string
		retry    int
	}{
		{
			name:     "rate limit outranks a sibling 5xx",
			attempts: []attemptRecord{rateLimited, serverError},
			status:   http.StatusTooManyRequests,
			code:     "rate_limited",
			retry:    7,
		},
		{
			name:     "smallest upstream retry-after wins",
			attempts: []attemptRecord{rateLimited, slowerRateLimit},
			status:   http.StatusTooManyRequests,
			code:     "rate_limited",
			retry:    3,
		},
		{
			name:     "accounts exhausted is server side",
			attempts: []attemptRecord{account, timeout},
			status:   http.StatusServiceUnavailable,
			code:     "accounts_unavailable",
		},
		{
			name:     "all timeouts is a gateway timeout",
			attempts: []attemptRecord{timeout, timeout},
			status:   http.StatusGatewayTimeout,
			code:     "upstream_timeout",
		},
		{
			name:     "mixed upstream faults stay a bad gateway",
			attempts: []attemptRecord{serverError, timeout},
			status:   http.StatusBadGateway,
			code:     "upstream_failed",
		},
		{
			name:     "a repairable client error is never dressed up as a pool fault",
			attempts: []attemptRecord{rateLimited, clientError},
			status:   http.StatusBadRequest,
			code:     "client_error",
			message:  "invalid parameter 'temperature'",
		},
		{
			name:     "single attempt is passed through verbatim",
			attempts: []attemptRecord{serverError},
			status:   http.StatusInternalServerError,
			code:     "upstream_failed",
			message:  "server error",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			summary := summarizeAttempts(test.attempts)
			if summary.Status != test.status {
				t.Errorf("status = %d, want %d", summary.Status, test.status)
			}
			if summary.Code != test.code {
				t.Errorf("code = %q, want %q", summary.Code, test.code)
			}
			if test.message != "" && summary.Message != test.message {
				t.Errorf("message = %q, want %q", summary.Message, test.message)
			}
			if summary.RetryAfter != test.retry {
				t.Errorf("retryAfter = %d, want %d", summary.RetryAfter, test.retry)
			}
		})
	}
}

// A rate-limited provider's Retry-After describes *its* limit. Waiting for it
// while switching to another provider adds the whole hint to the caller's
// latency and buys nothing upstream, because the failed model is already
// cooling down. This is the end-to-end guard for that rule.
func TestCrossProviderFallbackDoesNotWaitForRetryAfter(t *testing.T) {
	handler := failingPoolGateway(t, func(name, _ string) *http.Response {
		if name == "alpha" {
			response := testResponse(http.StatusTooManyRequests, `{"error":{"message":"rate limit reached"}}`)
			response.Header.Set("Retry-After", "3")
			return response
		}
		return testResponse(http.StatusOK, `{"choices":[{"message":{"content":"ok"}}]}`)
	})

	started := time.Now()
	recorder := chatCompletion(t, handler)
	elapsed := time.Since(started)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body=%s)", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("fallback took %v; it must not inherit the failed provider's Retry-After", elapsed)
	}
}

// Diagnostics must show how long each provider took to fail: an instant 401
// and a slow timeout need different fixes but used to look identical.
func TestAttemptsReportPerProviderLatency(t *testing.T) {
	handler := failingPoolGateway(t, func(string, string) *http.Response {
		time.Sleep(20 * time.Millisecond)
		return testResponse(http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
	})

	recorder := chatCompletion(t, handler)
	body := decodeErrorBody(t, recorder)
	attempts := attemptList(t, body)
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want 2 (%#v)", len(attempts), attempts)
	}
	for _, attempt := range attempts {
		latency, ok := attempt["latency_ms"].(float64)
		if !ok {
			t.Fatalf("attempt has no latency_ms: %#v", attempt)
		}
		if latency < 20 {
			t.Errorf("latency_ms = %v, want >= 20 (the simulated upstream delay)", latency)
		}
	}
	if elapsed, ok := body["elapsed_ms"].(float64); !ok || elapsed < 20 {
		t.Fatalf("elapsed_ms = %#v, want >= 20", body["elapsed_ms"])
	}
}
