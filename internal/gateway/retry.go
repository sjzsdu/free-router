package gateway

import (
	"net/http"
	"time"

	"github.com/sjzsdu/free-router/internal/catalog"
)

type RequestType string

const (
	RequestIdempotent    RequestType = "idempotent"
	RequestNonIdempotent RequestType = "non_idempotent"
	RequestStreaming     RequestType = "streaming"
)

func requestTypeForCapability(capability string) RequestType {
	switch capability {
	case catalog.FunctionChat, catalog.FunctionEmbedding, catalog.FunctionRerank, catalog.FunctionModeration, catalog.FunctionChatTools:
		return RequestIdempotent
	case catalog.FunctionImageGeneration, catalog.FunctionVideoGeneration, catalog.FunctionTextToSpeech:
		return RequestNonIdempotent
	case catalog.FunctionSpeechToText, catalog.FunctionImageUnderstanding, catalog.FunctionVideoUnderstanding:
		return RequestIdempotent
	default:
		return RequestNonIdempotent
	}
}

type RetryDecision struct {
	ShouldRetry bool
	Reason      string
	Delay       time.Duration
}

type RetryPolicy struct {
	MaxAttempts         int
	MaxRetryDelay       time.Duration
	RetryOnClientErrors bool
	RetryOnServerErrors bool
	RetryOnRateLimit    bool
	AllowNonIdempotent  bool
}

func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:         3,
		MaxRetryDelay:       30 * time.Second,
		RetryOnClientErrors: false,
		RetryOnServerErrors: true,
		RetryOnRateLimit:    true,
		AllowNonIdempotent:  false,
	}
}

func (p RetryPolicy) ShouldRetry(reqType RequestType, statusCode int, hasResponseBody bool, attempt int) RetryDecision {
	if attempt >= p.MaxAttempts {
		return RetryDecision{ShouldRetry: false, Reason: "max attempts exceeded"}
	}

	if reqType == RequestNonIdempotent && !p.AllowNonIdempotent {
		switch {
		case statusCode == 0:
			return RetryDecision{ShouldRetry: false, Reason: "connection error on non-idempotent request"}
		case statusCode >= 500:
			return RetryDecision{ShouldRetry: false, Reason: "server error on non-idempotent request"}
		case statusCode == http.StatusRequestTimeout || statusCode == http.StatusGatewayTimeout:
			return RetryDecision{ShouldRetry: false, Reason: "timeout on non-idempotent request"}
		case statusCode >= 400:
			return RetryDecision{ShouldRetry: false, Reason: "client error on non-idempotent request"}
		case statusCode >= 200:
			return RetryDecision{ShouldRetry: false, Reason: "non-idempotent request already processed"}
		}
	}

	switch {
	case statusCode >= 500:
		if p.RetryOnServerErrors {
			return RetryDecision{ShouldRetry: true, Reason: "server error", Delay: backoffDelay(attempt, p.MaxRetryDelay)}
		}
		return RetryDecision{ShouldRetry: false, Reason: "server error not retryable"}

	case statusCode == http.StatusTooManyRequests:
		if p.RetryOnRateLimit {
			return RetryDecision{ShouldRetry: true, Reason: "rate limited", Delay: backoffDelay(attempt, p.MaxRetryDelay)}
		}
		return RetryDecision{ShouldRetry: false, Reason: "rate limit not retryable"}

	case statusCode == http.StatusRequestTimeout || statusCode == http.StatusGatewayTimeout:
		return RetryDecision{ShouldRetry: true, Reason: "timeout", Delay: backoffDelay(attempt, p.MaxRetryDelay)}

	case statusCode >= 400 && statusCode < 500:
		if p.RetryOnClientErrors {
			return RetryDecision{ShouldRetry: true, Reason: "client error", Delay: backoffDelay(attempt, p.MaxRetryDelay)}
		}
		return RetryDecision{ShouldRetry: false, Reason: "client error not retryable"}

	case statusCode == 0:
		return RetryDecision{ShouldRetry: true, Reason: "connection error", Delay: backoffDelay(attempt, p.MaxRetryDelay)}

	default:
		return RetryDecision{ShouldRetry: false, Reason: "unknown status"}
	}
}

func backoffDelay(attempt int, maxDelay time.Duration) time.Duration {
	baseDelay := 500 * time.Millisecond
	delay := baseDelay * (1 << attempt)
	if delay > maxDelay {
		delay = maxDelay
	}
	return delay
}

func (g *Gateway) shouldRetry(model catalog.Model, capability string, statusCode int, hasResponseBody bool, attempt int) RetryDecision {
	var isIdempotent bool

	if g.adapterReg != nil {
		adapter := g.adapterReg.Get(model.Provider)
		isIdempotent = adapter.IdempotencySupport(capability)
	} else {
		isIdempotent = requestTypeForCapability(capability) == RequestIdempotent
	}

	var reqType RequestType
	if isIdempotent {
		reqType = RequestIdempotent
	} else {
		reqType = RequestNonIdempotent
	}

	policy := DefaultRetryPolicy()
	policy.AllowNonIdempotent = false

	return policy.ShouldRetry(reqType, statusCode, hasResponseBody, attempt)
}
