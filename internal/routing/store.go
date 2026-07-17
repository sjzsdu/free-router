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
)

const CurrentVersion = 1

type Route struct {
	Type        string   `json:"type"`
	RequireTool bool     `json:"require_tool,omitempty"`
	Models      []string `json:"models"`
}

type Config struct {
	Version int              `json:"version"`
	Routes  map[string]Route `json:"routes"`
}

type Store struct {
	path   string
	mu     sync.RWMutex
	config Config
}

func DefaultConfig() Config {
	return Config{Version: CurrentVersion, Routes: map[string]Route{
		"chat":       {Type: "normal", Models: []string{}},
		"chat-tools": {Type: "normal", RequireTool: true, Models: []string{}},
		"embedding":  {Type: "embedding", Models: []string{}},
		"audio":      {Type: "audio", Models: []string{}},
		"image":      {Type: "image", Models: []string{}},
		"video":      {Type: "video", Models: []string{}},
		"rerank":     {Type: "rerank", Models: []string{}},
		"moderation": {Type: "moderation", Models: []string{}},
	}}
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
	if err := validate(config); err != nil {
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
	config = mergeDefaults(config)
	if err := validate(config); err != nil {
		return fmt.Errorf("validate route config: %w", err)
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
	if config.Routes == nil {
		config.Routes = make(map[string]Route)
	}
	for alias, route := range defaults.Routes {
		if _, ok := config.Routes[alias]; !ok {
			config.Routes[alias] = route
		}
	}
	config.Version = CurrentVersion
	return config
}

func validate(config Config) error {
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
	return nil
}

func cloneConfig(config Config) Config {
	clone := Config{Version: config.Version, Routes: make(map[string]Route, len(config.Routes))}
	for alias, route := range config.Routes {
		route.Models = append([]string{}, route.Models...)
		clone.Routes[alias] = route
	}
	return clone
}
