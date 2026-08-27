package gateway

import (
	"sort"
	"sync"
	"sync/atomic"

	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/eligibility"
	"github.com/sjzsdu/free-router/internal/health"
	"github.com/sjzsdu/free-router/internal/provider"
	"github.com/sjzsdu/free-router/internal/routing"
)

// CandidatePlanner is a side-effect-free routing policy component except for
// its round-robin counters. Each planning pass reads one immutable eligibility
// snapshot and returns an ordered candidate sequence.
type CandidatePlanner struct {
	catalog     *catalog.Store
	registry    *provider.Registry
	routes      *routing.Store
	tracker     *health.Tracker
	maxAttempts int
	next        atomic.Uint64
	routeNext   sync.Map
}

func NewCandidatePlanner(store *catalog.Store, registry *provider.Registry, routes *routing.Store, tracker *health.Tracker, maxAttempts int) *CandidatePlanner {
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	return &CandidatePlanner{catalog: store, registry: registry, routes: routes, tracker: tracker, maxAttempts: maxAttempts}
}

func (p *CandidatePlanner) snapshot() eligibility.Snapshot {
	return eligibility.New(p.catalog, p.routes, p.tracker)
}

func (p *CandidatePlanner) RouteAvailable(route routing.Route) bool {
	view := p.snapshot()
	return p.routeAvailable(view, route)
}

func (p *CandidatePlanner) routeAvailable(view eligibility.Snapshot, route routing.Route) bool {
	for _, model := range view.Models() {
		if _, reason := view.Eligible(model, route, true, true); reason == "" {
			return true
		}
	}
	return false
}

func (p *CandidatePlanner) Candidates(requested, capability string, needsTools bool) ([]catalog.Model, bool) {
	return p.CandidatesSeeded(requested, capability, needsTools, 0)
}

// CandidatesSeeded behaves like Candidates but rotates round-robin groups
// starting from seed instead of the global counter. A non-zero seed keeps
// all requests in the same conversation on the same primary model, which is
// essential for multi-turn tool-call sessions: OpenAI tool_calls/tool_results
// must be interpreted by the same model across the dialog, otherwise schema
// mismatches and 4xx errors multiply and collapse the chat-tools pool.
func (p *CandidatePlanner) CandidatesSeeded(requested, capability string, needsTools bool, seed uint64) ([]catalog.Model, bool) {
	view := p.snapshot()
	if route, ok := view.Route(requested); ok {
		effectiveRoute := route
		effectiveRoute.RequireTool = effectiveRoute.RequireTool || needsTools
		if len(route.Models) > 0 {
			priority := make([]catalog.Model, 0, len(route.Models))
			configured := make(map[string]bool, len(route.Models))
			for _, id := range route.Models {
				configured[id] = true
				model, ok := view.Find(id)
				if !ok {
					continue
				}
				if effective, reason := view.Eligible(model, effectiveRoute, false, false); reason == "" {
					priority = append(priority, effective)
				}
			}
			result := p.strictlyAvailable(view, priority, effectiveRoute.Capability)
			if route.Strategy == routing.StrategyRoundRobin {
				if seed != 0 {
					result = rotateCandidates(result, seed)
				} else {
					result = rotateCandidates(result, p.nextForRoute(requested))
				}
			}
			if remaining, ok := p.pickRemaining(view, effectiveRoute, configured, true); ok {
				result = append(result, remaining...)
			}
			return result, true
		}
		return p.dynamicCandidatesSeeded(view, effectiveRoute, false, seed), true
	}
	if model, ok := view.Find(requested); ok {
		route := routing.Route{Capability: capability, RequireTool: needsTools}
		if effective, reason := view.Eligible(model, route, false, false); reason == "" {
			return []catalog.Model{effective}, false
		}
	}
	return nil, false
}

func (p *CandidatePlanner) RequestCapability(requested, fallback string) string {
	if route, ok := p.snapshot().Route(requested); ok {
		return route.Capability
	}
	return fallback
}

func (p *CandidatePlanner) nextForRoute(alias string) uint64 {
	counter, _ := p.routeNext.LoadOrStore(alias, &atomic.Uint64{})
	return counter.(*atomic.Uint64).Add(1) - 1
}

func (p *CandidatePlanner) dynamicCandidates(view eligibility.Snapshot, route routing.Route, needsTools bool) []catalog.Model {
	return p.dynamicCandidatesSeeded(view, route, needsTools, 0)
}

// dynamicCandidatesSeeded picks providers in round-robin order starting from
// seed. A non-zero seed ties all requests sharing that seed to the same
// starting provider/model so a multi-turn tool conversation keeps using the
// same upstream model instead of bouncing between providers.
func (p *CandidatePlanner) dynamicCandidatesSeeded(view eligibility.Snapshot, route routing.Route, needsTools bool, seed uint64) []catalog.Model {
	route.RequireTool = route.RequireTool || needsTools
	groups := make(map[string][]catalog.Model)
	var preferred *catalog.Model
	for _, model := range view.Models() {
		effective, reason := view.Eligible(model, route, true, false)
		if reason != "" {
			continue
		}
		if effective.Provider == "openrouter" && effective.UpstreamID == "openrouter/free" {
			copy := effective
			preferred = &copy
			continue
		}
		groups[effective.Provider] = append(groups[effective.Provider], effective)
	}
	providerIDs := make([]string, 0, len(groups))
	for id := range groups {
		providerIDs = append(providerIDs, id)
	}
	sort.Strings(providerIDs)
	if len(providerIDs) == 0 && preferred == nil {
		return nil
	}
	result := make([]catalog.Model, 0)
	if preferred != nil {
		result = append(result, *preferred)
	}
	if len(providerIDs) == 0 || len(result) == p.maxAttempts {
		return p.limitCandidates(p.availableCandidates(view, result, route.Capability))
	}
	if seed == 0 {
		seed = p.next.Add(1) - 1
	}
	// Keep the rotation arithmetic in uint64: a session seed is an FNV-64
	// hash and can exceed MaxInt64, which would make int(seed) negative and
	// produce negative modulo indices (panic: index out of range [-1]).
	numProviders := uint64(len(providerIDs))
	for round := 0; ; round++ {
		added := false
		for offset := range providerIDs {
			providerID := providerIDs[(seed+uint64(offset))%numProviders]
			models := groups[providerID]
			if round >= len(models) {
				continue
			}
			result = append(result, models[(seed+uint64(round))%uint64(len(models))])
			added = true
		}
		if !added {
			break
		}
	}
	return p.limitCandidates(p.availableCandidates(view, result, route.Capability))
}

// selectable gates a model on its health state. For chat-tools the gate is
// Availability (degraded/half-open still selectable) instead of strict
// health, so a single 429 does not collapse the entire tool candidate pool
// to zero and force a 503/transparent passthrough on the next request.
func (p *CandidatePlanner) selectable(view eligibility.Snapshot, model catalog.Model, capability string) bool {
	if !view.Verified(model, capability) {
		return false
	}
	if capability == catalog.FunctionChatTools {
		return view.Available(model.ID, capability)
	}
	return view.Healthy(model.ID, capability)
}

func (p *CandidatePlanner) strictlyAvailable(view eligibility.Snapshot, models []catalog.Model, capability string) []catalog.Model {
	result := make([]catalog.Model, 0, len(models))
	for _, model := range models {
		if p.selectable(view, model, capability) {
			result = append(result, model)
		}
	}
	return result
}

// pickRemaining now returns the full set of eligible non-excluded models
// instead of a single round-robin pick. The priority array alone is too
// short for chat-tools (often one or two models); padding it with every
// remaining available model gives the gateway enough fallback depth to
// absorb a 429 without bouncing straight to writeCandidateUnavailable and
// leaking the upstream message.
func (p *CandidatePlanner) pickRemaining(view eligibility.Snapshot, route routing.Route, excluded map[string]bool, healthyOnly bool) ([]catalog.Model, bool) {
	models := make([]catalog.Model, 0)
	for _, model := range view.Models() {
		if excluded[model.ID] {
			continue
		}
		effective, reason := view.Eligible(model, route, healthyOnly, healthyOnly)
		if reason == "" {
			models = append(models, effective)
		}
	}
	if len(models) == 0 {
		return nil, false
	}
	seed := p.next.Add(1) - 1
	return rotateCandidates(models, seed), true
}

func (p *CandidatePlanner) availableCandidates(view eligibility.Snapshot, models []catalog.Model, capability string) []catalog.Model {
	available := make([]catalog.Model, 0, len(models))
	for _, model := range models {
		if p.selectable(view, model, capability) {
			available = append(available, model)
		}
	}
	return available
}

func (p *CandidatePlanner) limitCandidates(models []catalog.Model) []catalog.Model {
	if len(models) > p.maxAttempts {
		return models[:p.maxAttempts]
	}
	return models
}
