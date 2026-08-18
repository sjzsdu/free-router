package transport

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestLimiterAcquireRelease(t *testing.T) {
	config := NewRateLimitConfig()
	config.MaxConcurrentRequests = 2
	limiter := NewLimiter(config)
	defer limiter.Close()

	err1 := limiter.Acquire(context.Background())
	if err1 != nil {
		t.Fatalf("first acquire failed: %v", err1)
	}

	err2 := limiter.Acquire(context.Background())
	if err2 != nil {
		t.Fatalf("second acquire failed: %v", err2)
	}

	done := make(chan error, 1)
	go func() {
		err3 := limiter.Acquire(context.Background())
		done <- err3
	}()

	time.Sleep(50 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("third acquire should block until release")
	default:
	}

	limiter.Release()
	select {
	case err3 := <-done:
		if err3 != nil {
			t.Fatalf("third acquire failed: %v", err3)
		}
	case <-time.After(time.Second):
		t.Fatal("third acquire should succeed after release")
	}

	limiter.Release()
}

func TestLimiterRateLimit(t *testing.T) {
	config := NewRateLimitConfig()
	config.RateLimitPerSecond = 10
	limiter := NewLimiter(config)
	defer limiter.Close()

	start := time.Now()
	for i := 0; i < 20; i++ {
		if err := limiter.Acquire(context.Background()); err != nil {
			t.Fatalf("acquire %d failed: %v", i, err)
		}
		limiter.Release()
	}
	elapsed := time.Since(start)

	if elapsed < time.Second {
		t.Fatalf("rate limiting not working: elapsed=%v expected >= 1s", elapsed)
	}
}

func TestLimiterQueueFull(t *testing.T) {
	config := NewRateLimitConfig()
	config.QueueSize = 2
	config.MaxConcurrentRequests = 1
	limiter := NewLimiter(config)
	defer limiter.Close()

	err1 := limiter.Acquire(context.Background())
	if err1 != nil {
		t.Fatalf("first acquire failed: %v", err1)
	}

	err2 := limiter.Acquire(context.Background())
	if err2 != ErrOverloaded {
		t.Fatalf("expected ErrOverloaded, got %v", err2)
	}
}

func TestLimiterContextCancel(t *testing.T) {
	config := NewRateLimitConfig()
	config.MaxConcurrentRequests = 1
	limiter := NewLimiter(config)
	defer limiter.Close()

	err1 := limiter.Acquire(context.Background())
	if err1 != nil {
		t.Fatalf("first acquire failed: %v", err1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err2 := limiter.Acquire(ctx)
	if err2 != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err2)
	}

	limiter.Release()
}

func TestLimiterConcurrentRequests(t *testing.T) {
	config := NewRateLimitConfig()
	config.MaxConcurrentRequests = 5
	limiter := NewLimiter(config)
	defer limiter.Close()

	var wg sync.WaitGroup
	var mu sync.Mutex
	maxInProgress := 0
	currentInProgress := 0

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			if err := limiter.Acquire(context.Background()); err != nil {
				t.Errorf("acquire %d failed: %v", id, err)
				return
			}
			mu.Lock()
			currentInProgress++
			if currentInProgress > maxInProgress {
				maxInProgress = currentInProgress
			}
			mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			mu.Lock()
			currentInProgress--
			mu.Unlock()
			limiter.Release()
		}(i)
	}

	wg.Wait()

	if maxInProgress > 5 {
		t.Fatalf("max concurrent requests exceeded: %d > 5", maxInProgress)
	}
}

func TestLimiterFractionalRateDoesNotPanic(t *testing.T) {
	config := NewRateLimitConfig()
	config.RateLimitPerSecond = 0.5
	limiter := NewLimiter(config)
	defer limiter.Close()
	if err := limiter.Acquire(context.Background()); err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	limiter.Release()
}

func TestLimiterReturnsTokenOnSemaphoreTimeout(t *testing.T) {
	config := NewRateLimitConfig()
	config.MaxConcurrentRequests = 1
	config.QueueSize = 10
	config.RateLimitPerSecond = 100
	limiter := NewLimiter(config)
	defer limiter.Close()

	if err := limiter.Acquire(context.Background()); err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	// Second acquire times out waiting for the semaphore; the token it
	// consumed must be returned or the bucket permanently loses capacity.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := limiter.Acquire(ctx); err == nil {
		t.Fatal("second acquire should time out on the semaphore")
	}
	limiter.Release()
	// Drain enough acquires to prove the token was not lost.
	for i := 0; i < 100; i++ {
		if err := limiter.Acquire(context.Background()); err != nil {
			t.Fatalf("acquire %d failed after token return: %v", i, err)
		}
		limiter.Release()
	}
}
