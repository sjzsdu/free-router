package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/sjzsdu/free-router/internal/catalog"
)

const (
	healthProbeConcurrency   = 8
	healthProbeTimeout       = 10 * time.Second
	expensiveProbeTimeout    = 2 * time.Minute
	expensiveProbeDailyLimit = 3
)

type ProbeStatus struct {
	Status                   string    `json:"status"`
	Total                    int       `json:"total"`
	Completed                int       `json:"completed"`
	Healthy                  int       `json:"healthy"`
	Failed                   int       `json:"failed"`
	Skipped                  int       `json:"skipped"`
	StartedAt                time.Time `json:"started_at,omitempty"`
	FinishedAt               time.Time `json:"finished_at,omitempty"`
	DryRun                   bool      `json:"dry_run,omitempty"`
	ExpensiveBudgetRemaining int       `json:"expensive_budget_remaining"`
}

type probeManager struct {
	mu         sync.RWMutex
	status     ProbeStatus
	budgetPath string
	auditPath  string
}

type probeBudget struct {
	Date string `json:"date"`
	Used int    `json:"used"`
}

type probeJob struct {
	Model      catalog.Model
	Capability string
}

func newProbeManager(dataDir string) *probeManager {
	return &probeManager{
		status:     ProbeStatus{Status: "idle", ExpensiveBudgetRemaining: expensiveProbeDailyLimit},
		budgetPath: filepath.Join(dataDir, "probe-budget.json"),
		auditPath:  filepath.Join(dataDir, "probe-audit.jsonl"),
	}
}

func (manager *probeManager) Snapshot() ProbeStatus {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.status
}

func (h *Handler) startHealthProbe(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Force  bool      `json:"force"`
		Models *[]string `json:"models"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&input); err != nil {
			writeError(w, http.StatusBadRequest, "invalid health probe request")
			return
		}
	}
	var status ProbeStatus
	var started bool
	if input.Models == nil {
		status, started = h.probes.Start(h, input.Force)
	} else {
		status, started = h.probes.StartModels(h, *input.Models, input.Force)
	}
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
		DryRun         bool   `json:"dry_run"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&input); err != nil || input.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	status, started, err := h.probes.StartModel(h, input.Model, input.AllowExpensive, input.DryRun)
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
	return manager.start(h, force, nil)
}

func (manager *probeManager) StartModels(h *Handler, modelIDs []string, force bool) (ProbeStatus, bool) {
	selected := make(map[string]struct{}, len(modelIDs))
	for _, modelID := range modelIDs {
		if modelID != "" {
			selected[modelID] = struct{}{}
		}
	}
	return manager.start(h, force, selected)
}

func (manager *probeManager) start(h *Handler, force bool, selected map[string]struct{}) (ProbeStatus, bool) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.status.Status == "running" {
		return manager.status, false
	}
	jobs, skipped := probeCandidates(h, force, selected)
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

func (manager *probeManager) StartModel(h *Handler, modelID string, allowExpensive, dryRun bool) (ProbeStatus, bool, error) {
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
	jobs := unverifiedProbeJobs(h, model)
	if hasExpensiveProbe(jobs) && !allowExpensive {
		return manager.status, false, errors.New("image and video probes require explicit cost confirmation")
	}
	expensive := countExpensiveProbes(jobs)
	remaining, err := manager.expensiveBudgetRemaining()
	if err != nil {
		return manager.status, false, err
	}
	if dryRun {
		now := time.Now()
		manager.status = ProbeStatus{Status: "completed", Total: len(jobs), Skipped: len(model.Functions) - len(jobs), StartedAt: now, FinishedAt: now, DryRun: true, ExpensiveBudgetRemaining: remaining}
		manager.audit(modelID, expensive, "dry-run")
		return manager.status, false, nil
	}
	if expensive > 0 {
		remaining, err = manager.reserveExpensiveBudget(expensive)
		if err != nil {
			manager.audit(modelID, expensive, "budget-denied")
			return manager.status, false, err
		}
		manager.audit(modelID, expensive, "started")
	}
	if len(jobs) == 0 {
		now := time.Now()
		manager.status = ProbeStatus{Status: "completed", Skipped: len(model.Functions), StartedAt: now, FinishedAt: now}
		return manager.status, false, nil
	}
	manager.status = ProbeStatus{Status: "running", Total: len(jobs), StartedAt: time.Now(), ExpensiveBudgetRemaining: remaining}
	go manager.run(h, jobs)
	return manager.status, true, nil
}

func countExpensiveProbes(jobs []probeJob) int {
	count := 0
	for _, job := range jobs {
		if expensiveCapability(job.Capability) {
			count++
		}
	}
	return count
}

func (manager *probeManager) expensiveBudgetRemaining() (int, error) {
	budget, err := manager.loadBudget()
	if err != nil {
		return 0, err
	}
	return max(0, expensiveProbeDailyLimit-budget.Used), nil
}

func (manager *probeManager) reserveExpensiveBudget(count int) (int, error) {
	budget, err := manager.loadBudget()
	if err != nil {
		return 0, err
	}
	if budget.Used+count > expensiveProbeDailyLimit {
		return max(0, expensiveProbeDailyLimit-budget.Used), fmt.Errorf("daily expensive probe budget exceeded: used %d of %d", budget.Used, expensiveProbeDailyLimit)
	}
	budget.Used += count
	if err := manager.saveBudget(budget); err != nil {
		return 0, err
	}
	return expensiveProbeDailyLimit - budget.Used, nil
}

func (manager *probeManager) loadBudget() (probeBudget, error) {
	today := time.Now().Format("2006-01-02")
	budget := probeBudget{Date: today}
	content, err := os.ReadFile(manager.budgetPath)
	if errors.Is(err, os.ErrNotExist) {
		return budget, nil
	}
	if err != nil {
		return budget, fmt.Errorf("read probe budget: %w", err)
	}
	if err := json.Unmarshal(content, &budget); err != nil {
		return budget, fmt.Errorf("decode probe budget: %w", err)
	}
	if budget.Date != today {
		budget = probeBudget{Date: today}
	}
	return budget, nil
}

func (manager *probeManager) saveBudget(budget probeBudget) error {
	if err := os.MkdirAll(filepath.Dir(manager.budgetPath), 0o700); err != nil {
		return fmt.Errorf("create probe budget directory: %w", err)
	}
	content, err := json.Marshal(budget)
	if err != nil {
		return err
	}
	temporary := manager.budgetPath + ".tmp"
	if err := os.WriteFile(temporary, content, 0o600); err != nil {
		return fmt.Errorf("write probe budget: %w", err)
	}
	if err := os.Rename(temporary, manager.budgetPath); err != nil {
		return fmt.Errorf("commit probe budget: %w", err)
	}
	return nil
}

func (manager *probeManager) audit(model string, expensive int, action string) {
	if err := os.MkdirAll(filepath.Dir(manager.auditPath), 0o700); err != nil {
		return
	}
	file, err := os.OpenFile(manager.auditPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_ = json.NewEncoder(file).Encode(map[string]any{"at": time.Now().UTC(), "model": model, "expensive_probes": expensive, "action": action})
}

func probeCandidates(h *Handler, force bool, selected map[string]struct{}) ([]probeJob, int) {
	jobs := make([]probeJob, 0)
	skipped := 0
	for _, model := range h.catalog.Models() {
		if selected != nil {
			if _, ok := selected[model.ID]; !ok {
				continue
			}
		}
		if !h.catalog.ProviderConfigured(model.Provider) {
			skipped++
			continue
		}
		var enabled bool
		model, enabled = h.routes.Apply(model)
		if !enabled || len(model.Functions) == 0 {
			skipped++
			continue
		}
		for _, job := range modelProbeJobs(model, false) {
			capability := job.Capability
			if expensiveCapability(capability) {
				skipped++
				continue
			}
			if !force && h.catalog.ModelCapabilityVerified(model, capability) {
				skipped++
				continue
			}
			jobs = append(jobs, job)
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

func providerProbeCandidates(h *Handler, providerID string) ([]probeJob, int) {
	jobs := make([]probeJob, 0)
	skipped := 0
	for _, model := range h.catalog.Models() {
		if model.Provider != providerID {
			continue
		}
		var enabled bool
		model, enabled = h.routes.Apply(model)
		if !enabled || len(model.Functions) == 0 {
			skipped++
			continue
		}
		for _, job := range modelProbeJobs(model, true) {
			capability := job.Capability
			if expensiveCapability(capability) {
				skipped++
				continue
			}
			if h.catalog.ModelCapabilityVerified(model, capability) {
				skipped++
				continue
			}
			jobs = append(jobs, job)
		}
	}
	return jobs, skipped
}

func modelProbeJobs(model catalog.Model, _ bool) []probeJob {
	if len(model.Functions) == 0 {
		return nil
	}
	jobs := make([]probeJob, 0, len(model.Functions))
	for _, capability := range model.Functions {
		jobs = append(jobs, probeJob{Model: model, Capability: capability})
	}
	return jobs
}

func unverifiedProbeJobs(h *Handler, model catalog.Model) []probeJob {
	jobs := make([]probeJob, 0, len(model.Functions))
	for _, job := range modelProbeJobs(model, true) {
		if !h.catalog.ModelCapabilityVerified(model, job.Capability) {
			jobs = append(jobs, job)
		}
	}
	return jobs
}

func (h *Handler) startProviderModelProbe(providerID string) bool {
	jobs, skipped := providerProbeCandidates(h, providerID)
	return h.probes.StartJobs(h, jobs, skipped)
}

func (manager *probeManager) StartJobs(h *Handler, jobs []probeJob, skipped int) bool {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.status.Status == "running" {
		return false
	}
	now := time.Now()
	manager.status = ProbeStatus{Status: "running", Total: len(jobs), Skipped: skipped, StartedAt: now}
	if len(jobs) == 0 {
		manager.status.Status = "completed"
		manager.status.FinishedAt = now
		return false
	}
	go manager.run(h, jobs)
	return true
}

func (h *Handler) markProviderModelsFailed(providerID string, status int, message string, latency time.Duration) {
	for _, model := range h.catalog.Models() {
		if model.Provider != providerID {
			continue
		}
		for _, capability := range model.Functions {
			h.health.ProbeFailure(model.ID, capability, latency, status, message)
		}
	}
}

func (h *Handler) resetProviderModelHealth(providerID string) {
	for _, model := range h.catalog.Models() {
		if model.Provider == providerID {
			h.health.Reset(model.ID, "")
		}
	}
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
					err = h.catalog.RecordModelCapabilityVerification(job.Model, job.Capability, time.Now(), latency)
					if err == nil {
						h.health.ProbeSuccess(job.Model.ID, job.Capability, latency)
						manager.record(true)
						continue
					}
				}
				status := result.Status
				var probeError *catalog.ModelProbeError
				if errors.As(err, &probeError) {
					status = probeError.Status
				}
				_ = h.catalog.ResetCapabilityVerification(job.Model.ID, job.Capability)
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
