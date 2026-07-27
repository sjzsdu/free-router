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

	semAvailable := false
	done := make(chan struct{})
	go func() {
		err3 := limiter.Acquire(context.Background())
		if err3 != nil {
			t.Fatalf("third acquire failed: %v", err3)
		}
		semAvailable = true
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	if semAvailable {
		t.Fatal("third acquire should block until release")
	}

	limiter.Release()
	select {
	case <-done:
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
	inProgress := make(chan int, 10)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			if err := limiter.Acquire(context.Background()); err != nil {
				t.Errorf("acquire %d failed: %v", id, err)
				return
			}
			inProgress <- id
			time.Sleep(10 * time.Millisecond)
			<-inProgress
			limiter.Release()
		}(i)
	}

	wg.Wait()
}
