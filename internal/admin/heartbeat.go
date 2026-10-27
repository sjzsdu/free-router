package admin

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sjzsdu/free-router/internal/health"
	"github.com/sjzsdu/free-router/internal/routing"
)

// The heartbeat is the manual "probe all" action on a timer: it reuses
// probeManager (selection, provider-serialised execution, verification and
// health recording) and only decides *which* capability probes are due.
//
// Rate protection is deliberately conservative, because every probe is a real
// upstream inference request against a free tier:
//   - at most MaxProbesPerRound probes per round,
//   - a gap of at least half the smallest configured interval between rounds,
//   - Low concurrency, one request per provider at a time,
//   - exponential per-target backoff after a failed probe,
//   - healthy models are only re-checked on the slow interval, if at all.
const (
	heartbeatTickInterval   = 15 * time.Second
	heartbeatStartupGrace   = 5 * time.Second
	heartbeatMinRoundGap    = time.Minute
	heartbeatMaxBackoffRate = 6
)

// Environment overrides win over the persisted configuration so an operator
// can disable keep-alive traffic without rewriting the config file.
const (
	heartbeatEnvEnabled     = "FREE_ROUTER_HEARTBEAT_ENABLED"
	heartbeatEnvInterval    = "FREE_ROUTER_HEARTBEAT_INTERVAL_MINUTES"
	heartbeatEnvRecovery    = "FREE_ROUTER_HEARTBEAT_RECOVERY_MINUTES"
	heartbeatEnvMaxProbes   = "FREE_ROUTER_HEARTBEAT_MAX_PROBES_PER_ROUND"
	heartbeatEnvConcurrency = "FREE_ROUTER_HEARTBEAT_CONCURRENCY"
)

// HeartbeatStatus is the observable state of the keep-alive loop. It is
// additive to the admin state response so an operator can tell whether the
// heartbeat is running, what it just probed and why anything was deferred.
type HeartbeatStatus struct {
	Enabled             bool      `json:"enabled"`
	IntervalMinutes     int       `json:"interval_minutes"`
	RecoveryMinutes     int       `json:"recovery_minutes"`
	MaxProbesPerRound   int       `json:"max_probes_per_round"`
	Concurrency         int       `json:"concurrency"`
	Rounds              uint64    `json:"rounds"`
	LastRoundStartedAt  time.Time `json:"last_round_started_at,omitempty"`
	LastRoundFinishedAt time.Time `json:"last_round_finished_at,omitempty"`
	LastRoundProbes     int       `json:"last_round_probes"`
	LastRoundHealthy    int       `json:"last_round_healthy"`
	LastRoundFailed     int       `json:"last_round_failed"`
	LastRoundSkipped    int       `json:"last_round_skipped"`
	// PendingRecovery counts quarantined or cooling capabilities that waited
	// for their retry window in the last round; BackoffTargets counts targets
	// currently sitting out an exponential backoff.
	PendingRecovery int       `json:"pending_recovery"`
	BackoffTargets  int       `json:"backoff_targets"`
	NextRoundAt     time.Time `json:"next_round_at,omitempty"`
}

type heartbeatTarget struct {
	failures int
	retryAt  time.Time
}

type heartbeatManager struct {
	probes *probeManager

	mu      sync.Mutex
	targets map[string]*heartbeatTarget
	status  HeartbeatStatus

	now  func() time.Time
	tick time.Duration
	// grace delays the first round so startup probing does not compete with
	// the catalog bootstrap; gap overrides the round gap. Tests shorten both.
	grace time.Duration
	gap   func(settings routing.HeartbeatSettings) time.Duration
}

func newHeartbeatManager(probes *probeManager) *heartbeatManager {
	return &heartbeatManager{
		probes:  probes,
		targets: make(map[string]*heartbeatTarget),
		now:     time.Now,
		tick:    heartbeatTickInterval,
		grace:   heartbeatStartupGrace,
		gap:     heartbeatRoundGap,
	}
}

// StartHeartbeat launches the keep-alive loop. It stops with ctx, so the
// server's shutdown signal also stops heartbeat traffic.
func (h *Handler) StartHeartbeat(ctx context.Context) {
	if h.heartbeat == nil {
		return
	}
	go h.heartbeat.loop(ctx, h)
}

func (manager *heartbeatManager) loop(ctx context.Context, h *Handler) {
	nextRound := manager.now().Add(manager.grace)
	for {
		delay := manager.tick
		if wait := nextRound.Sub(manager.now()); wait > 0 && wait < delay {
			delay = wait
		}
		if delay <= 0 {
			delay = time.Millisecond
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		settings := heartbeatSettings(h.routes.Config().Heartbeat)
		manager.publishSettings(settings)
		if !settings.Enabled {
			nextRound = manager.now().Add(manager.tick)
			continue
		}
		if manager.now().Before(nextRound) {
			continue
		}
		manager.runRound(h, settings)
		nextRound = manager.now().Add(manager.gap(settings))
	}
}

// heartbeatRoundGap keeps consecutive rounds apart so the loop cannot spin
// through the whole catalog in a few seconds.
func heartbeatRoundGap(settings routing.HeartbeatSettings) time.Duration {
	smallest := settings.RecoveryMinutes
	if settings.IntervalMinutes > 0 && (smallest == 0 || settings.IntervalMinutes < smallest) {
		smallest = settings.IntervalMinutes
	}
	if smallest <= 0 {
		return heartbeatMinRoundGap
	}
	gap := time.Duration(smallest) * time.Minute / 2
	if gap < heartbeatMinRoundGap {
		return heartbeatMinRoundGap
	}
	return gap
}

func (manager *heartbeatManager) publishSettings(settings routing.HeartbeatSettings) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.status.Enabled = settings.Enabled
	manager.status.IntervalMinutes = settings.IntervalMinutes
	manager.status.RecoveryMinutes = settings.RecoveryMinutes
	manager.status.MaxProbesPerRound = settings.MaxProbesPerRound
	manager.status.Concurrency = settings.Concurrency
}

func (manager *heartbeatManager) Snapshot() HeartbeatStatus {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	status := manager.status
	status.BackoffTargets = len(manager.targets)
	return status
}

func (manager *heartbeatManager) runRound(h *Handler, settings routing.HeartbeatSettings) {
	now := manager.now()
	jobs, skipped, pending := manager.selectJobs(h, settings, now)
	manager.mu.Lock()
	manager.status.Rounds++
	round := manager.status.Rounds
	manager.status.LastRoundStartedAt = now
	manager.status.LastRoundProbes = len(jobs)
	manager.status.LastRoundHealthy = 0
	manager.status.LastRoundFailed = 0
	manager.status.LastRoundSkipped = skipped
	manager.status.PendingRecovery = pending
	manager.status.LastRoundFinishedAt = time.Time{}
	manager.mu.Unlock()
	if len(jobs) == 0 {
		manager.mu.Lock()
		manager.status.LastRoundFinishedAt = now
		manager.status.NextRoundAt = now.Add(manager.gap(settings))
		manager.mu.Unlock()
		slog.Debug("heartbeat round idle", "pending_recovery", pending, "skipped", skipped, "backoff_targets", manager.Snapshot().BackoffTargets)
		return
	}
	hook := func(job probeJob, healthy bool, latency time.Duration) {
		manager.observe(round, settings, job, healthy, latency)
	}
	if _, started := h.probes.beginJobs(h, jobs, skipped, settings.Concurrency, hook); !started {
		// A manual probe-all is already running; the heartbeat must never stack
		// its own upstream requests on top of it.
		manager.mu.Lock()
		manager.status.LastRoundProbes = 0
		manager.status.LastRoundFinishedAt = now
		manager.status.NextRoundAt = now.Add(manager.gap(settings))
		manager.mu.Unlock()
		slog.Info("heartbeat round deferred", "reason", "another probe run is in progress", "jobs", len(jobs))
		return
	}
	manager.mu.Lock()
	manager.status.NextRoundAt = now.Add(manager.gap(settings))
	manager.mu.Unlock()
	slog.Info("heartbeat round started", "jobs", len(jobs), "pending_recovery", pending, "skipped", skipped, "concurrency", settings.Concurrency, "max_probes_per_round", settings.MaxProbesPerRound)
}

// selectJobs sorts every configured capability into the two heartbeat clocks
// and returns the due subset, bounded by MaxProbesPerRound.
//
// Recovery candidates (never verified, or quarantined/cooling) come first:
// they are the models a user cannot reach right now, which is exactly the
// situation the heartbeat exists to repair. Refresh candidates (already
// healthy) only come due on the slow interval and fill whatever budget the
// recovery set left over.
func (manager *heartbeatManager) selectJobs(h *Handler, settings routing.HeartbeatSettings, now time.Time) ([]probeJob, int, int) {
	states := make(map[string]health.State)
	for _, state := range h.health.Snapshot() {
		states[state.Model+"\x00"+state.Capability] = state
	}
	recovery := make([]probeJob, 0)
	refresh := make([]probeJob, 0)
	skipped := 0
	pending := 0
	manager.mu.Lock()
	targets := make(map[string]heartbeatTarget, len(manager.targets))
	for key, target := range manager.targets {
		targets[key] = *target
	}
	manager.mu.Unlock()
	for _, model := range h.catalog.Models() {
		if !h.catalog.ProviderConfigured(model.Provider) {
			skipped++
			continue
		}
		effective, enabled := h.routes.Apply(model)
		if !enabled || len(effective.Functions) == 0 {
			skipped++
			continue
		}
		for _, capability := range effective.Functions {
			if expensiveCapability(capability) {
				skipped++
				continue
			}
			job := probeJob{Model: effective, Capability: capability}
			key := model.ID + "\x00" + capability
			state, known := states[key]
			verified := h.catalog.ModelCapabilityVerified(effective, capability)
			recovering := !verified || !known || !state.Verified || state.Status != health.StatusHealthy
			if recovering {
				if settings.RecoveryMinutes <= 0 {
					skipped++
					continue
				}
				if target, ok := targets[key]; ok && now.Before(target.retryAt) {
					pending++
					continue
				}
				recovery = append(recovery, job)
				continue
			}
			if settings.IntervalMinutes <= 0 {
				skipped++
				continue
			}
			if !state.LastCheckedAt.IsZero() && now.Sub(state.LastCheckedAt) < time.Duration(settings.IntervalMinutes)*time.Minute {
				skipped++
				continue
			}
			refresh = append(refresh, job)
		}
	}
	jobs := make([]probeJob, 0, min(settings.MaxProbesPerRound, len(recovery)+len(refresh)))
	jobs = append(jobs, sortProbeJobs(recovery)...)
	jobs = append(jobs, sortProbeJobs(refresh)...)
	if len(jobs) > settings.MaxProbesPerRound {
		jobs = jobs[:settings.MaxProbesPerRound]
	}
	return jobs, skipped, pending
}

// observe records one finished probe. The backoff schedule is per target and
// always applied; the round counters are only touched when the result belongs
// to the round whose counts are being reported.
func (manager *heartbeatManager) observe(round uint64, settings routing.HeartbeatSettings, job probeJob, healthy bool, latency time.Duration) {
	key := job.Model.ID + "\x00" + job.Capability
	manager.mu.Lock()
	if healthy {
		delete(manager.targets, key)
	} else {
		target := manager.targets[key]
		if target == nil {
			target = &heartbeatTarget{}
			manager.targets[key] = target
		}
		target.failures++
		target.retryAt = manager.now().Add(heartbeatBackoff(settings, target.failures))
	}
	finished := false
	if round == manager.status.Rounds {
		if healthy {
			manager.status.LastRoundHealthy++
		} else {
			manager.status.LastRoundFailed++
		}
		finished = manager.status.LastRoundHealthy+manager.status.LastRoundFailed >= manager.status.LastRoundProbes
		if finished {
			manager.status.LastRoundFinishedAt = manager.now()
		}
	}
	healthyCount, failedCount, probes, backoff := manager.status.LastRoundHealthy, manager.status.LastRoundFailed, manager.status.LastRoundProbes, len(manager.targets)
	manager.mu.Unlock()

	if healthy {
		slog.Info("heartbeat probe ok", "model", job.Model.ID, "capability", job.Capability, "provider", job.Model.Provider, "latency_ms", latency.Milliseconds())
	} else {
		slog.Warn("heartbeat probe failed", "model", job.Model.ID, "capability", job.Capability, "provider", job.Model.Provider, "latency_ms", latency.Milliseconds(), "backoff_targets", backoff)
	}
	if finished {
		slog.Info("heartbeat round finished", "healthy", healthyCount, "failed", failedCount, "probes", probes, "backoff_targets", backoff)
	}
}

// heartbeatBackoff grows the retry window of a repeatedly failing target so a
// permanently dead model costs one probe per backoff window instead of one per
// recovery interval, while a model that just went down is retried quickly.
func heartbeatBackoff(settings routing.HeartbeatSettings, failures int) time.Duration {
	base := time.Duration(settings.RecoveryMinutes) * time.Minute
	if base <= 0 {
		base = time.Duration(settings.IntervalMinutes) * time.Minute
	}
	if base <= 0 {
		return heartbeatMinRoundGap
	}
	factor := 1
	for step := 0; step < failures-1 && factor < heartbeatMaxBackoffRate; step++ {
		factor *= 2
	}
	if factor > heartbeatMaxBackoffRate {
		factor = heartbeatMaxBackoffRate
	}
	return base * time.Duration(factor)
}

// heartbeatSettings resolves the persisted configuration and applies the
// environment overrides on top of it.
func heartbeatSettings(config *routing.HeartbeatConfig) routing.HeartbeatSettings {
	settings := config.Resolve()
	if value, ok := heartbeatEnvValue(heartbeatEnvEnabled); ok {
		settings.Enabled = value
	}
	if value, ok := heartbeatEnvInt(heartbeatEnvInterval); ok {
		settings.IntervalMinutes = value
	}
	if value, ok := heartbeatEnvInt(heartbeatEnvRecovery); ok {
		settings.RecoveryMinutes = value
	}
	if value, ok := heartbeatEnvInt(heartbeatEnvMaxProbes); ok && value > 0 {
		settings.MaxProbesPerRound = value
	}
	if value, ok := heartbeatEnvInt(heartbeatEnvConcurrency); ok && value > 0 {
		settings.Concurrency = value
	}
	if settings.Concurrency > routing.MaxHeartbeatConcurrency {
		settings.Concurrency = routing.MaxHeartbeatConcurrency
	}
	if settings.MaxProbesPerRound > routing.MaxHeartbeatProbesPerRound {
		settings.MaxProbesPerRound = routing.MaxHeartbeatProbesPerRound
	}
	return settings
}

func heartbeatEnvValue(name string) (bool, bool) {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	if raw == "" {
		return false, false
	}
	switch raw {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	default:
		return false, false
	}
}

func heartbeatEnvInt(name string) (int, bool) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, false
	}
	return value, true
}
