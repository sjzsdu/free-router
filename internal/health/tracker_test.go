package health

import (
	"testing"
	"time"
)

func TestTrackerCoolsAndRecoversModel(t *testing.T) {
	tracker := New()
	now := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	tracker.now = func() time.Time { return now }
	tracker.Failure("provider/model", time.Second, 429, "rate limited", time.Minute)
	if tracker.Available("provider/model") {
		t.Fatal("rate-limited model must cool down")
	}
	now = now.Add(2 * time.Minute)
	if !tracker.Available("provider/model") {
		t.Fatal("model did not leave cooldown")
	}
	tracker.Success("provider/model", 100*time.Millisecond, 200)
	state := tracker.Snapshot()[0]
	if state.Status != "healthy" || state.ConsecutiveFailures != 0 || state.Successes != 1 {
		t.Fatalf("state = %#v", state)
	}
}

func TestTrackerCoolsAfterRepeatedNetworkFailure(t *testing.T) {
	tracker := New()
	tracker.Failure("provider/model", 0, 0, "network", 0)
	if !tracker.Available("provider/model") {
		t.Fatal("first network failure should only degrade")
	}
	tracker.Failure("provider/model", 0, 0, "network", 0)
	if tracker.Available("provider/model") {
		t.Fatal("repeated network failure should cool down")
	}
}
