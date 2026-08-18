package transport

import (
	"context"
	"errors"
	"math"
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
	done   chan struct{}
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
	if config.QueueSize <= 0 {
		config.QueueSize = 100
	}

	l := &Limiter{
		sem:    make(chan struct{}, config.MaxConcurrentRequests),
		queue:  make(chan struct{}, config.QueueSize),
		tokens: make(chan struct{}, max(1, int(math.Ceil(config.RateLimitPerSecond*2)))),
		done:   make(chan struct{}),
	}

	for i := 0; i < max(1, int(math.Ceil(config.RateLimitPerSecond))); i++ {
		l.tokens <- struct{}{}
	}

	l.ticker = time.NewTicker(time.Duration(float64(time.Second) / config.RateLimitPerSecond))
	go l.tokenRefill()

	return l
}

func (l *Limiter) tokenRefill() {
	for {
		select {
		case <-l.done:
			return
		case <-l.ticker.C:
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
		close(l.done)
	}
}
