package health

import (
	"testing"
	"time"
)

func TestTrackerQuarantinesAndRecoversModel(t *testing.T) {
	tracker := New()
	now := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	tracker.now = func() time.Time { return now }
	tracker.Failure("provider/model", time.Second, 429, "rate limited", time.Minute)
	if tracker.Available("provider/model") {
		t.Fatal("failed model must be quarantined")
	}
	now = now.Add(2 * time.Minute)
	if tracker.Available("provider/model") {
		t.Fatal("failed model must remain quarantined until recovery")
	}
	tracker.Success("provider/model", 100*time.Millisecond, 200)
	state := tracker.Snapshot()[0]
	if state.Status != "healthy" || state.ConsecutiveFailures != 0 || state.Successes != 1 {
		t.Fatalf("state = %#v", state)
	}
}

func TestResetReturnsFailedModelToUnknownState(t *testing.T) {
	tracker := New()
	tracker.Failure("provider/model", 0, 500, "upstream failed", 0)
	tracker.Reset("provider/model")
	if !tracker.Available("provider/model") || len(tracker.Snapshot()) != 0 {
		t.Fatal("reset model must be eligible for a new probe")
	}
}

func TestTrackerQuarantinesAfterFirstNetworkFailure(t *testing.T) {
	tracker := New()
	tracker.Failure("provider/model", 0, 0, "network", 0)
	if tracker.Available("provider/model") {
		t.Fatal("first network failure must quarantine the model")
	}
	state := tracker.Snapshot()[0]
	if state.Status != "failed" || tracker.Summary().Failed != 1 {
		t.Fatalf("state = %#v summary = %#v", state, tracker.Summary())
	}
}

func TestProbeHealthUsesTTLWithoutInflatingRequests(t *testing.T) {
	tracker := New()
	now := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	tracker.now = func() time.Time { return now }
	tracker.ProbeSuccess("provider/model", 25*time.Millisecond)
	state := tracker.Snapshot()[0]
	if state.Status != "healthy" || state.Checks != 1 || state.Requests != 0 || state.LastCheckLatencyMS != 25 {
		t.Fatalf("state = %#v", state)
	}
	if tracker.ProbeDue("provider/model", 24*time.Hour) {
		t.Fatal("fresh probe must be cached")
	}
	now = now.Add(24 * time.Hour)
	if !tracker.ProbeDue("provider/model", 24*time.Hour) {
		t.Fatal("probe must expire after 24 hours")
	}
	tracker.ProbeFailure("provider/model", time.Second, 429, "rate limited")
	if tracker.Available("provider/model") || tracker.Snapshot()[0].Status != "failed" {
		t.Fatal("failed probe must quarantine model")
	}
}
