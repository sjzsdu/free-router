package transport

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	ErrRateLimited = errors.New("rate limited")
	ErrOverloaded  = errors.New("service overloaded")
)

type RateLimitConfig struct {
	MaxConcurrentRequests int
	RateLimitPerSecond    float64
	QueueSize             int
}

func NewRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{
		MaxConcurrentRequests: 10,
		RateLimitPerSecond:    50,
		QueueSize:             100,
	}
}

type Limiter struct {
	sem    chan struct{}
	tokens chan struct{}
	ticker *time.Ticker
	queue  chan struct{}
	closed bool
	mu     sync.Mutex
}

func NewLimiter(config RateLimitConfig) *Limiter {
	if config.MaxConcurrentRequests <= 0 {
		config.MaxConcurrentRequests = 10
	}
	if config.RateLimitPerSecond <= 0 {
		config.RateLimitPerSecond = 50
	}
	// Rates in (0,1) would truncate to zero and divide-by-zero in the
	// ticker interval; clamp to a minimum of 1 token per second.
	if config.RateLimitPerSecond < 1 {
		config.RateLimitPerSecond = 1
	}
	if config.QueueSize <= 0 {
		config.QueueSize = 100
	}

	l := &Limiter{
		sem:    make(chan struct{}, config.MaxConcurrentRequests),
		queue:  make(chan struct{}, config.QueueSize),
		tokens: make(chan struct{}, int(config.RateLimitPerSecond)*2),
	}

	for i := 0; i < int(config.RateLimitPerSecond); i++ {
		l.tokens <- struct{}{}
	}

	l.ticker = time.NewTicker(time.Second / time.Duration(config.RateLimitPerSecond))
	go l.tokenRefill(config.RateLimitPerSecond)

	return l
}

func (l *Limiter) tokenRefill(rate float64) {
	tokensPerTick := 1.0
	for range l.ticker.C {
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			return
		}
		l.mu.Unlock()
		for i := 0; i < int(tokensPerTick); i++ {
			select {
			case l.tokens <- struct{}{}:
			default:
			}
		}
	}
}

func (l *Limiter) Acquire(ctx context.Context) error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return ErrOverloaded
	}
	l.mu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case l.queue <- struct{}{}:
	default:
		return ErrOverloaded
	}

	defer func() { <-l.queue }()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-l.tokens:
	case <-time.After(time.Second * 5):
		return ErrRateLimited
	}

	select {
	case <-ctx.Done():
		l.returnToken()
		return ctx.Err()
	case l.sem <- struct{}{}:
	case <-time.After(time.Second * 2):
		l.returnToken()
		return ErrOverloaded
	}

	return nil
}

// returnToken puts a consumed token back into the bucket so a timed-out or
// cancelled Acquire does not permanently lose capacity.
func (l *Limiter) returnToken() {
	select {
	case l.tokens <- struct{}{}:
	default:
	}
}

func (l *Limiter) Release() {
	<-l.sem
}

func (l *Limiter) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		l.ticker.Stop()
	}
}
