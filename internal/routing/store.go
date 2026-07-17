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

const CurrentVersion = 4

type Route struct {
	Type        string   `json:"type"`
	RequireTool bool     `json:"require_tool,omitempty"`
	Models      []string `json:"models"`
}

type Config struct {
	Version     int                      `json:"version"`
	ProviderEnv map[string][]string      `json:"provider_env"`
	Routes      map[string]Route         `json:"routes"`
	Models      map[string]ModelOverride `json:"models"`
}

type ModelOverride struct {
	Disabled  bool   `json:"disabled,omitempty"`
	Type      string `json:"type,omitempty"`
	ToolCall  *bool  `json:"tool_call,omitempty"`
	Vision    *bool  `json:"vision,omitempty"`
	Reasoning *bool  `json:"reasoning,omitempty"`
}

type Store struct {
	path   string
	mu     sync.RWMutex
	config Config
}

func DefaultConfig() Config {
	return Config{Version: CurrentVersion, Models: map[string]ModelOverride{}, ProviderEnv: map[string][]string{}, Routes: map[string]Route{
		"chat":       {Type: "chat", Models: []string{}},
		"chat-tools": {Type: "chat-tools", RequireTool: true, Models: []string{}},
		"embedding":  {Type: "embedding", Models: []string{}},
		"audio":      {Type: "audio", Models: []string{}},
		"image":      {Type: "image", Models: []string{}},
		"video":      {Type: "video", Models: []string{}},
		"rerank":     {Type: "rerank", Models: []string{}},
		"moderation": {Type: "moderation", Models: []string{}},
	}}
}

func (s *Store) Apply(model catalog.Model) (catalog.Model, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	override, ok := s.config.Models[model.ID]
	if !ok {
		return model, true
	}
	if override.Disabled {
		return model, false
	}
	if override.Type != "" {
		model.Type = InternalModelType(override.Type)
		if override.Type == "chat-tools" {
			model.Capabilities.ToolCall = true
			model.Capabilities.ToolCallKnown = true
		}
	}
	if override.ToolCall != nil {
		model.Capabilities.ToolCall = *override.ToolCall
		model.Capabilities.ToolCallKnown = true
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

func InternalModelType(routeType string) string {
	switch routeType {
	case "chat", "chat-tools":
		return "normal"
	default:
		return routeType
	}
}

func Accepts(route Route, model catalog.Model) bool {
	if model.Type != InternalModelType(route.Type) {
		return false
	}
	return !(route.RequireTool || route.Type == "chat-tools") || model.Supports("tools")
}

func ModelRouteTypes(model catalog.Model) []string {
	if model.Type != "normal" {
		return []string{model.Type}
	}
	result := []string{"chat"}
	if model.Supports("tools") {
		result = append(result, "chat-tools")
	}
	return result
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
	if err := s.save(config); err != nil {
		return err
	}
	s.mu.Lock()
	s.config = cloneConfig(config)
	s.mu.Unlock()
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
	if config.Routes == nil {
		config.Routes = make(map[string]Route)
	}
	if config.Models == nil {
		config.Models = make(map[string]ModelOverride)
	}
	if config.ProviderEnv == nil {
		config.ProviderEnv = make(map[string][]string)
	}
	for alias, route := range defaults.Routes {
		if _, ok := config.Routes[alias]; !ok {
			config.Routes[alias] = route
		}
	}
	if previousVersion < 3 {
		for alias, route := range config.Routes {
			if route.Type == "normal" {
				if route.RequireTool || alias == "chat-tools" {
					route.Type = "chat-tools"
				} else {
					route.Type = "chat"
				}
				config.Routes[alias] = route
			}
		}
		for id, override := range config.Models {
			if override.Type == "normal" {
				override.Type = "chat"
				config.Models[id] = override
			}
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
		if strings.TrimSpace(alias) == "" || strings.TrimSpace(route.Type) == "" {
			return errors.New("route alias and type must not be empty")
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
		override.Type = strings.TrimSpace(override.Type)
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

func cloneConfig(config Config) Config {
	clone := Config{Version: config.Version, Routes: make(map[string]Route, len(config.Routes)), Models: make(map[string]ModelOverride, len(config.Models)), ProviderEnv: make(map[string][]string, len(config.ProviderEnv))}
	for alias, route := range config.Routes {
		route.Models = append([]string{}, route.Models...)
		clone.Routes[alias] = route
	}
	for model, override := range config.Models {
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
