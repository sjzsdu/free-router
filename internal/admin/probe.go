package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/sjzsdu/free-router/internal/catalog"
)

const (
	healthProbeTTL         = 24 * time.Hour
	healthProbeConcurrency = 3
	healthProbeTimeout     = 10 * time.Second
	expensiveProbeTimeout  = 2 * time.Minute
)

type ProbeStatus struct {
	Status     string    `json:"status"`
	Total      int       `json:"total"`
	Completed  int       `json:"completed"`
	Healthy    int       `json:"healthy"`
	Failed     int       `json:"failed"`
	Skipped    int       `json:"skipped"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

type probeManager struct {
	mu     sync.RWMutex
	status ProbeStatus
}

func newProbeManager() *probeManager {
	return &probeManager{status: ProbeStatus{Status: "idle"}}
}

func (manager *probeManager) Snapshot() ProbeStatus {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.status
}

func (h *Handler) startHealthProbe(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Force bool `json:"force"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&input); err != nil {
			writeError(w, http.StatusBadRequest, "invalid health probe request")
			return
		}
	}
	status, started := h.probes.Start(h, input.Force)
	code := http.StatusOK
	if started {
		code = http.StatusAccepted
	}
	writeJSON(w, code, status)
}

func (h *Handler) startModelHealthProbe(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Model          string `json:"model"`
		AllowExpensive bool   `json:"allow_expensive"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&input); err != nil || input.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	status, started, err := h.probes.StartModel(h, input.Model, input.AllowExpensive)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	code := http.StatusOK
	if started {
		code = http.StatusAccepted
	}
	writeJSON(w, code, status)
}

func (manager *probeManager) Start(h *Handler, force bool) (ProbeStatus, bool) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.status.Status == "running" {
		return manager.status, false
	}
	models, skipped := probeCandidates(h, force)
	if len(models) == 0 && !force && manager.status.Status == "completed" {
		return manager.status, false
	}
	now := time.Now()
	manager.status = ProbeStatus{Status: "running", Total: len(models), Skipped: skipped, StartedAt: now}
	if len(models) == 0 {
		manager.status.Status = "completed"
		manager.status.FinishedAt = now
		return manager.status, false
	}
	go manager.run(h, models)
	return manager.status, true
}

func (manager *probeManager) StartModel(h *Handler, modelID string, allowExpensive bool) (ProbeStatus, bool, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.status.Status == "running" {
		return manager.status, false, nil
	}
	model, ok := h.catalog.Find(modelID)
	if !ok {
		return manager.status, false, errors.New("model is not in the catalog")
	}
	var enabled bool
	model, enabled = h.routes.Apply(model)
	if !enabled {
		return manager.status, false, errors.New("disabled model cannot be probed")
	}
	if (model.Type == "image" || model.Type == "video") && !allowExpensive {
		return manager.status, false, errors.New("image and video probes require explicit cost confirmation")
	}
	if model.Type != "normal" && model.Type != "embedding" && model.Type != "rerank" && model.Type != "audio" && model.Type != "image" && model.Type != "video" {
		return manager.status, false, errors.New("this model type does not have a safe probe")
	}
	manager.status = ProbeStatus{Status: "running", Total: 1, StartedAt: time.Now()}
	go manager.run(h, []catalog.Model{model})
	return manager.status, true, nil
}

func probeCandidates(h *Handler, force bool) ([]catalog.Model, int) {
	models := make([]catalog.Model, 0)
	skipped := 0
	for _, model := range h.catalog.Models() {
		var enabled bool
		model, enabled = h.routes.Apply(model)
		if !enabled || (model.Type != "normal" && model.Type != "embedding" && model.Type != "rerank" && model.Type != "audio" && model.Type != "image" && model.Type != "video") {
			skipped++
			continue
		}
		if !force && !h.health.ProbeDue(model.ID, healthProbeTTL) {
			skipped++
			continue
		}
		models = append(models, model)
	}
	sort.SliceStable(models, func(i, j int) bool {
		if models[i].Provider == models[j].Provider {
			return models[i].ID < models[j].ID
		}
		return models[i].Provider < models[j].Provider
	})
	return interleaveProviders(models), skipped
}

func interleaveProviders(models []catalog.Model) []catalog.Model {
	groups := make(map[string][]catalog.Model)
	providers := make([]string, 0)
	for _, model := range models {
		if _, ok := groups[model.Provider]; !ok {
			providers = append(providers, model.Provider)
		}
		groups[model.Provider] = append(groups[model.Provider], model)
	}
	sort.Strings(providers)
	result := make([]catalog.Model, 0, len(models))
	for round := 0; len(result) < len(models); round++ {
		for _, providerID := range providers {
			if round < len(groups[providerID]) {
				result = append(result, groups[providerID][round])
			}
		}
	}
	return result
}

func (manager *probeManager) run(h *Handler, models []catalog.Model) {
	jobs := make(chan catalog.Model)
	var workers sync.WaitGroup
	providerLocks := make(map[string]chan struct{})
	for _, model := range models {
		if providerLocks[model.Provider] == nil {
			providerLocks[model.Provider] = make(chan struct{}, 1)
		}
	}
	for range min(healthProbeConcurrency, len(models)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for model := range jobs {
				lock := providerLocks[model.Provider]
				lock <- struct{}{}
				started := time.Now()
				timeout := healthProbeTimeout
				if model.Type == "image" || model.Type == "video" {
					timeout = expensiveProbeTimeout
				}
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				result, err := h.catalog.ProbeModel(ctx, model.ID)
				cancel()
				<-lock
				latency := time.Since(started)
				if err == nil {
					h.health.ProbeSuccess(model.ID, latency)
					manager.record(true)
					continue
				}
				status := result.Status
				var probeError *catalog.ModelProbeError
				if errors.As(err, &probeError) {
					status = probeError.Status
				}
				h.health.ProbeFailure(model.ID, latency, status, err.Error())
				manager.record(false)
			}
		}()
	}
	for _, model := range models {
		jobs <- model
	}
	close(jobs)
	workers.Wait()
	manager.mu.Lock()
	manager.status.Status = "completed"
	manager.status.FinishedAt = time.Now()
	manager.mu.Unlock()
}

func (manager *probeManager) record(healthy bool) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.status.Completed++
	if healthy {
		manager.status.Healthy++
	} else {
		manager.status.Failed++
	}
}
