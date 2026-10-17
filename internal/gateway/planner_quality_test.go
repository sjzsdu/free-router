package gateway

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/health"
	"github.com/sjzsdu/free-router/internal/provider"
	"github.com/sjzsdu/free-router/internal/routing"
)

func TestSortByQualityPutsScoredModelsFirst(t *testing.T) {
	models := []catalog.Model{
		{ID: "a/unscored"},
		{ID: "b/low", Quality: &catalog.Quality{Source: "lmarena", Score: 40}},
		{ID: "c/high-tie-rank-2", Quality: &catalog.Quality{Source: "lmarena", Score: 90, Rank: 2}},
		{ID: "d/high-tie-rank-1", Quality: &catalog.Quality{Source: "lmarena", Score: 90, Rank: 1}},
		{ID: "e/zero", Quality: &catalog.Quality{Source: "lmarena", Score: 0}},
	}
	got := make([]string, 0, len(models))
	for _, model := range sortByQuality(models) {
		got = append(got, model.ID)
	}
	want := []string{"d/high-tie-rank-1", "c/high-tie-rank-2", "b/low", "a/unscored", "e/zero"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// TestBestQualityStrategyReordersCandidates covers the whole path: manifest
// quality -> runtime catalog -> planner ordering. Unscored models must stay in
// the pool behind the scored ones rather than disappearing.
func TestBestQualityStrategyReordersCandidates(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "free-models.json")
	manifest := `{"schema_version":2,"providers":{
		"alpha":{"source_urls":["https://example.com/alpha"],"models":[{"id":"a","functions":["chat"],"quality":{"source":"lmarena","score":51.5,"observed_at":"2026-10-01"}}]},
		"beta":{"source_urls":["https://example.com/beta"],"models":[{"id":"b","functions":["chat"],"quality":{"source":"lmarena","score":83,"observed_at":"2026-10-01"}}]},
		"gamma":{"source_urls":["https://example.com/gamma"],"models":[{"id":"c","functions":["chat"]}]}
	}}`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := provider.NewRegistryWithManifest(
		`[{"id":"alpha","base_url":"https://alpha.invalid","no_auth":true},{"id":"beta","base_url":"https://beta.invalid","no_auth":true},{"id":"gamma","base_url":"https://gamma.invalid","no_auth":true}]`,
		provider.DefaultEnvMap(), manifestPath,
	)
	if err != nil {
		t.Fatal(err)
	}
	store := catalog.New(registry, filepath.Join(dir, "models.json"), http.DefaultClient)
	if err := store.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	routes, err := routing.New(filepath.Join(dir, "routes.json"))
	if err != nil {
		t.Fatal(err)
	}
	config := routes.Config()
	route := config.Routes[catalog.FunctionChat]
	// Deliberately hand-ordered from worst to best: the strategy is the only
	// thing that may reorder them.
	route.Models = []string{"alpha/a", "gamma/c", "beta/b"}
	route.Strategy = routing.StrategyBestQuality
	config.Routes[catalog.FunctionChat] = route
	if err := routes.Update(config); err != nil {
		t.Fatal(err)
	}
	tracker := health.New()
	for _, id := range route.Models {
		if err := store.RecordCapabilityVerification(id, catalog.FunctionChat, time.Now(), time.Millisecond); err != nil {
			t.Fatal(err)
		}
		tracker.RestoreProbeSuccess(id, catalog.FunctionChat, time.Now(), 1)
	}
	planner := NewCandidatePlanner(store, registry, routes, tracker, 3)

	models, routed := planner.Candidates(catalog.FunctionChat, catalog.FunctionChat, false)
	got := make([]string, 0, len(models))
	for _, model := range models {
		got = append(got, model.ID)
	}
	want := []string{"beta/b", "alpha/a", "gamma/c"}
	if !routed || !reflect.DeepEqual(got, want) {
		t.Fatalf("routed=%v candidates=%v want=%v", routed, got, want)
	}
}

// TestOrderedStrategyKeepsHandWrittenOrder guards the fallback contract: a
// scored model must not jump ahead of the user's order unless the route opts
// into best-quality.
func TestOrderedStrategyKeepsHandWrittenOrder(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "free-models.json")
	manifest := `{"schema_version":2,"providers":{
		"alpha":{"source_urls":["https://example.com/alpha"],"models":[{"id":"a","functions":["chat"],"quality":{"source":"lmarena","score":10}}]},
		"beta":{"source_urls":["https://example.com/beta"],"models":[{"id":"b","functions":["chat"],"quality":{"source":"lmarena","score":99}}]}
	}}`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := provider.NewRegistryWithManifest(
		`[{"id":"alpha","base_url":"https://alpha.invalid","no_auth":true},{"id":"beta","base_url":"https://beta.invalid","no_auth":true}]`,
		provider.DefaultEnvMap(), manifestPath,
	)
	if err != nil {
		t.Fatal(err)
	}
	store := catalog.New(registry, filepath.Join(dir, "models.json"), http.DefaultClient)
	if err := store.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	routes, err := routing.New(filepath.Join(dir, "routes.json"))
	if err != nil {
		t.Fatal(err)
	}
	config := routes.Config()
	route := config.Routes[catalog.FunctionChat]
	route.Models = []string{"alpha/a", "beta/b"}
	route.Strategy = routing.StrategyOrdered
	config.Routes[catalog.FunctionChat] = route
	if err := routes.Update(config); err != nil {
		t.Fatal(err)
	}
	tracker := health.New()
	for _, id := range route.Models {
		if err := store.RecordCapabilityVerification(id, catalog.FunctionChat, time.Now(), time.Millisecond); err != nil {
			t.Fatal(err)
		}
		tracker.RestoreProbeSuccess(id, catalog.FunctionChat, time.Now(), 1)
	}
	planner := NewCandidatePlanner(store, registry, routes, tracker, 3)

	models, _ := planner.Candidates(catalog.FunctionChat, catalog.FunctionChat, false)
	got := make([]string, 0, len(models))
	for _, model := range models {
		got = append(got, model.ID)
	}
	if want := []string{"alpha/a", "beta/b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates=%v want=%v", got, want)
	}
}
