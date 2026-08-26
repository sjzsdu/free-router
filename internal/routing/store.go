package routing

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/sjzsdu/free-router/internal/catalog"
)

const CurrentVersion = 6

var ErrConfigConflict = errors.New("route configuration was changed by another request; reload the latest configuration and retry")

const (
	StrategyOrdered    = "ordered"
	StrategyRoundRobin = "round-robin"
)

type Route struct {
	Comment     string   `json:"_comment,omitempty"`
	Capability  string   `json:"capability"`
	LegacyType  string   `json:"type,omitempty"`
	Strategy    string   `json:"strategy,omitempty"`
	RequireTool bool     `json:"require_tool,omitempty"`
	Models      []string `json:"models"`
}

type Config struct {
	Comment     string                   `json:"_comment,omitempty"`
	Help        map[string]string        `json:"_help,omitempty"`
	Version     int                      `json:"version"`
	Revision    uint64                   `json:"revision"`
	ProviderEnv map[string][]string      `json:"provider_env"`
	Routes      map[string]Route         `json:"routes"`
	Models      map[string]ModelOverride `json:"models"`
}

type ModelOverride struct {
	Disabled   bool     `json:"disabled,omitempty"`
	Functions  []string `json:"functions,omitempty"`
	LegacyType string   `json:"type,omitempty"`
	ToolCall   *bool    `json:"tool_call,omitempty"`
	Vision     *bool    `json:"vision,omitempty"`
	Reasoning  *bool    `json:"reasoning,omitempty"`
}

type Store struct {
	path   string
	mu     sync.RWMutex
	config Config
}

func DefaultConfig() Config {
	return Config{Version: CurrentVersion, Revision: 1, Models: map[string]ModelOverride{}, ProviderEnv: map[string][]string{}, Routes: map[string]Route{
		catalog.FunctionChat:               {Capability: catalog.FunctionChat, Strategy: StrategyOrdered, Models: []string{}},
		catalog.FunctionChatTools:          {Capability: catalog.FunctionChatTools, Strategy: StrategyOrdered, RequireTool: true, Models: []string{}},
		catalog.FunctionImageUnderstanding: {Capability: catalog.FunctionImageUnderstanding, Strategy: StrategyOrdered, Models: []string{}},
		catalog.FunctionImageGeneration:    {Capability: catalog.FunctionImageGeneration, Strategy: StrategyOrdered, Models: []string{}},
		catalog.FunctionVideoUnderstanding: {Capability: catalog.FunctionVideoUnderstanding, Strategy: StrategyOrdered, Models: []string{}},
		catalog.FunctionVideoGeneration:    {Capability: catalog.FunctionVideoGeneration, Strategy: StrategyOrdered, Models: []string{}},
		catalog.FunctionAudioUnderstanding: {Capability: catalog.FunctionAudioUnderstanding, Strategy: StrategyOrdered, Models: []string{}},
		catalog.FunctionSpeechToText:       {Capability: catalog.FunctionSpeechToText, Strategy: StrategyOrdered, Models: []string{}},
		catalog.FunctionTextToSpeech:       {Capability: catalog.FunctionTextToSpeech, Strategy: StrategyOrdered, Models: []string{}},
		catalog.FunctionEmbedding:          {Capability: catalog.FunctionEmbedding, Strategy: StrategyOrdered, Models: []string{}},
		catalog.FunctionRerank:             {Capability: catalog.FunctionRerank, Strategy: StrategyOrdered, Models: []string{}},
		catalog.FunctionModeration:         {Capability: catalog.FunctionModeration, Strategy: StrategyOrdered, Models: []string{}},
	}}
}

func (s *Store) Apply(model catalog.Model) (catalog.Model, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Apply(s.config, model)
}

// Apply projects one model through an immutable configuration snapshot.
func Apply(config Config, model catalog.Model) (catalog.Model, bool) {
	override, ok := config.Models[model.ID]
	if !ok {
		return model, true
	}
	if override.Disabled {
		return model, false
	}
	if len(override.Functions) > 0 {
		model.Functions = append([]string{}, override.Functions...)
	}
	if override.ToolCall != nil {
		model.Capabilities.ToolCall = *override.ToolCall
		model.Capabilities.ToolCallKnown = true
		if *override.ToolCall && model.SupportsFunction(catalog.FunctionChat) && !model.SupportsFunction(catalog.FunctionChatTools) {
			model.Functions = append(model.Functions, catalog.FunctionChatTools)
		}
		if !*override.ToolCall {
			model.Functions = withoutFunction(model.Functions, catalog.FunctionChatTools)
		}
	}
	if override.Vision != nil {
		model.Capabilities.Vision = *override.Vision
		model.Capabilities.VisionKnown = true
	}
	if override.Reasoning != nil {
		model.Capabilities.Reasoning = *override.Reasoning
		model.Capabilities.ReasoningKnown = true
	}
	return model, true
}

func withoutFunction(functions []string, excluded string) []string {
	result := make([]string, 0, len(functions))
	for _, function := range functions {
		if function != excluded {
			result = append(result, function)
		}
	}
	return result
}

func Accepts(route Route, model catalog.Model) bool {
	if !model.SupportsFunction(route.Capability) {
		return false
	}
	return !(route.RequireTool || route.Capability == catalog.FunctionChatTools) || model.Supports("tools")
}

func ModelRouteTypes(model catalog.Model) []string {
	return append([]string{}, model.Functions...)
}

func New(path string) (*Store, error) {
	store := &Store{path: path, config: DefaultConfig()}
	if err := store.load(); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err := store.save(store.config); err != nil {
			return nil, err
		}
	}
	return store, nil
}

func (s *Store) Path() string { return s.path }

func (s *Store) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneConfig(s.config)
}

func (s *Store) Route(alias string) (Route, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	route, ok := s.config.Routes[alias]
	route.Models = append([]string{}, route.Models...)
	return route, ok
}

func (s *Store) Aliases() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]string, 0, len(s.config.Routes))
	for alias := range s.config.Routes {
		result = append(result, alias)
	}
	sort.Strings(result)
	return result
}

func (s *Store) Update(config Config) error {
	config = mergeDefaults(config)
	if err := validate(&config); err != nil {
		return err
	}
	// Serialize save+set so two concurrent PUTs cannot leave the on-disk
	// config and the in-memory config divergent (disk=A, memory=B).
	s.mu.Lock()
	defer s.mu.Unlock()
	if config.Revision != s.config.Revision {
		return ErrConfigConflict
	}
	config.Revision++
	if err := s.save(config); err != nil {
		return err
	}
	s.config = cloneConfig(config)
	return nil
}

func (s *Store) UpdateTransactional(config Config, validateFunc func(Config, Config) error) error {
	config = mergeDefaults(config)
	if err := validate(&config); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if config.Revision != s.config.Revision {
		return ErrConfigConflict
	}
	current := cloneConfig(s.config)
	config.Revision++
	if validateFunc != nil {
		if err := validateFunc(current, config); err != nil {
			return err
		}
	}
	if err := s.save(config); err != nil {
		return err
	}

	s.config = cloneConfig(config)
	return nil
}

func (s *Store) load() error {
	content, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var config Config
	if err := json.Unmarshal(content, &config); err != nil {
		return fmt.Errorf("decode route config: %w", err)
	}
	originalVersion := config.Version
	config = mergeDefaults(config)
	if err := validate(&config); err != nil {
		return fmt.Errorf("validate route config: %w", err)
	}
	if originalVersion != CurrentVersion {
		if err := s.save(config); err != nil {
			return fmt.Errorf("save migrated route config: %w", err)
		}
	}
	s.config = config
	return nil
}

func (s *Store) save(config Config) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	content, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}

func mergeDefaults(config Config) Config {
	defaults := DefaultConfig()
	previousVersion := config.Version
	if config.Revision == 0 {
		config.Revision = 1
	}
	if config.Routes == nil {
		config.Routes = make(map[string]Route)
	}
	if config.Models == nil {
		config.Models = make(map[string]ModelOverride)
	}
	if config.ProviderEnv == nil {
		config.ProviderEnv = make(map[string][]string)
	}
	if previousVersion < 6 {
		migrated := make(map[string]Route)
		for alias, route := range config.Routes {
			capability := route.Capability
			if capability == "" {
				capability = route.LegacyType
			}
			if capability == "normal" {
				capability = catalog.FunctionChat
				if route.RequireTool || alias == catalog.FunctionChatTools {
					capability = catalog.FunctionChatTools
				}
			}
			route.LegacyType = ""
			switch capability {
			case "image":
				route.Capability = catalog.FunctionImageGeneration
				migrated[catalog.FunctionImageGeneration] = route
			case "video":
				route.Capability = catalog.FunctionVideoGeneration
				migrated[catalog.FunctionVideoGeneration] = route
			case "audio":
				for _, target := range []string{catalog.FunctionSpeechToText, catalog.FunctionTextToSpeech} {
					copy := route
					copy.Capability = target
					migrated[target] = copy
				}
			default:
				route.Capability = capability
				migrated[alias] = route
			}
		}
		config.Routes = migrated
		for id, override := range config.Models {
			if len(override.Functions) == 0 && override.LegacyType != "" {
				override.Functions = legacyFunctions(override.LegacyType)
			}
			override.LegacyType = ""
			config.Models[id] = override
		}
	}
	for alias, route := range defaults.Routes {
		if _, ok := config.Routes[alias]; !ok {
			config.Routes[alias] = route
		}
	}
	config.Version = CurrentVersion
	return config
}

func validate(config *Config) error {
	if len(config.Routes) == 0 {
		return errors.New("at least one route is required")
	}
	for alias, route := range config.Routes {
		if strings.TrimSpace(alias) == "" || strings.TrimSpace(route.Capability) == "" {
			return errors.New("route alias and capability must not be empty")
		}
		if alias != route.Capability || !knownFunction(route.Capability) {
			return fmt.Errorf("route %q must use the same built-in capability name", alias)
		}
		if route.Strategy == "" {
			route.Strategy = StrategyOrdered
		}
		if route.Strategy != StrategyOrdered && route.Strategy != StrategyRoundRobin {
			return fmt.Errorf("route %q strategy must be %q or %q", alias, StrategyOrdered, StrategyRoundRobin)
		}
		seen := make(map[string]bool)
		cleaned := make([]string, 0, len(route.Models))
		for _, model := range route.Models {
			model = strings.TrimSpace(model)
			if model == "" || seen[model] {
				continue
			}
			seen[model] = true
			cleaned = append(cleaned, model)
		}
		route.Models = cleaned
		config.Routes[alias] = route
	}
	cleanedOverrides := make(map[string]ModelOverride, len(config.Models))
	for model, override := range config.Models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		override.Functions = cleanFunctions(override.Functions)
		override.LegacyType = ""
		cleanedOverrides[model] = override
	}
	config.Models = cleanedOverrides
	cleanedProviderEnv := make(map[string][]string, len(config.ProviderEnv))
	for providerID, names := range config.ProviderEnv {
		providerID = strings.TrimSpace(providerID)
		if providerID == "" {
			continue
		}
		seen := make(map[string]bool)
		cleaned := make([]string, 0, len(names))
		for _, name := range names {
			name = strings.TrimSpace(name)
			if name == "" || seen[name] {
				continue
			}
			if !validEnvName(name) {
				return fmt.Errorf("invalid environment variable %q for provider %q", name, providerID)
			}
			seen[name] = true
			cleaned = append(cleaned, name)
		}
		if len(cleaned) > 0 {
			cleanedProviderEnv[providerID] = cleaned
		}
	}
	config.ProviderEnv = cleanedProviderEnv
	return nil
}

func knownFunction(function string) bool {
	for _, candidate := range catalog.AllFunctions() {
		if function == candidate {
			return true
		}
	}
	return false
}

func cleanFunctions(functions []string) []string {
	seen := make(map[string]bool)
	cleaned := make([]string, 0, len(functions))
	for _, function := range functions {
		function = strings.TrimSpace(function)
		if function == "" || seen[function] || !knownFunction(function) {
			continue
		}
		seen[function] = true
		cleaned = append(cleaned, function)
	}
	return cleaned
}

func legacyFunctions(modelType string) []string {
	switch modelType {
	case "normal", "chat":
		return []string{catalog.FunctionChat}
	case "chat-tools":
		return []string{catalog.FunctionChat, catalog.FunctionChatTools}
	case "image":
		return []string{catalog.FunctionImageGeneration}
	case "video":
		return []string{catalog.FunctionVideoGeneration}
	case "audio":
		return []string{catalog.FunctionSpeechToText, catalog.FunctionTextToSpeech}
	default:
		if knownFunction(modelType) {
			return []string{modelType}
		}
		return nil
	}
}

func cloneConfig(config Config) Config {
	clone := Config{Comment: config.Comment, Version: config.Version, Revision: config.Revision, Routes: make(map[string]Route, len(config.Routes)), Models: make(map[string]ModelOverride, len(config.Models)), ProviderEnv: make(map[string][]string, len(config.ProviderEnv))}
	if config.Help != nil {
		clone.Help = make(map[string]string, len(config.Help))
		for field, description := range config.Help {
			clone.Help[field] = description
		}
	}
	for alias, route := range config.Routes {
		route.Models = append([]string{}, route.Models...)
		clone.Routes[alias] = route
	}
	for model, override := range config.Models {
		override.Functions = append([]string{}, override.Functions...)
		clone.Models[model] = override
	}
	for providerID, names := range config.ProviderEnv {
		clone.ProviderEnv[providerID] = append([]string{}, names...)
	}
	return clone
}

func validEnvName(name string) bool {
	for index, character := range name {
		if (character >= 'A' && character <= 'Z') || (character >= 'a' && character <= 'z') || character == '_' || (index > 0 && character >= '0' && character <= '9') {
			continue
		}
		return false
	}
	return name != ""
}
