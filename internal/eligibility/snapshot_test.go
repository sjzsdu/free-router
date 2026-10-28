package eligibility

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/health"
	"github.com/sjzsdu/free-router/internal/provider"
	"github.com/sjzsdu/free-router/internal/routing"
)

func testSnapshot(t *testing.T) (Snapshot, *catalog.Store, *health.Tracker, *routing.Store) {
	t.Helper()
	for _, key := range provider.SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
				{"id": "tool-chat", "supported_parameters": []string{"tools"}},
				{"id": "plain-chat"},
			}})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(upstream.Close)

	registry, err := provider.NewRegistry(`[{"id":"test","base_url":"` + upstream.URL + `","no_auth":true}]`)
	if err != nil {
		t.Fatal(err)
	}
	store := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), upstream.Client())
	models, failures := store.DiscoverFromProviders(context.Background())
	if len(failures) > 0 || len(models) == 0 {
		t.Fatalf("models=%#v failures=%#v", models, failures)
	}
	for _, model := range models {
		for _, capability := range model.Functions {
			if err := store.RecordCapabilityVerification(model.ID, capability, time.Now(), time.Millisecond); err != nil {
				t.Fatal(err)
			}
		}
	}
	routes, err := routing.New(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	tracker := health.New()
	for _, verification := range store.CapabilityVerifications() {
		tracker.RestoreProbeSuccess(verification.Model, verification.Capability, verification.CheckedAt, verification.LatencyMS)
	}
	return New(store, routes, tracker), store, tracker, routes
}

// routeState relies on EligibleEffective to avoid projecting each model twice.
// The two entry points must stay interchangeable, otherwise the model list and
// the routing planner would disagree about the same model.
func TestEligibleEffectiveMatchesEligible(t *testing.T) {
	view, store, tracker, routes := testSnapshot(t)
	config := routes.Config()

	// Cover every reason code, not only the happy path.
	for range health.DefaultFailureThreshold {
		tracker.Failure("test/plain-chat", catalog.FunctionChat, 0, http.StatusBadGateway, "broken", 0)
	}
	disabled := true
	config.Models["test/tool-chat"] = routing.ModelOverride{Disabled: disabled}
	if err := routes.Update(config); err != nil {
		t.Fatal(err)
	}
	// Re-snapshot so the failure state and the override above are visible.
	view = New(store, routes, tracker)

	checked := 0
	for _, alias := range catalog.AllFunctions() {
		route, ok := view.Route(alias)
		if !ok {
			continue
		}
		for _, model := range view.Models() {
			viaEligible, eligibleReason := view.Eligible(model, route, true, true)
			effective, enabled := view.Apply(model)
			if !enabled {
				// A disabled model never reaches EligibleEffective, which is the
				// only place the two paths legitimately differ.
				if eligibleReason != ReasonDisabled {
					t.Fatalf("%s/%s: disabled model reported %q", alias, model.ID, eligibleReason)
				}
				continue
			}
			viaEffective, effectiveReason := view.EligibleEffective(effective, route, true, true)
			if eligibleReason != effectiveReason {
				t.Fatalf("%s/%s: reason %q vs %q", alias, model.ID, eligibleReason, effectiveReason)
			}
			// Compare the whole projection, not a hand-picked subset: if Eligible
			// ever starts returning a deeper copy (or a different field is
			// populated on one path only) this has to fail. Model carries slices,
			// so the comparison goes through reflect rather than ==.
			if !reflect.DeepEqual(viaEligible, viaEffective) {
				t.Fatalf("%s/%s: projections differ\n eligible  = %+v\n effective = %+v", alias, model.ID, viaEligible, viaEffective)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no model/route pair was compared")
	}
}
