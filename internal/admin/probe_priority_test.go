package admin

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/credentials"
	"github.com/sjzsdu/free-router/internal/health"
	"github.com/sjzsdu/free-router/internal/provider"
	"github.com/sjzsdu/free-router/internal/routing"
)

// probeJobFor builds a probe target without a full catalog round-trip.
func probeJobFor(providerID, modelID, capability string) probeJob {
	return probeJob{
		Model:      catalog.Model{ID: providerID + "/" + modelID, Provider: providerID, UpstreamID: modelID, Functions: []string{capability}},
		Capability: capability,
	}
}

// A cold start has nothing measured yet, so the probe budget should establish
// the capability that is hardest to obtain and that agent frameworks need
// first.
func TestSortProbeJobsPromotesChatTools(t *testing.T) {
	jobs := []probeJob{
		probeJobFor("a", "m1", catalog.FunctionChat),
		probeJobFor("a", "m1", catalog.FunctionChatTools),
		probeJobFor("a", "m2", catalog.FunctionChat),
		probeJobFor("a", "m2", catalog.FunctionChatTools),
		probeJobFor("b", "m1", catalog.FunctionChat),
		probeJobFor("b", "m1", catalog.FunctionChatTools),
		probeJobFor("b", "m2", catalog.FunctionChat),
		probeJobFor("b", "m2", catalog.FunctionChatTools),
	}

	promoted := sortProbeJobs(jobs, true)
	tools := 0
	for _, job := range promoted {
		if job.Capability == catalog.FunctionChatTools {
			tools++
		}
	}
	if tools != 4 {
		t.Fatalf("promoted list lost chat-tools jobs: %d of 4", tools)
	}
	for index := 0; index < tools; index++ {
		if promoted[index].Capability != catalog.FunctionChatTools {
			t.Fatalf("job %d is %q, want chat-tools first: %#v", index, promoted[index].Capability, promoted)
		}
	}

	// Provider interleaving must survive the promotion: the first two promoted
	// jobs have to come from different providers, otherwise a bounded round
	// would drain one free source.
	if promoted[0].Model.Provider == promoted[1].Model.Provider {
		t.Fatalf("promotion collapsed the provider interleaving: %#v", promoted[:tools])
	}

	// Without the switch the historical order is untouched.
	plain := sortProbeJobs(jobs, false)
	if plain[0].Capability == catalog.FunctionChatTools && plain[1].Capability == catalog.FunctionChatTools {
		t.Fatalf("promotion applied while disabled: %#v", plain[:4])
	}
}

// newToolAwareHandler builds a handler over one or more fake providers whose
// single model advertises tools, which is the only way a discovered model gains
// the chat-tools capability.
func newToolAwareHandler(t *testing.T, providers int) *Handler {
	t.Helper()
	for _, key := range provider.SupportedKeyEnvs() {
		t.Setenv(key, "")
	}
	entries := make([]string, 0, providers)
	var client *http.Client
	for index := 0; index < providers; index++ {
		modelID := fmt.Sprintf("tool-%c", 'a'+index)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/models") {
				fmt.Fprintf(w, `{"data":[{"id":%q,"supported_parameters":["tools"]}]}`, modelID)
				return
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
		}))
		t.Cleanup(server.Close)
		if client == nil {
			client = server.Client()
		}
		entries = append(entries, fmt.Sprintf(`{"id":"p%c","base_url":%q,"no_auth":true}`, 'a'+index, server.URL))
	}
	registry, err := provider.NewRegistry("[" + strings.Join(entries, ",") + "]")
	if err != nil {
		t.Fatal(err)
	}
	models := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), client)
	discoverModelsForTest(t, models)
	routes, err := routing.New(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	return New(routes, models, credentials.NewFileOnly(filepath.Join(t.TempDir(), "credentials.json")), health.New(), Config{}, nil)
}

// The decision is now taken from the answer the probe pass already computed;
// it must not look at anything else.
func TestPrioritizeColdStartToolsUsesRoundResult(t *testing.T) {
	enabled := routing.HeartbeatSettings{PrioritizeChatTools: true}
	if !prioritizeColdStartTools(enabled, false) {
		t.Fatal("cold start did not prioritise chat-tools")
	}
	if prioritizeColdStartTools(enabled, true) {
		t.Fatal("a verified tool target still prioritised the cold-start order")
	}

	disabled := routing.HeartbeatSettings{PrioritizeChatTools: false}
	if prioritizeColdStartTools(disabled, false) {
		t.Fatal("disabled switch did not turn the priority off")
	}
}

// The behaviour behind that decision: once one tool target is verified the probe
// order returns to the historical one, without any cache to expire.
func TestProbePriorityEndsAfterFirstVerifiedToolModel(t *testing.T) {
	t.Setenv(heartbeatEnvPrioritizeTools, "")
	handler := newToolAwareHandler(t, 2)

	jobs, _ := probeCandidates(handler, false, nil)
	if len(jobs) == 0 || jobs[0].Capability != catalog.FunctionChatTools {
		t.Fatalf("cold start did not lead with chat-tools: %#v", jobs)
	}

	if err := handler.catalog.RecordCapabilityVerification("pa/tool-a", catalog.FunctionChatTools, time.Now(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	jobs, _ = probeCandidates(handler, false, nil)
	if len(jobs) == 0 {
		t.Fatal("no probe candidates after verification")
	}
	if jobs[0].Capability != catalog.FunctionChat {
		t.Fatalf("verification did not end the cold-start priority: %#v", jobs)
	}
}

// The round the heartbeat actually runs must start with chat-tools while the
// pool is cold, and the promoted prefix must still alternate providers.
func TestHeartbeatColdStartPromotesChatToolsAcrossProviders(t *testing.T) {
	handler := newToolAwareHandler(t, 2)
	settings := routing.HeartbeatSettings{Enabled: true, IntervalMinutes: 30, RecoveryMinutes: 10, MaxProbesPerRound: 8, Concurrency: 2, PrioritizeChatTools: true}
	jobs, _, _ := handler.heartbeat.selectJobs(handler, settings, time.Now())
	if len(jobs) < 4 {
		t.Fatalf("cold start produced %d probe jobs, want 4: %#v", len(jobs), jobs)
	}
	for index := 0; index < 2; index++ {
		if jobs[index].Capability != catalog.FunctionChatTools {
			t.Fatalf("job %d is %q, want chat-tools first: %#v", index, jobs[index].Capability, jobs)
		}
	}
	if jobs[0].Model.Provider == jobs[1].Model.Provider {
		t.Fatalf("promotion collapsed the provider interleaving: %#v", jobs)
	}
}

// Turning the priority off through the environment must be visible in the
// resolved settings the heartbeat uses.
func TestHeartbeatSettingsPrioritizeChatToolsEnv(t *testing.T) {
	t.Setenv(heartbeatEnvPrioritizeTools, "")
	settings, problems := heartbeatSettings(nil)
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %#v", problems)
	}
	if !settings.PrioritizeChatTools {
		t.Fatal("prioritisation must default to on")
	}

	t.Setenv(heartbeatEnvPrioritizeTools, "false")
	settings, problems = heartbeatSettings(nil)
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %#v", problems)
	}
	if settings.PrioritizeChatTools {
		t.Fatal("environment override did not disable prioritisation")
	}

	t.Setenv(heartbeatEnvPrioritizeTools, "definitely-not-a-bool")
	if _, problems = heartbeatSettings(nil); len(problems) == 0 {
		t.Fatal("an invalid boolean was silently accepted")
	}
}

// probeCandidates is the manual "probe all" path; a cold start must order it the
// same way the heartbeat does, and the historical order must return once the
// switch is off.
func TestProbeCandidatesPromoteChatToolsOnColdStart(t *testing.T) {
	t.Setenv(heartbeatEnvPrioritizeTools, "")
	handler := newToolAwareHandler(t, 2)
	jobs, _ := probeCandidates(handler, false, nil)
	if len(jobs) == 0 {
		t.Fatal("no probe candidates")
	}
	seenOther := false
	for _, job := range jobs {
		if job.Capability == catalog.FunctionChatTools {
			if seenOther {
				t.Fatalf("chat-tools target appeared after a plain one: %#v", jobs)
			}
			continue
		}
		seenOther = true
	}

	t.Setenv(heartbeatEnvPrioritizeTools, "false")
	disabled := newToolAwareHandler(t, 2)
	jobs, _ = probeCandidates(disabled, false, nil)
	if len(jobs) < 2 {
		t.Fatalf("no probe candidates with the switch off: %#v", jobs)
	}
	if jobs[0].Capability == catalog.FunctionChatTools && jobs[1].Capability == catalog.FunctionChatTools {
		t.Fatalf("promotion applied while disabled: %#v", jobs)
	}
}
