package statistics

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestStoreAggregatesAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "statistics.json")
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC)
	if err := store.Record(Attempt{Model: "test/chat", Provider: "test", Capability: "chat", Success: true, StatusCode: 200, Latency: 120 * time.Millisecond, Usage: &Usage{InputTokens: 10, OutputTokens: 4, TotalTokens: 14}, At: at}); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(Attempt{Model: "test/chat", Provider: "test", Capability: "chat", Success: true, StatusCode: 200, Latency: 80 * time.Millisecond, MissingUsage: true, At: at.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(Attempt{Model: "test/chat", Provider: "test", Capability: "chat", Success: false, StatusCode: 429, Latency: 100 * time.Millisecond, At: at.Add(2 * time.Second)}); err != nil {
		t.Fatal(err)
	}

	reloaded, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := reloaded.Snapshot()
	if len(snapshot.Models) != 1 {
		t.Fatalf("models=%d", len(snapshot.Models))
	}
	got := snapshot.Models[0]
	if got.Requests != 3 || got.Successes != 2 || got.Failures != 1 || got.SuccessRate != 2.0/3.0 {
		t.Fatalf("quality aggregate=%+v", got)
	}
	if got.InputTokens != 10 || got.OutputTokens != 4 || got.TotalTokens != 14 || got.UsageReported != 1 || got.UsageMissing != 1 {
		t.Fatalf("usage aggregate=%+v", got)
	}
	if got.AverageLatencyMS != 100 {
		t.Fatalf("average latency=%v", got.AverageLatencyMS)
	}
}

func TestStoreSerializesConcurrentRecords(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "statistics.json"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := store.Record(Attempt{Model: "test/chat", Provider: "test", Capability: "chat", Success: true, Usage: &Usage{InputTokens: 1}}); err != nil {
				t.Errorf("record: %v", err)
			}
		}()
	}
	wg.Wait()
	got := store.Snapshot().Models[0]
	if got.Requests != 20 || got.InputTokens != 20 {
		t.Fatalf("aggregate=%+v", got)
	}
}
