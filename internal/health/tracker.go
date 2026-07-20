package health

import (
	"sort"
	"sync"
	"time"
)

type State struct {
	Model               string    `json:"model"`
	Capability          string    `json:"capability"`
	Status              string    `json:"status"`
	Requests            uint64    `json:"requests"`
	Successes           uint64    `json:"successes"`
	Failures            uint64    `json:"failures"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	AverageLatencyMS    float64   `json:"average_latency_ms"`
	LastStatus          int       `json:"last_status,omitempty"`
	LastError           string    `json:"last_error,omitempty"`
	LastUsedAt          time.Time `json:"last_used_at,omitempty"`
	Checks              uint64    `json:"checks"`
	LastCheckedAt       time.Time `json:"last_checked_at,omitempty"`
	LastCheckLatencyMS  float64   `json:"last_check_latency_ms,omitempty"`
}

type Summary struct {
	Requests  uint64 `json:"requests"`
	Successes uint64 `json:"successes"`
	Failures  uint64 `json:"failures"`
	Failed    int    `json:"failed"`
}

type Tracker struct {
	mu     sync.RWMutex
	states map[string]*State
	now    func() time.Time
}

func New() *Tracker {
	return &Tracker{states: make(map[string]*State), now: time.Now}
}

func (t *Tracker) Available(model, capability string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	state := t.states[stateKey(model, capability)]
	return state == nil || state.Status == "unknown" || state.Status == "healthy"
}

func (t *Tracker) Success(model, capability string, latency time.Duration, status int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.state(model, capability)
	state.Requests++
	state.Successes++
	state.ConsecutiveFailures = 0
	state.LastStatus = status
	state.LastError = ""
	state.LastUsedAt = t.now()
	state.Status = "healthy"
	state.AverageLatencyMS = rollingAverage(state.AverageLatencyMS, state.Requests, float64(latency.Microseconds())/1000)
}

func (t *Tracker) Failure(model, capability string, latency time.Duration, status int, message string, _ time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.state(model, capability)
	state.Requests++
	state.Failures++
	state.ConsecutiveFailures++
	state.LastStatus = status
	state.LastError = message
	state.LastUsedAt = t.now()
	state.AverageLatencyMS = rollingAverage(state.AverageLatencyMS, state.Requests, float64(latency.Microseconds())/1000)

	state.Status = "failed"
}

func (t *Tracker) ProbeSuccess(model, capability string, latency time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.state(model, capability)
	state.Checks++
	state.LastCheckedAt = t.now()
	state.LastCheckLatencyMS = float64(latency.Microseconds()) / 1000
	state.LastStatus = 200
	state.LastError = ""
	state.ConsecutiveFailures = 0
	state.Status = "healthy"
}

func (t *Tracker) ProbeFailure(model, capability string, latency time.Duration, status int, message string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.state(model, capability)
	state.Checks++
	state.LastCheckedAt = t.now()
	state.LastCheckLatencyMS = float64(latency.Microseconds()) / 1000
	state.LastStatus = status
	state.LastError = message
	state.ConsecutiveFailures++
	state.Status = "failed"
}

func (t *Tracker) ProbeDue(model, capability string, ttl time.Duration) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	state := t.states[stateKey(model, capability)]
	return state == nil || state.LastCheckedAt.IsZero() || t.now().Sub(state.LastCheckedAt) >= ttl
}

func (t *Tracker) Snapshot() []State {
	t.mu.RLock()
	defer t.mu.RUnlock()
	result := make([]State, 0, len(t.states))
	for _, state := range t.states {
		copy := *state
		result = append(result, copy)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Model == result[j].Model {
			return result[i].Capability < result[j].Capability
		}
		return result[i].Model < result[j].Model
	})
	return result
}

func (t *Tracker) Summary() Summary {
	states := t.Snapshot()
	var summary Summary
	for _, state := range states {
		summary.Requests += state.Requests
		summary.Successes += state.Successes
		summary.Failures += state.Failures
		if state.Status == "failed" {
			summary.Failed++
		}
	}
	return summary
}

func (t *Tracker) Reset(model, capability string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if capability != "" {
		delete(t.states, stateKey(model, capability))
		return
	}
	for key, state := range t.states {
		if state.Model == model {
			delete(t.states, key)
		}
	}
}

func (t *Tracker) state(model, capability string) *State {
	key := stateKey(model, capability)
	state := t.states[key]
	if state == nil {
		state = &State{Model: model, Capability: capability, Status: "unknown"}
		t.states[key] = state
	}
	return state
}

func stateKey(model, capability string) string { return model + "\x00" + capability }

func rollingAverage(current float64, count uint64, value float64) float64 {
	if count <= 1 {
		return value
	}
	return current + (value-current)/float64(count)
}
