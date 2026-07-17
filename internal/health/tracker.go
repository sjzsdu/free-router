package health

import (
	"sort"
	"sync"
	"time"
)

type State struct {
	Model               string    `json:"model"`
	Status              string    `json:"status"`
	Requests            uint64    `json:"requests"`
	Successes           uint64    `json:"successes"`
	Failures            uint64    `json:"failures"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	AverageLatencyMS    float64   `json:"average_latency_ms"`
	LastStatus          int       `json:"last_status,omitempty"`
	LastError           string    `json:"last_error,omitempty"`
	LastUsedAt          time.Time `json:"last_used_at,omitempty"`
	CooldownUntil       time.Time `json:"cooldown_until,omitempty"`
}

type Summary struct {
	Requests  uint64 `json:"requests"`
	Successes uint64 `json:"successes"`
	Failures  uint64 `json:"failures"`
	Cooling   int    `json:"cooling"`
}

type Tracker struct {
	mu     sync.RWMutex
	states map[string]*State
	now    func() time.Time
}

func New() *Tracker {
	return &Tracker{states: make(map[string]*State), now: time.Now}
}

func (t *Tracker) Available(model string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	state := t.states[model]
	return state == nil || state.CooldownUntil.IsZero() || !t.now().Before(state.CooldownUntil)
}

func (t *Tracker) Success(model string, latency time.Duration, status int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.state(model)
	state.Requests++
	state.Successes++
	state.ConsecutiveFailures = 0
	state.LastStatus = status
	state.LastError = ""
	state.LastUsedAt = t.now()
	state.CooldownUntil = time.Time{}
	state.Status = "healthy"
	state.AverageLatencyMS = rollingAverage(state.AverageLatencyMS, state.Requests, float64(latency.Microseconds())/1000)
}

func (t *Tracker) Failure(model string, latency time.Duration, status int, message string, retryAfter time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.state(model)
	state.Requests++
	state.Failures++
	state.ConsecutiveFailures++
	state.LastStatus = status
	state.LastError = message
	state.LastUsedAt = t.now()
	state.AverageLatencyMS = rollingAverage(state.AverageLatencyMS, state.Requests, float64(latency.Microseconds())/1000)

	cooldown := retryAfter
	switch {
	case cooldown > 0:
	case status == 401 || status == 403:
		cooldown = 5 * time.Minute
	case status == 429:
		cooldown = 30 * time.Second
	case state.ConsecutiveFailures >= 2:
		cooldown = 30 * time.Second
	}
	if cooldown > 0 {
		state.Status = "cooling"
		state.CooldownUntil = t.now().Add(cooldown)
	} else {
		state.Status = "degraded"
	}
}

func (t *Tracker) Snapshot() []State {
	t.mu.RLock()
	defer t.mu.RUnlock()
	now := t.now()
	result := make([]State, 0, len(t.states))
	for _, state := range t.states {
		copy := *state
		if copy.Status == "cooling" && !now.Before(copy.CooldownUntil) {
			copy.Status = "degraded"
			copy.CooldownUntil = time.Time{}
		}
		result = append(result, copy)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Model < result[j].Model })
	return result
}

func (t *Tracker) Summary() Summary {
	states := t.Snapshot()
	var summary Summary
	for _, state := range states {
		summary.Requests += state.Requests
		summary.Successes += state.Successes
		summary.Failures += state.Failures
		if state.Status == "cooling" {
			summary.Cooling++
		}
	}
	return summary
}

func (t *Tracker) Reset(model string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.states, model)
}

func (t *Tracker) state(model string) *State {
	state := t.states[model]
	if state == nil {
		state = &State{Model: model, Status: "unknown"}
		t.states[model] = state
	}
	return state
}

func rollingAverage(current float64, count uint64, value float64) float64 {
	if count <= 1 {
		return value
	}
	return current + (value-current)/float64(count)
}
