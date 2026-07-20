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

type probeJob struct {
	Model      catalog.Model
	Capability string
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
	jobs, skipped := probeCandidates(h, force)
	if len(jobs) == 0 && !force && manager.status.Status == "completed" {
		return manager.status, false
	}
	now := time.Now()
	manager.status = ProbeStatus{Status: "running", Total: len(jobs), Skipped: skipped, StartedAt: now}
	if len(jobs) == 0 {
		manager.status.Status = "completed"
		manager.status.FinishedAt = now
		return manager.status, false
	}
	go manager.run(h, jobs)
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
	jobs := make([]probeJob, 0, len(model.Functions))
	for _, capability := range model.Functions {
		jobs = append(jobs, probeJob{Model: model, Capability: capability})
	}
	if hasExpensiveProbe(jobs) && !allowExpensive {
		return manager.status, false, errors.New("image and video probes require explicit cost confirmation")
	}
	if len(jobs) == 0 {
		return manager.status, false, errors.New("this model does not advertise a probeable capability")
	}
	manager.status = ProbeStatus{Status: "running", Total: len(jobs), StartedAt: time.Now()}
	go manager.run(h, jobs)
	return manager.status, true, nil
}

func probeCandidates(h *Handler, force bool) ([]probeJob, int) {
	jobs := make([]probeJob, 0)
	skipped := 0
	for _, model := range h.catalog.Models() {
		var enabled bool
		model, enabled = h.routes.Apply(model)
		if !enabled || len(model.Functions) == 0 {
			skipped++
			continue
		}
		for _, capability := range model.Functions {
			if !force && !h.health.ProbeDue(model.ID, capability, healthProbeTTL) {
				skipped++
				continue
			}
			jobs = append(jobs, probeJob{Model: model, Capability: capability})
		}
	}
	sort.SliceStable(jobs, func(i, j int) bool {
		if jobs[i].Model.Provider == jobs[j].Model.Provider {
			if jobs[i].Model.ID == jobs[j].Model.ID {
				return jobs[i].Capability < jobs[j].Capability
			}
			return jobs[i].Model.ID < jobs[j].Model.ID
		}
		return jobs[i].Model.Provider < jobs[j].Model.Provider
	})
	return interleaveProviders(jobs), skipped
}

func interleaveProviders(jobs []probeJob) []probeJob {
	groups := make(map[string][]probeJob)
	providers := make([]string, 0)
	for _, job := range jobs {
		if _, ok := groups[job.Model.Provider]; !ok {
			providers = append(providers, job.Model.Provider)
		}
		groups[job.Model.Provider] = append(groups[job.Model.Provider], job)
	}
	sort.Strings(providers)
	result := make([]probeJob, 0, len(jobs))
	for round := 0; len(result) < len(jobs); round++ {
		for _, providerID := range providers {
			if round < len(groups[providerID]) {
				result = append(result, groups[providerID][round])
			}
		}
	}
	return result
}

func expensiveCapability(capability string) bool {
	return capability == catalog.FunctionImageGeneration || capability == catalog.FunctionVideoGeneration
}

func hasExpensiveProbe(jobs []probeJob) bool {
	for _, job := range jobs {
		if expensiveCapability(job.Capability) {
			return true
		}
	}
	return false
}

func (manager *probeManager) run(h *Handler, models []probeJob) {
	jobs := make(chan probeJob)
	var workers sync.WaitGroup
	providerLocks := make(map[string]chan struct{})
	for _, job := range models {
		if providerLocks[job.Model.Provider] == nil {
			providerLocks[job.Model.Provider] = make(chan struct{}, 1)
		}
	}
	for range min(healthProbeConcurrency, len(models)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for job := range jobs {
				lock := providerLocks[job.Model.Provider]
				lock <- struct{}{}
				started := time.Now()
				timeout := healthProbeTimeout
				if expensiveCapability(job.Capability) {
					timeout = expensiveProbeTimeout
				}
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				result, err := h.catalog.ProbeModel(ctx, job.Model.ID, job.Capability)
				cancel()
				<-lock
				latency := time.Since(started)
				if err == nil {
					h.health.ProbeSuccess(job.Model.ID, job.Capability, latency)
					manager.record(true)
					continue
				}
				status := result.Status
				var probeError *catalog.ModelProbeError
				if errors.As(err, &probeError) {
					status = probeError.Status
				}
				h.health.ProbeFailure(job.Model.ID, job.Capability, latency, status, err.Error())
				manager.record(false)
			}
		}()
	}
	for _, job := range models {
		jobs <- job
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
