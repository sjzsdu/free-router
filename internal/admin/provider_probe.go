package admin

import (
	"sync"
	"time"
)

type providerProbeState struct {
	Status        string    `json:"status"`
	FormulaModels int       `json:"formula_models"`
	LatencyMS     int64     `json:"latency_ms"`
	Error         string    `json:"error,omitempty"`
	CheckedAt     time.Time `json:"checked_at"`
}

type providerProbeStore struct {
	mu     sync.RWMutex
	states map[string]providerProbeState
}

func newProviderProbeStore() *providerProbeStore {
	return &providerProbeStore{states: make(map[string]providerProbeState)}
}

func (store *providerProbeStore) success(providerID string, models int, latency time.Duration) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.states[providerID] = providerProbeState{
		Status: "healthy", FormulaModels: models, LatencyMS: latency.Milliseconds(), CheckedAt: time.Now(),
	}
}

func (store *providerProbeStore) failure(providerID string, _ int, message string, latency time.Duration) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.states[providerID] = providerProbeState{
		Status: "error", LatencyMS: latency.Milliseconds(), Error: message, CheckedAt: time.Now(),
	}
}

func (store *providerProbeStore) remove(providerID string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	delete(store.states, providerID)
}

func (store *providerProbeStore) decorate(providers []map[string]any) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	for _, item := range providers {
		providerID, _ := item["id"].(string)
		if state, ok := store.states[providerID]; ok {
			item["connection_status"] = state.Status
			item["connection_formula_models"] = state.FormulaModels
			item["connection_latency_ms"] = state.LatencyMS
			item["connection_error"] = state.Error
			item["connection_checked_at"] = state.CheckedAt
		}
	}
}
