package eligibility

import (
	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/health"
	"github.com/sjzsdu/free-router/internal/routing"
)

const (
	ReasonProviderNotConfigured = "provider-not-configured"
	ReasonDisabled              = "disabled"
	ReasonCapabilityMismatch    = "capability-mismatch"
	ReasonUnverified            = "unverified"
	ReasonUnhealthy             = "unhealthy"
)

// Snapshot is the shared, immutable projection used by routing and management
// views to answer whether a model is currently selectable and why.
type Snapshot struct {
	catalog        catalog.Snapshot
	config         routing.Config
	applyOverrides bool
	providers      map[string]bool
	health         map[string]health.State
	providerHealth map[string]health.State
}

func New(models *catalog.Store, routes *routing.Store, tracker *health.Tracker) Snapshot {
	config := routing.DefaultConfig()
	applyOverrides := routes != nil
	if routes != nil {
		config = routes.Config()
	}
	providers := make(map[string]bool)
	modelSnapshot := models.Snapshot()
	for _, model := range modelSnapshot.Models() {
		providers[model.Provider] = models.ProviderConfigured(model.Provider)
	}
	states := make(map[string]health.State)
	providerStates := make(map[string]health.State)
	if tracker != nil {
		for _, state := range tracker.Snapshot() {
			states[key(state.Model, state.Capability)] = state
		}
		for _, state := range tracker.ProviderSnapshot() {
			providerStates[state.Model] = state
		}
	}
	return Snapshot{
		catalog: modelSnapshot, config: config, applyOverrides: applyOverrides,
		providers: providers, health: states, providerHealth: providerStates,
	}
}

func (s Snapshot) Models() []catalog.Model { return s.catalog.Models() }

func (s Snapshot) Find(id string) (catalog.Model, bool) { return s.catalog.Find(id) }

func (s Snapshot) Route(alias string) (routing.Route, bool) {
	route, ok := s.config.Routes[alias]
	route.Models = append([]string(nil), route.Models...)
	return route, ok
}

func (s Snapshot) Apply(model catalog.Model) (catalog.Model, bool) {
	if !s.applyOverrides {
		return model, true
	}
	return routing.Apply(s.config, model)
}

func (s Snapshot) Configured(providerID string) bool { return s.providers[providerID] }

func (s Snapshot) Verified(model catalog.Model, capability string) bool {
	return s.catalog.CapabilityVerified(model, capability)
}

func (s Snapshot) Healthy(modelID, capability string) bool {
	providerID := ""
	if model, ok := s.catalog.Find(modelID); ok {
		providerID = model.Provider
	}
	if state, ok := s.providerHealth[providerID]; ok && state.Status != health.StatusHealthy {
		return false
	}
	state, ok := s.health[key(modelID, capability)]
	return ok && state.Verified && state.Status == health.StatusHealthy
}

// Eligible applies all effective routing rules in one place. The empty reason
// means the model is selectable.
func (s Snapshot) Eligible(model catalog.Model, route routing.Route, requireVerified, requireHealthy bool) (catalog.Model, string) {
	if !s.Configured(model.Provider) {
		return model, ReasonProviderNotConfigured
	}
	effective, enabled := s.Apply(model)
	if !enabled {
		return effective, ReasonDisabled
	}
	if !routing.Accepts(route, effective) {
		return effective, ReasonCapabilityMismatch
	}
	if requireVerified && !s.Verified(effective, route.Capability) {
		return effective, ReasonUnverified
	}
	if requireHealthy && !s.Healthy(effective.ID, route.Capability) {
		return effective, ReasonUnhealthy
	}
	return effective, ""
}

func key(model, capability string) string { return model + "\x00" + capability }
