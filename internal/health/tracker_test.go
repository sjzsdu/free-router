package health

import (
	"testing"
	"time"
)

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

func TestProbeFailureWithClientErrorKeepsHealthy(t *testing.T) {
	tracker := New()
	tracker.ProbeFailure("provider/model", "chat", time.Second, 400, "client error")
	if !tracker.Available("provider/model", "chat") {
		t.Fatal("client error probe should not quarantine model")
	}
	state := tracker.Snapshot()[0]
	if state.Status != StatusHealthy {
		t.Fatalf("client error probe set status to %q, want %q", state.Status, StatusHealthy)
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
		{403, ErrorAuth},
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
