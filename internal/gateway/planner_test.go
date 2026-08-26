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

func TestCandidatePlannerProducesStableSequencesFromSnapshot(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "free-models.json")
	manifest := `{"schema_version":2,"providers":{"alpha":{"source_urls":["https://example.com/alpha"],"models":[{"id":"a","functions":["chat"]}]},"beta":{"source_urls":["https://example.com/beta"],"models":[{"id":"b","functions":["chat"]}]}}}`
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
	route.Models = []string{"beta/b", "alpha/a"}
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

	assertIDs := func(want []string) {
		t.Helper()
		models, routed := planner.Candidates(catalog.FunctionChat, catalog.FunctionChat, false)
		got := make([]string, 0, len(models))
		for _, model := range models {
			got = append(got, model.ID)
		}
		if !routed || !reflect.DeepEqual(got, want) {
			t.Fatalf("routed=%v candidates=%v want=%v", routed, got, want)
		}
	}
	assertIDs([]string{"beta/b", "alpha/a"})

	config = routes.Config()
	config.Models["beta/b"] = routing.ModelOverride{Disabled: true}
	if err := routes.Update(config); err != nil {
		t.Fatal(err)
	}
	assertIDs([]string{"alpha/a"})
}
