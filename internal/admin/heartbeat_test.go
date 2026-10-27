package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/credentials"
	"github.com/sjzsdu/free-router/internal/health"
	"github.com/sjzsdu/free-router/internal/provider"
	"github.com/sjzsdu/free-router/internal/routing"
)

// chatUpstream is a minimal OpenAI-compatible upstream whose availability can
// be flipped at runtime, which is what the heartbeat exists to observe.
type chatUpstream struct {
	*httptest.Server
	failing atomic.Bool
	calls   atomic.Int64
	mu      sync.Mutex
	models  []string
}

func newChatUpstream(t *testing.T, models ...string) *chatUpstream {
	t.Helper()
	upstream := &chatUpstream{models: models}
	upstream.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/models"):
			entries := make([]string, 0, len(upstream.models))
			for _, model := range upstream.models {
				entries = append(entries, fmt.Sprintf(`{"id":%q}`, model))
			}
			_, _ = w.Write([]byte(`{"data":[` + strings.Join(entries, ",") + `]}`))
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			upstream.calls.Add(1)
			if upstream.failing.Load() {
				http.Error(w, `{"error":{"message":"upstream down","type":"server_error"}}`, http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func newHeartbeatHandler(t *testing.T, providersJSON string, upstream *chatUpstream) (*Handler, *chatUpstream) {
	t.Helper()
	for _, key := range provider.SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
	registry, err := provider.NewRegistry(providersJSON)
	if err != nil {
		t.Fatal(err)
	}
	models := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), upstream.Client())
	discoverModelsForTest(t, models)
	routes, err := routing.New(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := New(routes, models, credentials.NewFileOnly(filepath.Join(t.TempDir(), "credentials.json")), health.New(), Config{}, nil)
	return handler, upstream
}

func waitForProbeRun(t *testing.T, probes *probeManager) ProbeStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if status := probes.Snapshot(); status.Status == "completed" {
			return status
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("probe run did not finish: %#v", probes.Snapshot())
	return ProbeStatus{}
}

// The whole point of the feature: a model that tripped its circuit breaker is
// recovered by the heartbeat without anyone opening the admin page.
func TestHeartbeatRecoversQuarantinedModel(t *testing.T) {
	upstream := newChatUpstream(t, "chat-a")
	handler, _ := newHeartbeatHandler(t, `[{"id":"test","base_url":"`+upstream.URL+`","no_auth":true}]`, upstream)

	// The model was verified earlier, then failed in production and got
	// quarantined by the health tracker.
	if err := handler.catalog.RecordCapabilityVerification("test/chat-a", catalog.FunctionChat, time.Now(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		handler.health.Failure("test/chat-a", catalog.FunctionChat, time.Millisecond, http.StatusBadGateway, "upstream down", 0)
	}
	if handler.health.Healthy("test/chat-a", catalog.FunctionChat) {
		t.Fatal("test setup did not quarantine the model")
	}

	settings := routing.HeartbeatSettings{Enabled: true, IntervalMinutes: 30, RecoveryMinutes: 10, MaxProbesPerRound: 8, Concurrency: 2}
	handler.heartbeat.runRound(handler, settings)
	status := waitForProbeRun(t, handler.probes)
	if status.Healthy != 1 || status.Failed != 0 {
		t.Fatalf("probe status = %#v", status)
	}
	if !handler.health.Healthy("test/chat-a", catalog.FunctionChat) {
		t.Fatalf("heartbeat did not restore model health: %#v", handler.health.Snapshot())
	}
	if !handler.catalog.CapabilityVerified("test/chat-a", catalog.FunctionChat) {
		t.Fatal("heartbeat did not restore capability verification")
	}
	heartbeat := handler.heartbeat.Snapshot()
	if heartbeat.Rounds != 1 || heartbeat.LastRoundProbes != 1 || heartbeat.LastRoundHealthy != 1 || heartbeat.LastRoundFinishedAt.IsZero() {
		t.Fatalf("heartbeat status = %#v", heartbeat)
	}
	if upstream.calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", upstream.calls.Load())
	}
}

// A failed probe must not keep hammering the same model: the target backs off
// and the next round reports it as pending instead of re-probing it.
func TestHeartbeatBacksOffAfterFailedProbe(t *testing.T) {
	upstream := newChatUpstream(t, "chat-a")
	upstream.failing.Store(true)
	handler, _ := newHeartbeatHandler(t, `[{"id":"test","base_url":"`+upstream.URL+`","no_auth":true}]`, upstream)

	settings := routing.HeartbeatSettings{Enabled: true, IntervalMinutes: 30, RecoveryMinutes: 10, MaxProbesPerRound: 8, Concurrency: 1}
	handler.heartbeat.runRound(handler, settings)
	status := waitForProbeRun(t, handler.probes)
	if status.Failed != 1 {
		t.Fatalf("probe status = %#v", status)
	}
	heartbeat := handler.heartbeat.Snapshot()
	if heartbeat.BackoffTargets != 1 || heartbeat.LastRoundFailed != 1 {
		t.Fatalf("heartbeat status = %#v", heartbeat)
	}

	jobs, _, pending := handler.heartbeat.selectJobs(handler, settings, time.Now())
	if len(jobs) != 0 || pending != 1 {
		t.Fatalf("backoff target was scheduled again: jobs=%#v pending=%d", jobs, pending)
	}
	if calls := upstream.calls.Load(); calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}

	// Once the retry window elapses the target becomes due again and the
	// recovery succeeds.
	upstream.failing.Store(false)
	handler.heartbeat.mu.Lock()
	for _, target := range handler.heartbeat.targets {
		target.retryAt = time.Now().Add(-time.Second)
	}
	handler.heartbeat.mu.Unlock()
	handler.heartbeat.runRound(handler, settings)
	status = waitForProbeRun(t, handler.probes)
	if status.Healthy != 1 {
		t.Fatalf("probe status = %#v", status)
	}
	if !handler.health.Healthy("test/chat-a", catalog.FunctionChat) {
		t.Fatal("recovery retry did not restore the model")
	}
	if handler.heartbeat.Snapshot().BackoffTargets != 0 {
		t.Fatalf("successful recovery kept a backoff target: %#v", handler.heartbeat.Snapshot())
	}
}

// One round must never fan out over every provider: the budget caps the round
// and the selection interleaves providers so the cap is shared fairly.
func TestHeartbeatRoundBudgetSpreadsAcrossProviders(t *testing.T) {
	upstream := newChatUpstream(t, "chat-a", "chat-b", "chat-c", "chat-d")
	handler, _ := newHeartbeatHandler(t, `[{"id":"alpha","base_url":"`+upstream.URL+`","no_auth":true},{"id":"beta","base_url":"`+upstream.URL+`","no_auth":true},{"id":"gamma","base_url":"`+upstream.URL+`","no_auth":true}]`, upstream)
	settings := routing.HeartbeatSettings{Enabled: true, IntervalMinutes: 30, RecoveryMinutes: 10, MaxProbesPerRound: 3, Concurrency: 2}

	jobs, _, _ := handler.heartbeat.selectJobs(handler, settings, time.Now())
	if len(jobs) != 3 {
		t.Fatalf("round budget ignored: %d jobs", len(jobs))
	}
	providers := make(map[string]int)
	for _, job := range jobs {
		providers[job.Model.Provider]++
	}
	if len(providers) != 3 {
		t.Fatalf("round was not spread across providers: %#v", providers)
	}

	handler.heartbeat.runRound(handler, settings)
	status := waitForProbeRun(t, handler.probes)
	if status.Total != 3 || status.Healthy != 3 {
		t.Fatalf("probe status = %#v", status)
	}
	if calls := upstream.calls.Load(); calls != 3 {
		t.Fatalf("upstream calls = %d, want 3 (budget is a hard cap)", calls)
	}
}

// Healthy models are only re-checked on the slow interval, and never when that
// interval is disabled.
func TestHeartbeatLeavesFreshHealthyModelsAlone(t *testing.T) {
	upstream := newChatUpstream(t, "chat-a")
	handler, _ := newHeartbeatHandler(t, `[{"id":"test","base_url":"`+upstream.URL+`","no_auth":true}]`, upstream)
	if err := handler.catalog.RecordCapabilityVerification("test/chat-a", catalog.FunctionChat, time.Now(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	handler.health.ProbeSuccess("test/chat-a", catalog.FunctionChat, time.Millisecond)

	active := routing.HeartbeatSettings{Enabled: true, IntervalMinutes: 30, RecoveryMinutes: 10, MaxProbesPerRound: 8, Concurrency: 1}
	if jobs, _, _ := handler.heartbeat.selectJobs(handler, active, time.Now()); len(jobs) != 0 {
		t.Fatalf("freshly checked model was rescheduled: %#v", jobs)
	}

	// The same healthy model becomes a refresh candidate once its last check
	// is older than the slow interval.
	handler.health.RestoreProbeSuccess("test/chat-a", catalog.FunctionChat, time.Now().Add(-31*time.Minute), 1)
	if jobs, _, _ := handler.heartbeat.selectJobs(handler, active, time.Now()); len(jobs) != 1 {
		t.Fatalf("stale healthy model was not rescheduled: %#v", jobs)
	}

	refreshDisabled := active
	refreshDisabled.IntervalMinutes = 0
	if jobs, _, _ := handler.heartbeat.selectJobs(handler, refreshDisabled, time.Now()); len(jobs) != 0 {
		t.Fatalf("refresh clock ran while disabled: %#v", jobs)
	}
}

// Unconfigured providers and expensive generation capabilities are never part
// of a heartbeat round: the first has no key, the second costs real quota.
func TestHeartbeatSkipsUnconfiguredAndExpensiveTargets(t *testing.T) {
	upstream := newChatUpstream(t, "chat-a")
	handler, _ := newHeartbeatHandler(t, `[{"id":"test","base_url":"`+upstream.URL+`","no_auth":true}]`, upstream)

	config := handler.routes.Config()
	config.Models["test/chat-a"] = routing.ModelOverride{Functions: []string{catalog.FunctionImageGeneration}}
	if err := handler.routes.Update(config); err != nil {
		t.Fatal(err)
	}
	settings := routing.HeartbeatSettings{Enabled: true, IntervalMinutes: 30, RecoveryMinutes: 10, MaxProbesPerRound: 8, Concurrency: 1}
	if jobs, skipped, _ := handler.heartbeat.selectJobs(handler, settings, time.Now()); len(jobs) != 0 || skipped == 0 {
		t.Fatalf("expensive capability was scheduled: jobs=%#v skipped=%d", jobs, skipped)
	}

	// A model disabled through the route configuration is never probed.
	config = handler.routes.Config()
	config.Models["test/chat-a"] = routing.ModelOverride{Disabled: true}
	if err := handler.routes.Update(config); err != nil {
		t.Fatal(err)
	}
	if jobs, skipped, _ := handler.heartbeat.selectJobs(handler, settings, time.Now()); len(jobs) != 0 || skipped == 0 {
		t.Fatalf("disabled model was scheduled: jobs=%#v skipped=%d", jobs, skipped)
	}
}

// The heartbeat must never stack on top of a manual probe-all.
func TestHeartbeatDefersWhenManualProbeIsRunning(t *testing.T) {
	upstream := newChatUpstream(t, "chat-a")
	handler, _ := newHeartbeatHandler(t, `[{"id":"test","base_url":"`+upstream.URL+`","no_auth":true}]`, upstream)

	handler.probes.mu.Lock()
	handler.probes.status = ProbeStatus{Status: "running", Total: 5}
	handler.probes.mu.Unlock()
	settings := routing.HeartbeatSettings{Enabled: true, IntervalMinutes: 30, RecoveryMinutes: 10, MaxProbesPerRound: 8, Concurrency: 1}
	handler.heartbeat.runRound(handler, settings)
	if calls := upstream.calls.Load(); calls != 0 {
		t.Fatalf("heartbeat probed while a manual run owned the slot: calls=%d", calls)
	}
	heartbeat := handler.heartbeat.Snapshot()
	if heartbeat.LastRoundProbes != 0 || heartbeat.LastRoundFinishedAt.IsZero() {
		t.Fatalf("heartbeat status = %#v", heartbeat)
	}
}

func TestHeartbeatLoopProbesOnItsOwn(t *testing.T) {
	upstream := newChatUpstream(t, "chat-a")
	handler, _ := newHeartbeatHandler(t, `[{"id":"test","base_url":"`+upstream.URL+`","no_auth":true}]`, upstream)
	handler.heartbeat.tick = 2 * time.Millisecond
	handler.heartbeat.grace = 0
	handler.heartbeat.gap = func(routing.HeartbeatSettings) time.Duration { return time.Millisecond }

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	handler.StartHeartbeat(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if upstream.calls.Load() > 0 && handler.health.Healthy("test/chat-a", catalog.FunctionChat) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("heartbeat loop never probed: status=%#v calls=%d", handler.heartbeat.Snapshot(), upstream.calls.Load())
}

func TestHeartbeatLoopRespectsDisabledSwitch(t *testing.T) {
	upstream := newChatUpstream(t, "chat-a")
	handler, _ := newHeartbeatHandler(t, `[{"id":"test","base_url":"`+upstream.URL+`","no_auth":true}]`, upstream)
	disabled := false
	config := handler.routes.Config()
	config.Heartbeat = &routing.HeartbeatConfig{Enabled: &disabled}
	if err := handler.routes.Update(config); err != nil {
		t.Fatal(err)
	}
	handler.heartbeat.tick = 2 * time.Millisecond
	handler.heartbeat.grace = 0
	handler.heartbeat.gap = func(routing.HeartbeatSettings) time.Duration { return time.Millisecond }

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	handler.StartHeartbeat(ctx)
	time.Sleep(100 * time.Millisecond)
	if calls := upstream.calls.Load(); calls != 0 {
		t.Fatalf("disabled heartbeat probed: calls=%d", calls)
	}
	if status := handler.heartbeat.Snapshot(); status.Rounds != 0 || status.Enabled {
		t.Fatalf("heartbeat status = %#v", status)
	}
}

func TestHeartbeatSettingsDefaultsAndEnvironmentOverrides(t *testing.T) {
	for _, name := range []string{heartbeatEnvEnabled, heartbeatEnvInterval, heartbeatEnvRecovery, heartbeatEnvMaxProbes, heartbeatEnvConcurrency} {
		t.Setenv(name, "")
	}
	settings, problems := heartbeatSettings(nil)
	if len(problems) != 0 {
		t.Fatalf("defaults reported problems: %#v", problems)
	}
	if !settings.Enabled || settings.IntervalMinutes != 30 || settings.RecoveryMinutes != 10 || settings.MaxProbesPerRound != 8 || settings.Concurrency != 2 {
		t.Fatalf("defaults = %#v", settings)
	}

	enabled := false
	interval, recovery, probes, concurrency := 0, 5, 3, 1
	configured, problems := heartbeatSettings(&routing.HeartbeatConfig{Enabled: &enabled, IntervalMinutes: &interval, RecoveryMinutes: &recovery, MaxProbesPerRound: &probes, Concurrency: &concurrency})
	if len(problems) != 0 {
		t.Fatalf("configured heartbeat reported problems: %#v", problems)
	}
	if configured.Enabled || configured.IntervalMinutes != 0 || configured.RecoveryMinutes != 5 || configured.MaxProbesPerRound != 3 || configured.Concurrency != 1 {
		t.Fatalf("configured = %#v", configured)
	}

	t.Setenv(heartbeatEnvEnabled, "off")
	t.Setenv(heartbeatEnvInterval, "45")
	t.Setenv(heartbeatEnvRecovery, "20")
	t.Setenv(heartbeatEnvMaxProbes, "4")
	t.Setenv(heartbeatEnvConcurrency, "3")
	fromEnv, _ := heartbeatSettings(nil)
	if fromEnv.Enabled || fromEnv.IntervalMinutes != 45 || fromEnv.RecoveryMinutes != 20 || fromEnv.MaxProbesPerRound != 4 || fromEnv.Concurrency != 3 {
		t.Fatalf("environment overrides = %#v", fromEnv)
	}

	// Values above the documented maximum are clamped instead of trusted, and
	// the clamp is reported so the operator can see it in the log.
	t.Setenv(heartbeatEnvConcurrency, "40")
	t.Setenv(heartbeatEnvMaxProbes, "900")
	clamped, clampProblems := heartbeatSettings(nil)
	if clamped.Concurrency != routing.MaxHeartbeatConcurrency || clamped.MaxProbesPerRound != routing.MaxHeartbeatProbesPerRound {
		t.Fatalf("clamped = %#v", clamped)
	}
	if len(clampProblems) != 2 {
		t.Fatalf("clamped values were not reported: %#v", clampProblems)
	}
}

// The environment must not be able to produce the combination config.json
// rejects. Otherwise an enabled heartbeat with both clocks at 0 looks alive in
// the admin UI while every quarantined model silently stays quarantined — the
// exact symptom keep-alive exists to remove.
func TestHeartbeatEnvironmentCannotDisableBothClocks(t *testing.T) {
	t.Setenv(heartbeatEnvEnabled, "1")
	t.Setenv(heartbeatEnvInterval, "0")
	t.Setenv(heartbeatEnvRecovery, "0")
	t.Setenv(heartbeatEnvMaxProbes, "")
	t.Setenv(heartbeatEnvConcurrency, "")

	settings, problems := heartbeatSettings(nil)
	if !settings.Enabled {
		t.Fatalf("enabled override was dropped: %#v", settings)
	}
	if settings.IntervalMinutes != routing.DefaultHeartbeatIntervalMinutes || settings.RecoveryMinutes != routing.DefaultHeartbeatRecoveryMinutes {
		t.Fatalf("both clocks stayed 0: %#v", settings)
	}
	if len(problems) == 0 {
		t.Fatal("the double-zero environment combination was accepted silently")
	}
	if err := routing.ValidateHeartbeatSettings(settings); err != nil {
		t.Fatalf("merged settings still fail validation: %v", err)
	}

	// One clock at 0 stays legal: disabling a single clock is a real feature.
	t.Setenv(heartbeatEnvInterval, "")
	t.Setenv(heartbeatEnvRecovery, "")
	enabled := true
	interval, recovery := 0, 15
	onlyRecovery, singleProblems := heartbeatSettings(&routing.HeartbeatConfig{Enabled: &enabled, IntervalMinutes: &interval, RecoveryMinutes: &recovery})
	if onlyRecovery.IntervalMinutes != 0 || onlyRecovery.RecoveryMinutes != 15 || len(singleProblems) != 0 {
		t.Fatalf("a single disabled clock was treated as a problem: %#v %#v", onlyRecovery, singleProblems)
	}
}

// A mistyped environment variable must be visible in the log instead of
// silently falling back to the default.
func TestHeartbeatInvalidEnvironmentValuesAreReported(t *testing.T) {
	t.Setenv(heartbeatEnvEnabled, "sure")
	t.Setenv(heartbeatEnvInterval, "thirty")
	t.Setenv(heartbeatEnvRecovery, "-5")
	t.Setenv(heartbeatEnvMaxProbes, "")
	t.Setenv(heartbeatEnvConcurrency, "")

	settings, problems := heartbeatSettings(nil)
	if settings.Enabled != routing.DefaultHeartbeatEnabled || settings.IntervalMinutes != routing.DefaultHeartbeatIntervalMinutes || settings.RecoveryMinutes != routing.DefaultHeartbeatRecoveryMinutes {
		t.Fatalf("invalid values changed the settings: %#v", settings)
	}
	if len(problems) != 3 {
		t.Fatalf("problems = %#v, want one per invalid variable", problems)
	}

	// warnOnce is what turns those strings into log lines; running it twice must
	// not duplicate them.
	manager := newHeartbeatManager(newProbeManager(t.TempDir()))
	manager.warnOnce(problems)
	first := len(manager.warned)
	manager.warnOnce(problems)
	if first != len(problems) || len(manager.warned) != first {
		t.Fatalf("warnOnce was not idempotent: %#v", manager.warned)
	}
}

func TestHeartbeatRoundGapIsConservative(t *testing.T) {
	cases := []struct {
		settings routing.HeartbeatSettings
		want     time.Duration
	}{
		{routing.HeartbeatSettings{IntervalMinutes: 30, RecoveryMinutes: 10}, 5 * time.Minute},
		{routing.HeartbeatSettings{IntervalMinutes: 30, RecoveryMinutes: 0}, 15 * time.Minute},
		{routing.HeartbeatSettings{IntervalMinutes: 0, RecoveryMinutes: 2}, time.Minute},
		{routing.HeartbeatSettings{IntervalMinutes: 0, RecoveryMinutes: 0}, time.Minute},
	}
	for _, testCase := range cases {
		if got := heartbeatRoundGap(testCase.settings); got != testCase.want {
			t.Errorf("gap(%#v) = %s, want %s", testCase.settings, got, testCase.want)
		}
	}
}

func TestAdminStateExposesHeartbeatStatus(t *testing.T) {
	upstream := newChatUpstream(t, "chat-a")
	handler, _ := newHeartbeatHandler(t, `[{"id":"test","base_url":"`+upstream.URL+`","no_auth":true}]`, upstream)

	request := httptest.NewRequest(http.MethodGet, "/admin/api/state", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Heartbeat HeartbeatStatus `json:"heartbeat"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if !response.Heartbeat.Enabled || response.Heartbeat.IntervalMinutes != 30 || response.Heartbeat.RecoveryMinutes != 10 {
		t.Fatalf("heartbeat = %#v", response.Heartbeat)
	}
}
