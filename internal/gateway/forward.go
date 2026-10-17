package gateway

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/sjzsdu/free-router/internal/adapter"
	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/provider"
	"github.com/sjzsdu/free-router/internal/transport"
)

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
