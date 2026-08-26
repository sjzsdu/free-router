package health

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestPersistentTrackerRestoresManualValidationAndRuntimeFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health.json")
	tracker, err := NewPersistent(path)
	if err != nil {
		t.Fatal(err)
	}
	tracker.ProbeSuccess("provider/good", "chat", 25*time.Millisecond)
	tracker.ProbeFailure("provider/bad", "chat", 30*time.Millisecond, 404, "model unavailable")
	tracker.Failure("provider/good", "chat", 40*time.Millisecond, 500, "runtime failed", 0)

	reloaded, err := NewPersistent(path)
	if err != nil {
		t.Fatal(err)
	}
	states := reloaded.Snapshot()
	if len(states) != 2 {
		t.Fatalf("restored states=%#v", states)
	}
	byModel := make(map[string]State, len(states))
	for _, state := range states {
		byModel[state.Model] = state
	}
	good := byModel["provider/good"]
	bad := byModel["provider/bad"]
	if !good.Verified || good.Status != StatusDegraded || good.LastError != "runtime failed" {
		t.Fatalf("runtime failure was not restored: %#v", good)
	}
	if bad.Verified || bad.Status != StatusDegraded || bad.LastError != "model unavailable" || bad.Checks != 1 {
		t.Fatalf("manual validation failure was not restored: %#v", bad)
	}
	reloaded.Success("provider/good", "chat", 20*time.Millisecond, 200)
	recovered, err := NewPersistent(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range recovered.Snapshot() {
		if state.Model == "provider/good" && (state.Status != StatusHealthy || state.LastError != "") {
			t.Fatalf("runtime recovery was not persisted: %#v", state)
		}
	}
}

func TestPersistentTrackerRestoresProviderFailureAndReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health.json")
	tracker, err := NewPersistent(path)
	if err != nil {
		t.Fatal(err)
	}
	tracker.Failure("provider/model", "chat", 0, 401, "invalid key", 0)
	reloaded, err := NewPersistent(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Available("provider/other", "chat") {
		t.Fatal("persisted provider failure did not quarantine sibling models")
	}
	reloaded.Reset("provider/model", "")
	reset, err := NewPersistent(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reset.Available("provider/other", "chat") || len(reset.Snapshot()) != 0 || len(reset.ProviderSnapshot()) != 0 {
		t.Fatalf("reset was not persisted: models=%#v providers=%#v", reset.Snapshot(), reset.ProviderSnapshot())
	}
}

func TestClientErrorDoesNotAffectHealth(t *testing.T) {
	tracker := New()
	tracker.Failure("provider/model", "chat", 0, 400, "bad request", 0)
	if !tracker.Available("provider/model", "chat") {
		t.Fatal("client error should not quarantine model")
	}
	state := tracker.Snapshot()[0]
	if state.Status != StatusHealthy {
		t.Fatalf("client error set status to %q, want %q", state.Status, StatusHealthy)
	}
}

func TestAuthErrorQuarantinesProvider(t *testing.T) {
	tracker := New()
	tracker.Failure("provider/model", "chat", 0, 401, "unauthorized", 0)
	if tracker.Available("provider/model", "chat") {
		t.Fatal("auth error should quarantine provider")
	}
	if tracker.Available("provider/other", "chat") {
		t.Fatal("auth error should quarantine all models from same provider")
	}
}

func TestQuotaErrorQuarantinesProvider(t *testing.T) {
	tracker := New()
	tracker.Failure("provider/model", "chat", 0, 402, "quota exhausted", 0)
	if tracker.Available("provider/other", "chat") {
		t.Fatal("account quota error should quarantine all models from same provider")
	}
}

func TestRateLimitWithRetryAfterEnforcesCooldown(t *testing.T) {
	tracker := New()
	now := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	tracker.now = func() time.Time { return now }

	tracker.Failure("provider/model", "chat", time.Second, 429, "rate limited", 2*time.Minute)
	if tracker.Available("provider/model", "chat") {
		t.Fatal("rate limited model should be unavailable during cooldown")
	}
	state := tracker.Snapshot()[0]
	if state.Status != StatusCooling {
		t.Fatalf("rate limit set status to %q, want %q", state.Status, StatusCooling)
	}

	now = now.Add(1 * time.Minute)
	if tracker.Available("provider/model", "chat") {
		t.Fatal("model should remain unavailable during cooldown")
	}

	now = now.Add(2 * time.Minute)
	if !tracker.Available("provider/model", "chat") {
		t.Fatal("model should become available after cooldown expires")
	}
	state = tracker.Snapshot()[0]
	if state.Status != StatusHalfOpen {
		t.Fatalf("after cooldown status should be %q, got %q", StatusHalfOpen, state.Status)
	}
}

func TestServerErrorRequiresThresholdBeforeQuarantine(t *testing.T) {
	tracker := New()
	for i := 0; i < DefaultFailureThreshold-1; i++ {
		tracker.Failure("provider/model", "chat", 0, 500, "server error", 0)
		if !tracker.Available("provider/model", "chat") {
			t.Fatalf("model should be available after %d failures (below threshold)", i+1)
		}
	}

	tracker.Failure("provider/model", "chat", 0, 500, "server error", 0)
	if tracker.Available("provider/model", "chat") {
		t.Fatal("model should be quarantined after reaching threshold")
	}
	state := tracker.Snapshot()[0]
	if state.Status != StatusOpen {
		t.Fatalf("after threshold status should be %q, got %q", StatusOpen, state.Status)
	}
}

func TestNetworkErrorRequiresThresholdBeforeQuarantine(t *testing.T) {
	tracker := New()
	for i := 0; i < DefaultFailureThreshold-1; i++ {
		tracker.Failure("provider/model", "chat", 0, 0, "network error", 0)
		if !tracker.Available("provider/model", "chat") {
			t.Fatalf("model should be available after %d network failures (below threshold)", i+1)
		}
		state := tracker.Snapshot()[0]
		if state.Status != StatusDegraded {
			t.Fatalf("below threshold status should be %q, got %q", StatusDegraded, state.Status)
		}
	}

	tracker.Failure("provider/model", "chat", 0, 0, "network error", 0)
	if tracker.Available("provider/model", "chat") {
		t.Fatal("model should be quarantined after reaching threshold")
	}
}

func TestSuccessRecoversFromAllStates(t *testing.T) {
	tracker := New()
	now := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	tracker.now = func() time.Time { return now }

	for i := 0; i < DefaultFailureThreshold; i++ {
		tracker.Failure("provider/model", "chat", 0, 500, "error", 0)
	}
	if tracker.Available("provider/model", "chat") {
		t.Fatal("model should be quarantined before recovery")
	}

	tracker.Success("provider/model", "chat", 100*time.Millisecond, 200)
	if !tracker.Available("provider/model", "chat") {
		t.Fatal("model should recover after success")
	}
	state := tracker.Snapshot()[0]
	if state.Status != StatusHealthy || state.ConsecutiveFailures != 0 {
		t.Fatalf("state = %#v", state)
	}
}

func TestHalfOpenAfterCooldown(t *testing.T) {
	tracker := New()
	now := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	tracker.now = func() time.Time { return now }

	for i := 0; i < DefaultFailureThreshold; i++ {
		tracker.Failure("provider/model", "chat", 0, 500, "error", 0)
	}

	state := tracker.Snapshot()[0]
	if state.Status != StatusOpen {
		t.Fatalf("status should be %q, got %q", StatusOpen, state.Status)
	}

	now = now.Add(DefaultCoolDownMax + time.Minute)
	if !tracker.Available("provider/model", "chat") {
		t.Fatal("model should become available after cooldown")
	}
	state = tracker.Snapshot()[0]
	if state.Status != StatusHalfOpen {
		t.Fatalf("after cooldown status should be %q, got %q", StatusHalfOpen, state.Status)
	}
}

func TestResetReturnsFailedModelToUnknownState(t *testing.T) {
	tracker := New()
	for i := 0; i < DefaultFailureThreshold; i++ {
		tracker.Failure("provider/model", "chat", 0, 500, "upstream failed", 0)
	}
	tracker.Reset("provider/model", "")
	if !tracker.Available("provider/model", "chat") || len(tracker.Snapshot()) != 0 {
		t.Fatal("reset model must be eligible for a new probe")
	}
}

func TestCapabilityFailureDoesNotQuarantineOtherCapabilities(t *testing.T) {
	tracker := New()
	for i := 0; i < DefaultFailureThreshold; i++ {
		tracker.Failure("provider/multimodal", "image-understanding", 0, 500, "unsupported image", 0)
	}
	if tracker.Available("provider/multimodal", "image-understanding") {
		t.Fatal("failed image understanding capability should be quarantined")
	}
	if !tracker.Available("provider/multimodal", "chat") {
		t.Fatal("image failure should not quarantine chat capability")
	}
}

func TestProbeHealthUsesTTLWithoutInflatingRequests(t *testing.T) {
	tracker := New()
	now := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	tracker.now = func() time.Time { return now }
	tracker.ProbeSuccess("provider/model", "chat", 25*time.Millisecond)
	state := tracker.Snapshot()[0]
	if state.Status != StatusHealthy || state.Checks != 1 || state.Requests != 0 || state.LastCheckLatencyMS != 25 {
		t.Fatalf("state = %#v", state)
	}
	if tracker.ProbeDue("provider/model", "chat", 24*time.Hour) {
		t.Fatal("fresh probe must be cached")
	}
	now = now.Add(24 * time.Hour)
	if !tracker.ProbeDue("provider/model", "chat", 24*time.Hour) {
		t.Fatal("probe must expire after 24 hours")
	}
	for i := 0; i < DefaultFailureThreshold; i++ {
		tracker.ProbeFailure("provider/model", "chat", time.Second, 500, "server error")
	}
	if tracker.Available("provider/model", "chat") {
		t.Fatal("failed probe must quarantine model after threshold")
	}
}

func TestHealthyRequiresExplicitSuccessAndRejectsDegradedState(t *testing.T) {
	tracker := New()
	if tracker.Healthy("provider/model", "chat") {
		t.Fatal("unknown capability must not be a healthy route candidate")
	}
	tracker.Success("provider/model", "chat", time.Millisecond, 200)
	if tracker.Healthy("provider/model", "chat") {
		t.Fatal("ordinary traffic success must not replace explicit capability verification")
	}
	tracker.ProbeSuccess("provider/model", "chat", time.Millisecond)
	if !tracker.Healthy("provider/model", "chat") {
		t.Fatal("successful capability probe did not make the route candidate healthy")
	}
	tracker.Failure("provider/model", "chat", time.Millisecond, 500, "server error", 0)
	if tracker.Healthy("provider/model", "chat") {
		t.Fatal("degraded capability remained a healthy route candidate")
	}
}

func TestProbeFailureWithClientErrorIsNotHealthy(t *testing.T) {
	tracker := New()
	tracker.ProbeFailure("provider/model", "chat", time.Second, 400, "client error")
	if !tracker.Available("provider/model", "chat") {
		t.Fatal("one client error probe should remain below the circuit-breaker threshold")
	}
	state := tracker.Snapshot()[0]
	if state.Status != StatusDegraded {
		t.Fatalf("client error probe set status to %q, want %q", state.Status, StatusDegraded)
	}
}

func TestErrorClassification(t *testing.T) {
	tests := []struct {
		status int
		want   ErrorType
	}{
		{0, ErrorNetwork},
		{400, ErrorClient},
		{401, ErrorAuth},
		{402, ErrorAuth},
		{403, ErrorAuth},
		{404, ErrorServer},
		{410, ErrorServer},
		{429, ErrorRateLimit},
		{500, ErrorServer},
		{503, ErrorServer},
		{302, ErrorServer},
	}
	for _, tt := range tests {
		t.Run(fmtStatus(tt.status), func(t *testing.T) {
			if got := classifyError(tt.status); got != tt.want {
				t.Fatalf("classifyError(%d) = %v, want %v", tt.status, got, tt.want)
			}
		})
	}
}

func fmtStatus(status int) string {
	if status == 0 {
		return "network"
	}
	return string(rune('0'+status/100)) + "xx"
}

func TestProviderFromModel(t *testing.T) {
	tests := []struct {
		model    string
		provider string
	}{
		{"provider/model", "provider"},
		{"groq/llama3-70b", "groq"},
		{"openrouter:free", "openrouter"},
		{"model-only", ""},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			if got := providerFromModel(tt.model); got != tt.provider {
				t.Fatalf("providerFromModel(%q) = %q, want %q", tt.model, got, tt.provider)
			}
		})
	}
}

func TestAuthRecoveryResetsProviderState(t *testing.T) {
	tracker := New()
	tracker.Failure("provider/model", "chat", 0, 401, "unauthorized", 0)
	if tracker.Available("provider/model", "chat") {
		t.Fatal("auth error should quarantine provider")
	}

	tracker.Success("provider/model", "chat", 100*time.Millisecond, 200)
	if !tracker.Available("provider/model", "chat") {
		t.Fatal("success should recover provider")
	}
	if !tracker.Available("provider/other", "chat") {
		t.Fatal("success should recover all models from same provider")
	}
}

func TestTryAcquireRespectsCooldownPeriod(t *testing.T) {
	tracker := New()
	now := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	tracker.now = func() time.Time { return now }

	for i := 0; i < DefaultFailureThreshold; i++ {
		tracker.Failure("provider/model", "chat", 0, 500, "error", 0)
	}

	if tracker.Available("provider/model", "chat") {
		t.Fatal("model should be unavailable during cooldown")
	}

	if tracker.TryAcquire("provider/model", "chat") {
		t.Fatal("TryAcquire should return false during cooldown (available=false first_acquire=false)")
	}

	now = now.Add(DefaultCoolDownMax + time.Second)
	if !tracker.Available("provider/model", "chat") {
		t.Fatal("model should be available after cooldown")
	}

	if !tracker.TryAcquire("provider/model", "chat") {
		t.Fatal("TryAcquire should return true after cooldown")
	}
}

func TestTryAcquireConcurrentCooldownProtection(t *testing.T) {
	tracker := New()
	now := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	tracker.now = func() time.Time { return now }

	for i := 0; i < DefaultFailureThreshold; i++ {
		tracker.Failure("provider/model", "chat", 0, 500, "error", 0)
	}

	if tracker.Available("provider/model", "chat") {
		t.Fatal("model should be unavailable during cooldown")
	}

	var wg sync.WaitGroup
	successCount := 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if tracker.TryAcquire("provider/model", "chat") {
				successCount++
			}
		}()
	}
	wg.Wait()

	if successCount != 0 {
		t.Fatalf("TryAcquire should reject all requests during cooldown, got %d successes", successCount)
	}
}

func TestCooldownForNeverOverflows(t *testing.T) {
	tracker := New()
	for _, failures := range []int{1, 5, 30, 63, 64, 100, 1000} {
		cooldown := tracker.cooldownFor(failures)
		if cooldown < 0 {
			t.Fatalf("cooldownFor(%d) returned negative %v", failures, cooldown)
		}
		if cooldown > tracker.cooldownMax+tracker.cooldownMax/2 {
			t.Fatalf("cooldownFor(%d) = %v exceeds cooldownMax + jitter", failures, cooldown)
		}
	}
}

func TestClientErrorReleasesInFlightSlot(t *testing.T) {
	tracker := New()
	now := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	tracker.now = func() time.Time { return now }

	// Drive the model open with repeated server errors, then let the
	// cooldown expire so it becomes half-open.
	for i := 0; i < tracker.failureThreshold; i++ {
		tracker.Failure("provider/model", "chat", time.Second, 500, "boom", 0)
	}
	now = now.Add(tracker.cooldownMax + time.Minute)

	if !tracker.TryAcquire("provider/model", "chat") {
		t.Fatal("half-open model should accept a single probe")
	}
	if tracker.TryAcquire("provider/model", "chat") {
		t.Fatal("half-open model must not accept a second probe while one is in flight")
	}

	// A client error completes the attempt and must release the slot,
	// otherwise the model would be wedged permanently.
	tracker.Failure("provider/model", "chat", time.Second, 400, "bad request", 0)
	if !tracker.TryAcquire("provider/model", "chat") {
		t.Fatal("client error should release the in-flight slot")
	}
}

func TestAuthErrorReleasesModelInFlightSlot(t *testing.T) {
	tracker := New()
	now := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	tracker.now = func() time.Time { return now }

	for i := 0; i < tracker.failureThreshold; i++ {
		tracker.Failure("provider/model", "chat", time.Second, 500, "boom", 0)
	}
	now = now.Add(tracker.cooldownMax + time.Minute)
	if !tracker.TryAcquire("provider/model", "chat") {
		t.Fatal("half-open model should accept a probe")
	}

	// 401 opens the provider; the model-level in-flight slot must be
	// released too or the model stays wedged after provider recovery.
	tracker.Failure("provider/model", "chat", time.Second, 401, "unauthorized", 0)
	state := tracker.Snapshot()[0]
	if state.LastStatus != 401 {
		t.Fatalf("model should record the auth failure, got last_status=%d", state.LastStatus)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	tracker := New()
	now := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	tracker.now = func() time.Time { return now }

	for i := 0; i < tracker.failureThreshold; i++ {
		tracker.Failure("provider/model", "chat", time.Second, 500, "boom", 0)
	}
	now = now.Add(tracker.cooldownMax + time.Minute)

	if !tracker.TryAcquire("provider/model", "chat") {
		t.Fatal("half-open model should accept a probe")
	}
	tracker.Release("provider/model", "chat")
	tracker.Release("provider/model", "chat")
	if !tracker.TryAcquire("provider/model", "chat") {
		t.Fatal("idempotent Release should free the slot")
	}
}
