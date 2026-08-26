package admin

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/credentials"
	"github.com/sjzsdu/free-router/internal/health"
	"github.com/sjzsdu/free-router/internal/routing"
)

type ServiceError struct {
	Stage string
	Err   error
}

func (e *ServiceError) Error() string { return fmt.Sprintf("%s: %v", e.Stage, e.Err) }
func (e *ServiceError) Unwrap() error { return e.Err }

type ConfigUpdateResult struct {
	Saved  bool           `json:"saved"`
	Config routing.Config `json:"config"`
}

type ConfigService struct {
	mu           *sync.Mutex
	routes       *routing.Store
	catalog      *catalog.Store
	health       *health.Tracker
	reload       func(map[string][]string) (func(), error)
	refreshAsync func()
}

func (s *ConfigService) Update(config routing.Config) (ConfigUpdateResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var previous routing.Config
	var rollback func()
	validate := func(current, next routing.Config) error {
		previous = current
		if reflect.DeepEqual(current.ProviderEnv, next.ProviderEnv) || s.reload == nil {
			return nil
		}
		var err error
		rollback, err = s.reload(next.ProviderEnv)
		return err
	}
	if err := s.routes.UpdateTransactional(config, validate); err != nil {
		if rollback != nil {
			rollback()
		}
		return ConfigUpdateResult{}, &ServiceError{Stage: "update-config", Err: err}
	}
	current := s.routes.Config()
	if !reflect.DeepEqual(previous.ProviderEnv, current.ProviderEnv) && s.refreshAsync != nil {
		s.refreshAsync()
	}
	changed := make(map[string]bool)
	for id, before := range previous.Models {
		if after, ok := current.Models[id]; !ok || !reflect.DeepEqual(before, after) {
			changed[id] = true
		}
	}
	for id, after := range current.Models {
		if before, ok := previous.Models[id]; !ok || !reflect.DeepEqual(before, after) {
			changed[id] = true
		}
	}
	for id := range changed {
		if err := s.catalog.ResetCapabilityVerification(id, ""); err != nil {
			return ConfigUpdateResult{}, &ServiceError{Stage: "reset-model-verification", Err: err}
		}
		s.health.Reset(id, "")
	}
	return ConfigUpdateResult{Saved: true, Config: current}, nil
}

type CredentialValidation struct {
	OK            bool   `json:"ok"`
	Provider      string `json:"provider"`
	FormulaModels int    `json:"formula_models,omitempty"`
	Error         string `json:"error,omitempty"`
	LatencyMS     int64  `json:"latency_ms"`
}

type CredentialSaveResult struct {
	Saved             bool                 `json:"saved"`
	Backend           string               `json:"backend"`
	Models            int                  `json:"models"`
	Validation        CredentialValidation `json:"validation"`
	ModelProbeStarted bool                 `json:"model_probe_started,omitempty"`
}

type CredentialDeleteResult struct {
	Removed bool `json:"removed"`
	Models  int  `json:"models"`
}

type CredentialService struct {
	mu                 *sync.Mutex
	vault              *credentials.Vault
	routes             *routing.Store
	catalog            *catalog.Store
	reload             func(map[string][]string) (func(), error)
	providerProbes     *providerProbeStore
	markProviderFailed func(string, int, string, time.Duration)
	startModelProbe    func(string) bool
	resetProvider      func(string)
}

func (s *CredentialService) Save(ctx context.Context, providerID, apiKey string) (CredentialSaveResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	oldKey, _ := s.vault.Get(providerID)
	backend, err := s.vault.Set(providerID, apiKey)
	if err != nil {
		return CredentialSaveResult{}, &ServiceError{Stage: "save-credential", Err: err}
	}
	var rollback func()
	if s.reload != nil {
		rollback, err = s.reload(s.routes.Config().ProviderEnv)
	}
	if err != nil {
		if oldKey != "" {
			_, _ = s.vault.Set(providerID, oldKey)
		} else {
			_ = s.vault.Delete(providerID)
		}
		if rollback != nil {
			rollback()
		}
		return CredentialSaveResult{}, &ServiceError{Stage: "reload-providers", Err: err}
	}
	started := time.Now()
	count, probeErr := s.catalog.Probe(ctx, providerID)
	latency := time.Since(started)
	result := CredentialSaveResult{Saved: true, Backend: backend, Models: len(s.catalog.Models()), Validation: CredentialValidation{Provider: providerID, FormulaModels: count, LatencyMS: latency.Milliseconds()}}
	if probeErr != nil {
		status, message := providerProbeFailure(providerID, probeErr)
		s.providerProbes.failure(providerID, status, message, latency)
		s.markProviderFailed(providerID, status, message, latency)
		result.Validation.Error = message
		return result, nil
	}
	result.Validation.OK = true
	s.providerProbes.success(providerID, count, latency)
	result.ModelProbeStarted = s.startModelProbe(providerID)
	return result, nil
}

func (s *CredentialService) Delete(providerID string) (CredentialDeleteResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	oldKey, _ := s.vault.Get(providerID)
	if err := s.vault.Delete(providerID); err != nil {
		return CredentialDeleteResult{}, &ServiceError{Stage: "delete-credential", Err: err}
	}
	var rollback func()
	var err error
	if s.reload != nil {
		rollback, err = s.reload(s.routes.Config().ProviderEnv)
	}
	if err != nil {
		if oldKey != "" {
			_, _ = s.vault.Set(providerID, oldKey)
		}
		if rollback != nil {
			rollback()
		}
		return CredentialDeleteResult{}, &ServiceError{Stage: "reload-providers", Err: err}
	}
	s.providerProbes.remove(providerID)
	s.resetProvider(providerID)
	return CredentialDeleteResult{Removed: true, Models: len(s.catalog.Models())}, nil
}
