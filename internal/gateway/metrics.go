package gateway

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"
)

type Metrics struct {
	Requests         atomic.Uint64
	Successes        atomic.Uint64
	Failures         atomic.Uint64
	FallbackAttempts atomic.Uint64
	RateLimits       atomic.Uint64
	Timeouts         atomic.Uint64
	ErrorsClient     atomic.Uint64
	ErrorsServer     atomic.Uint64
	ErrorsNetwork    atomic.Uint64
	LatencySum       atomic.Int64
	LatencyCount     atomic.Uint64
	TTFBSum          atomic.Int64
	TTFBCount        atomic.Uint64
	OutputBytes      atomic.Uint64
	Incomplete       atomic.Uint64
	Cancellations    atomic.Uint64
	Concurrency      atomic.Int32
	MaxConcurrency   atomic.Int32
}

func NewMetrics() *Metrics {
	return &Metrics{}
}

func (m *Metrics) RecordRequest() {
	m.Requests.Add(1)
	current := m.Concurrency.Add(1)
	for {
		max := m.MaxConcurrency.Load()
		if current <= max {
			break
		}
		if m.MaxConcurrency.CompareAndSwap(max, current) {
			break
		}
	}
}

func (m *Metrics) RecordSuccess(latency time.Duration) {
	m.Successes.Add(1)
	m.LatencySum.Add(int64(latency))
	m.LatencyCount.Add(1)
	m.Concurrency.Add(-1)
}

func (m *Metrics) RecordFailure(latency time.Duration, statusCode int) {
	m.Failures.Add(1)
	m.LatencySum.Add(int64(latency))
	m.LatencyCount.Add(1)
	m.Concurrency.Add(-1)

	switch {
	case statusCode == 0:
		m.ErrorsNetwork.Add(1)
	case statusCode >= 400 && statusCode < 500:
		m.ErrorsClient.Add(1)
	case statusCode >= 500:
		m.ErrorsServer.Add(1)
	}
}

func (m *Metrics) RecordTransfer(success bool, total, ttfb time.Duration, outputBytes int64, statusCode int, canceled bool) {
	if success {
		m.Successes.Add(1)
	} else {
		m.Failures.Add(1)
		m.Incomplete.Add(1)
		if canceled {
			m.Cancellations.Add(1)
		} else {
			switch {
			case statusCode == 0:
				m.ErrorsNetwork.Add(1)
			case statusCode >= 400 && statusCode < 500:
				m.ErrorsClient.Add(1)
			case statusCode >= 500:
				m.ErrorsServer.Add(1)
			}
		}
	}
	m.LatencySum.Add(int64(total))
	m.LatencyCount.Add(1)
	if ttfb > 0 {
		m.TTFBSum.Add(int64(ttfb))
		m.TTFBCount.Add(1)
	}
	if outputBytes > 0 {
		m.OutputBytes.Add(uint64(outputBytes))
	}
	m.Concurrency.Add(-1)
}

func (m *Metrics) RecordFallback() {
	m.FallbackAttempts.Add(1)
}

func (m *Metrics) RecordRateLimit() {
	m.RateLimits.Add(1)
}

func (m *Metrics) RecordTimeout() {
	m.Timeouts.Add(1)
}

type MetricsSnapshot struct {
	Requests         uint64        `json:"requests"`
	Successes        uint64        `json:"successes"`
	Failures         uint64        `json:"failures"`
	FallbackAttempts uint64        `json:"fallback_attempts"`
	RateLimits       uint64        `json:"rate_limits"`
	Timeouts         uint64        `json:"timeouts"`
	ErrorsClient     uint64        `json:"errors_client"`
	ErrorsServer     uint64        `json:"errors_server"`
	ErrorsNetwork    uint64        `json:"errors_network"`
	Concurrency      int32         `json:"concurrency"`
	MaxConcurrency   int32         `json:"max_concurrency"`
	AverageLatency   time.Duration `json:"average_latency_ms"`
	AverageTTFB      time.Duration `json:"average_ttfb_ms"`
	OutputBytes      uint64        `json:"output_bytes"`
	Incomplete       uint64        `json:"incomplete"`
	Cancellations    uint64        `json:"cancellations"`
}

func (m *Metrics) Snapshot() MetricsSnapshot {
	count := m.LatencyCount.Load()
	var avgLatency time.Duration
	if count > 0 {
		avgLatency = time.Duration(m.LatencySum.Load() / int64(count))
	}
	ttfbCount := m.TTFBCount.Load()
	var avgTTFB time.Duration
	if ttfbCount > 0 {
		avgTTFB = time.Duration(m.TTFBSum.Load() / int64(ttfbCount))
	}

	return MetricsSnapshot{
		Requests:         m.Requests.Load(),
		Successes:        m.Successes.Load(),
		Failures:         m.Failures.Load(),
		FallbackAttempts: m.FallbackAttempts.Load(),
		RateLimits:       m.RateLimits.Load(),
		Timeouts:         m.Timeouts.Load(),
		ErrorsClient:     m.ErrorsClient.Load(),
		ErrorsServer:     m.ErrorsServer.Load(),
		ErrorsNetwork:    m.ErrorsNetwork.Load(),
		Concurrency:      m.Concurrency.Load(),
		MaxConcurrency:   m.MaxConcurrency.Load(),
		AverageLatency:   avgLatency / time.Millisecond,
		AverageTTFB:      avgTTFB / time.Millisecond,
		OutputBytes:      m.OutputBytes.Load(),
		Incomplete:       m.Incomplete.Load(),
		Cancellations:    m.Cancellations.Load(),
	}
}

func (m *Metrics) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(m.Snapshot())
	}
}
