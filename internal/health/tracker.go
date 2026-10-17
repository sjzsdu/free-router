package health

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	StatusUnknown  = "unknown"
	StatusHealthy  = "healthy"
	StatusDegraded = "degraded"
	StatusOpen     = "open"
	StatusHalfOpen = "half-open"
	StatusCooling  = "cooling"

	DefaultFailureThreshold = 5
	DefaultCoolDownBase     = time.Second
	DefaultCoolDownMax      = 30 * time.Second

	// Credential and quota failures (401/402/403) are not fixed by retrying:
	// the key was revoked, the account was suspended or the free allowance is
	// spent. Re-probing them on the transient-failure curve (30s) means every
	// such provider keeps taking a share of each request's attempt budget
	// forever while it can never answer. A long, still self-healing quarantine
	// keeps the pool's fallback depth for providers that can actually serve,
	// and an operator who fixes the key can lift it immediately from the admin
	// UI (health reset) or by saving the provider config.
	DefaultCredentialCoolDownBase = 2 * time.Minute
	DefaultCredentialCoolDownMax  = 30 * time.Minute
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
	Verified            bool      `json:"verified"`
	LastCheckedAt       time.Time `json:"last_checked_at,omitempty"`
	LastCheckLatencyMS  float64   `json:"last_check_latency_ms,omitempty"`
	FailureThreshold    int       `json:"failure_threshold"`
	CooldownUntil       time.Time `json:"cooldown_until,omitempty"`
	LastFailureAt       time.Time `json:"last_failure_at,omitempty"`
	InFlight            int32     `json:"in_flight"`
}

type Summary struct {
	Requests  uint64 `json:"requests"`
	Successes uint64 `json:"successes"`
	Failures  uint64 `json:"failures"`
	Failed    int    `json:"failed"`
}

type Tracker struct {
	mu                     sync.RWMutex
	states                 map[string]*State
	providerStates         map[string]*State
	now                    func() time.Time
	failureThreshold       int
	cooldownBase           time.Duration
	cooldownMax            time.Duration
	credentialCooldownBase time.Duration
	credentialCooldownMax  time.Duration
	path                   string

	// Persistence is deliberately kept off the state lock. Serialising the
	// whole state and writing it to disk while holding mu blocked every
	// concurrent Available/TryAcquire/Healthy call — including the ones in
	// flight for healthy providers — for the duration of a filesystem write,
	// which is exactly when a free pool is having its worst moment (a 429
	// storm produces one write per failed attempt).
	//
	// seq is advanced under mu for every snapshot; persistMu serialises the
	// writers and persistSeq records the newest snapshot already on disk so a
	// slower, older snapshot can never overwrite a newer one.
	// A monotonic snapshot sequence, advanced under the read lock by
	// snapshotLocked; see persist for how it prevents stale writes.
	seq        atomic.Uint64
	persistMu  sync.Mutex
	persistSeq uint64
}

type persistedState struct {
	SchemaVersion int     `json:"schema_version"`
	Models        []State `json:"models"`
	Providers     []State `json:"providers,omitempty"`
}

func New() *Tracker {
	return &Tracker{
		states:                 make(map[string]*State),
		providerStates:         make(map[string]*State),
		now:                    time.Now,
		failureThreshold:       DefaultFailureThreshold,
		cooldownBase:           DefaultCoolDownBase,
		cooldownMax:            DefaultCoolDownMax,
		credentialCooldownBase: DefaultCredentialCoolDownBase,
		credentialCooldownMax:  DefaultCredentialCoolDownMax,
	}
}

// NewPersistent loads a tracker whose validation and runtime health state is
// atomically saved after every meaningful state change.
func NewPersistent(path string) (*Tracker, error) {
	tracker := New()
	tracker.path = path
	if err := tracker.load(); err != nil {
		return nil, err
	}
	return tracker, nil
}

func (t *Tracker) Available(model, capability string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()

	provider := providerFromModel(model)
	if provider != "" {
		if pState := t.providerStates[provider]; pState != nil {
			if !t.isStateAvailable(pState.Status, pState.CooldownUntil) {
				return false
			}
			if pState.Status == StatusHalfOpen && atomic.LoadInt32(&pState.InFlight) > 0 {
				return false
			}
		}
	}

	state := t.states[stateKey(model, capability)]
	if state == nil {
		return true
	}
	if !t.isStateAvailable(state.Status, state.CooldownUntil) {
		return false
	}
	if state.Status == StatusHalfOpen && atomic.LoadInt32(&state.InFlight) > 0 {
		return false
	}
	return true
}

// Healthy is stricter than Available: it is used for route candidacy and only
// accepts capabilities with an explicit healthy state. Degraded, half-open,
// cooling, open, and unknown capabilities must be revalidated first.
func (t *Tracker) Healthy(model, capability string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()

	provider := providerFromModel(model)
	if provider != "" {
		if state := t.providerStates[provider]; state != nil && state.Status != StatusHealthy {
			return false
		}
	}
	state := t.states[stateKey(model, capability)]
	return state != nil && state.Verified && state.Status == StatusHealthy
}

func (t *Tracker) HasState(model, capability string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, ok := t.states[stateKey(model, capability)]
	return ok
}

func (t *Tracker) TryAcquire(model, capability string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	provider := providerFromModel(model)
	if provider != "" {
		if pState := t.providerStates[provider]; pState != nil {
			t.checkAndTransition(pState)
			if !t.isStateAvailable(pState.Status, pState.CooldownUntil) {
				return false
			}
			if pState.Status == StatusHalfOpen {
				if atomic.LoadInt32(&pState.InFlight) > 0 {
					return false
				}
				atomic.AddInt32(&pState.InFlight, 1)
			}
		}
	}

	state := t.states[stateKey(model, capability)]
	if state == nil {
		return true
	}
	t.checkAndTransition(state)
	if !t.isStateAvailable(state.Status, state.CooldownUntil) {
		return false
	}
	if state.Status == StatusHalfOpen {
		if atomic.LoadInt32(&state.InFlight) > 0 {
			return false
		}
		atomic.AddInt32(&state.InFlight, 1)
	}
	return true
}

// Release returns the in-flight slot taken by a successful TryAcquire. It is
// idempotent and safe to call from a deferred function on every exit path, so
// a half-open model can never wedge permanently.
func (t *Tracker) Release(model, capability string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := stateKey(model, capability)
	if state, ok := t.states[key]; ok {
		atomic.StoreInt32(&state.InFlight, 0)
	}
	if provider := providerFromModel(model); provider != "" {
		if pState, ok := t.providerStates[provider]; ok {
			atomic.StoreInt32(&pState.InFlight, 0)
		}
	}
}

func (t *Tracker) isStateAvailable(status string, cooldownUntil time.Time) bool {
	switch status {
	case StatusUnknown, StatusHealthy, StatusDegraded, StatusHalfOpen:
		return t.now().After(cooldownUntil)
	case StatusOpen, StatusCooling:
		return t.now().After(cooldownUntil)
	default:
		return false
	}
}

func (t *Tracker) checkAndTransition(state *State) {
	if t.now().After(state.CooldownUntil) {
		switch state.Status {
		case StatusOpen:
			state.Status = StatusHalfOpen
		case StatusCooling:
			state.Status = StatusHalfOpen
		}
	}
}

func (t *Tracker) Success(model, capability string, latency time.Duration, status int) {
	var persistRecovery bool
	t.mu.Lock()
	// persist runs after Unlock (deferred calls are LIFO), so the file write
	// never happens while the state lock is held.
	defer func() {
		if persistRecovery {
			t.persist()
		}
	}()
	defer t.mu.Unlock()

	existing := t.states[stateKey(model, capability)]
	persistRecovery = existing != nil && (existing.Status != StatusHealthy || existing.LastError != "")
	provider := providerFromModel(model)
	if provider != "" {
		pState := t.providerStates[provider]
		persistRecovery = persistRecovery || pState != nil && (pState.Status != StatusHealthy || pState.LastError != "")
	}

	state := t.state(model, capability)
	state.Requests++
	state.Successes++
	state.ConsecutiveFailures = 0
	state.LastStatus = status
	state.LastError = ""
	state.LastUsedAt = t.now()
	state.Status = StatusHealthy
	state.CooldownUntil = time.Time{}
	atomic.StoreInt32(&state.InFlight, 0)
	state.AverageLatencyMS = rollingAverage(state.AverageLatencyMS, state.Requests, float64(latency.Microseconds())/1000)

	if provider != "" {
		pState := t.providerState(provider)
		pState.ConsecutiveFailures = 0
		pState.Status = StatusHealthy
		pState.LastError = ""
		pState.CooldownUntil = time.Time{}
		atomic.StoreInt32(&pState.InFlight, 0)
	}
	// Failures are persisted eagerly, so a successful request only needs to
	// write when it clears a previously persisted model or provider failure;
	// the deferred persist above performs that write.
}

func (t *Tracker) Failure(model, capability string, latency time.Duration, status int, message string, retryAfter time.Duration) {
	t.mu.Lock()
	// Deferred calls run LIFO: Unlock first, then the disk write.
	defer t.persist()
	defer t.mu.Unlock()

	errType := classifyError(status)
	if errType == ErrorClient {
		state := t.state(model, capability)
		state.Requests++
		state.LastStatus = status
		state.LastError = message
		state.LastUsedAt = t.now()
		state.AverageLatencyMS = rollingAverage(state.AverageLatencyMS, state.Requests, float64(latency.Microseconds())/1000)
		if state.Status == StatusUnknown {
			state.Status = StatusHealthy
		}
		// A client error still completes the attempt: release the in-flight
		// slot so a half-open model is not wedged by a single 4xx.
		atomic.StoreInt32(&state.InFlight, 0)
		return
	}

	if errType == ErrorAuth {
		provider := providerFromModel(model)
		if provider != "" {
			pState := t.providerState(provider)
			pState.Requests++
			pState.Failures++
			pState.ConsecutiveFailures++
			pState.LastStatus = status
			pState.LastError = message
			pState.LastUsedAt = t.now()
			pState.Status = StatusOpen
			pState.CooldownUntil = t.now().Add(t.authCooldownFor(status, pState.ConsecutiveFailures))
			atomic.StoreInt32(&pState.InFlight, 0)
		}
		// Record the credential failure on the model as well and always
		// release its in-flight slot: a model that was half-open when the
		// 401 arrived would otherwise stay permanently unwedgeable.
		state := t.state(model, capability)
		state.Requests++
		state.LastStatus = status
		state.LastError = message
		state.LastUsedAt = t.now()
		atomic.StoreInt32(&state.InFlight, 0)
		return
	}

	state := t.state(model, capability)
	state.Requests++
	state.Failures++
	state.ConsecutiveFailures++
	state.LastStatus = status
	state.LastError = message
	state.LastUsedAt = t.now()
	state.LastFailureAt = t.now()
	state.AverageLatencyMS = rollingAverage(state.AverageLatencyMS, state.Requests, float64(latency.Microseconds())/1000)

	if errType == ErrorRateLimit && retryAfter > 0 {
		state.Status = StatusCooling
		state.CooldownUntil = t.now().Add(retryAfter)
		atomic.StoreInt32(&state.InFlight, 0)
		return
	}

	if state.ConsecutiveFailures >= state.FailureThreshold {
		state.Status = StatusOpen
		state.CooldownUntil = t.now().Add(t.cooldownFor(state.ConsecutiveFailures))
	} else {
		state.Status = StatusDegraded
	}
	atomic.StoreInt32(&state.InFlight, 0)
}

func (t *Tracker) ProbeSuccess(model, capability string, latency time.Duration) {
	t.mu.Lock()
	// Deferred calls run LIFO: Unlock first, then the disk write.
	defer t.persist()
	defer t.mu.Unlock()

	state := t.state(model, capability)
	state.Checks++
	state.Verified = true
	state.LastCheckedAt = t.now()
	state.LastCheckLatencyMS = float64(latency.Microseconds()) / 1000
	state.LastStatus = 200
	state.LastError = ""
	state.ConsecutiveFailures = 0
	state.Status = StatusHealthy
	state.CooldownUntil = time.Time{}

	provider := providerFromModel(model)
	if provider != "" {
		pState := t.providerState(provider)
		pState.ConsecutiveFailures = 0
		pState.Status = StatusHealthy
		pState.CooldownUntil = time.Time{}
	}
}

// RestoreProbeSuccess hydrates a persisted capability verification without
// treating service startup as a new probe.
func (t *Tracker) RestoreProbeSuccess(model, capability string, checkedAt time.Time, latencyMS float64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	state := t.state(model, capability)
	state.Checks = 1
	state.Verified = true
	state.LastCheckedAt = checkedAt
	state.LastCheckLatencyMS = latencyMS
	state.LastStatus = 200
	state.LastError = ""
	state.ConsecutiveFailures = 0
	state.Status = StatusHealthy
	state.CooldownUntil = time.Time{}
}

func (t *Tracker) ProbeFailure(model, capability string, latency time.Duration, status int, message string) {
	t.mu.Lock()
	// Deferred calls run LIFO: Unlock first, then the disk write.
	defer t.persist()
	defer t.mu.Unlock()

	state := t.state(model, capability)
	state.Checks++
	state.Verified = false
	state.LastCheckedAt = t.now()
	state.LastCheckLatencyMS = float64(latency.Microseconds()) / 1000
	state.LastStatus = status
	state.LastError = message
	state.ConsecutiveFailures++
	state.LastFailureAt = t.now()

	errType := classifyError(status)
	if errType == ErrorClient {
		state.Status = StatusDegraded
		return
	}

	if errType == ErrorAuth {
		state.Status = StatusOpen
		state.CooldownUntil = t.now().Add(t.authCooldownFor(status, state.ConsecutiveFailures))
		return
	}

	if state.ConsecutiveFailures >= state.FailureThreshold {
		state.Status = StatusOpen
		state.CooldownUntil = t.now().Add(t.cooldownFor(state.ConsecutiveFailures))
	} else {
		state.Status = StatusDegraded
	}
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
		state := *state
		if t.now().After(state.CooldownUntil) && state.Status == StatusCooling {
			state.Status = StatusHalfOpen
		}
		if t.now().After(state.CooldownUntil) && state.Status == StatusOpen {
			state.Status = StatusHalfOpen
		}
		result = append(result, state)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Model == result[j].Model {
			return result[i].Capability < result[j].Capability
		}
		return result[i].Model < result[j].Model
	})
	return result
}

func (t *Tracker) ProviderSnapshot() []State {
	t.mu.RLock()
	defer t.mu.RUnlock()

	result := make([]State, 0, len(t.providerStates))
	for _, state := range t.providerStates {
		state := *state
		if t.now().After(state.CooldownUntil) && state.Status == StatusCooling {
			state.Status = StatusHalfOpen
		}
		if t.now().After(state.CooldownUntil) && state.Status == StatusOpen {
			state.Status = StatusHalfOpen
		}
		result = append(result, state)
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
		if state.Status == StatusOpen || state.Status == StatusCooling {
			summary.Failed++
		}
	}
	return summary
}

func (t *Tracker) Reset(model, capability string) {
	t.mu.Lock()
	// Deferred calls run LIFO: Unlock first, then the disk write.
	defer t.persist()
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
	provider := providerFromModel(model)
	if provider != "" {
		delete(t.providerStates, provider)
	}
}

func (t *Tracker) load() error {
	if t.path == "" {
		return nil
	}
	content, err := os.ReadFile(t.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read health state: %w", err)
	}
	var persisted persistedState
	if err := json.Unmarshal(content, &persisted); err != nil {
		return t.backupInvalidState(fmt.Errorf("decode health state: %w", err))
	}
	if persisted.SchemaVersion != 1 {
		return t.backupInvalidState(fmt.Errorf("unsupported health state schema version %d", persisted.SchemaVersion))
	}
	for _, saved := range persisted.Models {
		state := saved
		state.InFlight = 0
		if state.FailureThreshold <= 0 {
			state.FailureThreshold = t.failureThreshold
		}
		t.states[stateKey(state.Model, state.Capability)] = &state
	}
	for _, saved := range persisted.Providers {
		state := saved
		state.InFlight = 0
		if state.FailureThreshold <= 0 {
			state.FailureThreshold = t.failureThreshold
		}
		t.providerStates[state.Model] = &state
	}
	return nil
}

func (t *Tracker) backupInvalidState(cause error) error {
	backup := fmt.Sprintf("%s.corrupted.%s", t.path, t.now().UTC().Format("20060102T150405.000000000Z"))
	if err := os.Rename(t.path, backup); err != nil {
		return fmt.Errorf("backup invalid health state after %v: %w", cause, err)
	}
	slog.Error("health state is invalid; backed it up and starting fresh",
		"path", t.path,
		"backup", backup,
		"error", cause,
	)
	return nil
}

// snapshotLocked copies the persistable state and advances the snapshot
// sequence. The caller must hold mu (a read lock is enough). It is pure memory
// work, so it can run under the lock; encoding and the filesystem write happen
// in persist, outside it.
func (t *Tracker) snapshotLocked() (persistedState, uint64) {
	persisted := persistedState{SchemaVersion: 1}
	for _, state := range t.states {
		copy := *state
		copy.InFlight = 0
		persisted.Models = append(persisted.Models, copy)
	}
	for _, state := range t.providerStates {
		copy := *state
		copy.InFlight = 0
		persisted.Providers = append(persisted.Providers, copy)
	}
	sort.Slice(persisted.Models, func(i, j int) bool {
		if persisted.Models[i].Model == persisted.Models[j].Model {
			return persisted.Models[i].Capability < persisted.Models[j].Capability
		}
		return persisted.Models[i].Model < persisted.Models[j].Model
	})
	sort.Slice(persisted.Providers, func(i, j int) bool { return persisted.Providers[i].Model < persisted.Providers[j].Model })
	return persisted, t.seq.Add(1)
}

// persist writes the current state to disk *without* holding the state lock.
// Callers register it as a deferred call so every exit path saves exactly once.
//
// Two requests can reach this point concurrently. persistMu serialises the
// writers, and the snapshot sequence keeps the file monotonic: if a newer
// snapshot was already written, an older one is dropped instead of rolling the
// file back to stale state.
func (t *Tracker) persist() {
	if t.path == "" {
		return
	}
	t.mu.RLock()
	snapshot, seq := t.snapshotLocked()
	t.mu.RUnlock()

	t.persistMu.Lock()
	defer t.persistMu.Unlock()
	if seq <= t.persistSeq {
		return
	}
	t.persistSeq = seq
	t.writeSnapshot(snapshot)
}

func (t *Tracker) writeSnapshot(persisted persistedState) {
	content, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		slog.Error("encode health state", "path", t.path, "error", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(t.path), 0o700); err != nil {
		slog.Error("create health state directory", "path", t.path, "error", err)
		return
	}
	temporary, err := os.CreateTemp(filepath.Dir(t.path), ".health-*.json")
	if err != nil {
		slog.Error("create temporary health state", "path", t.path, "error", err)
		return
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err == nil {
		_, err = temporary.Write(content)
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temporaryPath, t.path)
	}
	if err != nil {
		slog.Error("persist health state", "path", t.path, "error", err)
	}
}

func (t *Tracker) state(model, capability string) *State {
	key := stateKey(model, capability)
	state := t.states[key]
	if state == nil {
		state = &State{
			Model:            model,
			Capability:       capability,
			Status:           StatusUnknown,
			FailureThreshold: t.failureThreshold,
		}
		t.states[key] = state
	}
	return state
}

func (t *Tracker) providerState(provider string) *State {
	state := t.providerStates[provider]
	if state == nil {
		state = &State{
			Model:            provider,
			Capability:       "provider",
			Status:           StatusUnknown,
			FailureThreshold: t.failureThreshold,
		}
		t.providerStates[provider] = state
	}
	return state
}

func (t *Tracker) cooldownFor(failures int) time.Duration {
	return cooldownWith(t.cooldownBase, t.cooldownMax, failures)
}

// credentialCooldownFor is the quarantine curve for failures that retrying
// cannot fix (revoked key, suspended account, exhausted quota). It is longer
// than the transient curve on purpose: while such a provider stays selectable,
// every request spends one of its few attempt slots on a provider that is
// guaranteed to fail, which shortens the effective fallback depth for the
// healthy ones.
func (t *Tracker) credentialCooldownFor(failures int) time.Duration {
	return cooldownWith(t.credentialCooldownBase, t.credentialCooldownMax, failures)
}

// authCooldownFor picks the quarantine curve for an auth failure. A 401
// (invalid key) or 402 (allowance spent) condemns the whole provider: retrying
// cannot fix it and every request that still selects it burns one of its few
// attempt slots, so it gets the long credential curve. A 403 is ambiguous — it
// covers a suspended account as well as a single model the key may not call —
// so it stays on the short transient curve to avoid freezing the provider's
// other models for minutes.
func (t *Tracker) authCooldownFor(status, failures int) time.Duration {
	if providerWideAuthFailure(status) {
		return t.credentialCooldownFor(failures)
	}
	return t.cooldownFor(failures)
}

func providerWideAuthFailure(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusPaymentRequired
}

func cooldownWith(base, max time.Duration, failures int) time.Duration {
	// Exponential backoff with saturation: double up to max without ever
	// overflowing int64 (a negative backoff would panic rand.Int63n below).
	// Values of failures <= 0 fall back to the base.
	backoff := base
	for i := 1; i < failures; i++ {
		if backoff >= max/2 {
			backoff = max
			break
		}
		backoff *= 2
	}
	if backoff > max {
		backoff = max
	}
	var jitter time.Duration
	if backoff >= 2 {
		jitter = time.Duration(rand.Int63n(int64(backoff / 2)))
	}
	return backoff + jitter
}

func stateKey(model, capability string) string { return model + "\x00" + capability }

func providerFromModel(model string) string {
	if idx := findSeparator(model); idx >= 0 {
		return model[:idx]
	}
	return ""
}

func findSeparator(s string) int {
	for i, c := range s {
		if c == '/' || c == ':' {
			return i
		}
	}
	return -1
}

type ErrorType int

const (
	ErrorClient ErrorType = iota
	ErrorAuth
	ErrorRateLimit
	ErrorServer
	ErrorNetwork
)

func classifyError(status int) ErrorType {
	switch {
	case status == 0:
		return ErrorNetwork
	case status >= 400 && status < 500:
		switch status {
		case 401, 402, 403:
			return ErrorAuth
		case 429:
			return ErrorRateLimit
		case 404, 410:
			return ErrorServer
		default:
			return ErrorClient
		}
	case status >= 500:
		return ErrorServer
	default:
		return ErrorServer
	}
}

func rollingAverage(current float64, count uint64, value float64) float64 {
	if count <= 1 {
		return value
	}
	return current + (value-current)/float64(count)
}
