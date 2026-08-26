package statistics

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const schemaVersion = 1

// Usage is the token accounting reported by an OpenAI-compatible upstream.
type Usage struct {
	InputTokens  uint64 `json:"input_tokens"`
	OutputTokens uint64 `json:"output_tokens"`
	TotalTokens  uint64 `json:"total_tokens"`
}

// Attempt describes one actual upstream model call. Failed fallback attempts are
// deliberately included so success rates reflect provider quality, not only the
// final response observed by the client.
type Attempt struct {
	Model        string
	Provider     string
	Capability   string
	Success      bool
	StatusCode   int
	Latency      time.Duration
	Usage        *Usage
	MissingUsage bool
	At           time.Time
}

// ModelStats is the durable aggregate returned by the admin API.
type ModelStats struct {
	Model              string    `json:"model"`
	Provider           string    `json:"provider"`
	Capability         string    `json:"capability"`
	Requests           uint64    `json:"requests"`
	Successes          uint64    `json:"successes"`
	Failures           uint64    `json:"failures"`
	SuccessRate        float64   `json:"success_rate"`
	InputTokens        uint64    `json:"input_tokens"`
	OutputTokens       uint64    `json:"output_tokens"`
	TotalTokens        uint64    `json:"total_tokens"`
	UsageReported      uint64    `json:"usage_reported"`
	UsageMissing       uint64    `json:"usage_missing"`
	AverageLatencyMS   float64   `json:"average_latency_ms"`
	LastStatus         int       `json:"last_status,omitempty"`
	LastUsedAt         time.Time `json:"last_used_at"`
	latencyTotalMicros uint64
}

type Snapshot struct {
	UpdatedAt *time.Time   `json:"updated_at,omitempty"`
	Models    []ModelStats `json:"models"`
}

type persisted struct {
	SchemaVersion int              `json:"schema_version"`
	UpdatedAt     time.Time        `json:"updated_at"`
	Models        []persistedModel `json:"models"`
}

type persistedModel struct {
	ModelStats
	LatencyTotalMicros uint64 `json:"latency_total_micros"`
}

// Store keeps model aggregates in memory and atomically replaces its JSON file
// after every attempt. A blank path creates an in-memory store for tests.
type Store struct {
	mu        sync.RWMutex
	path      string
	models    map[string]*ModelStats
	updatedAt time.Time
	now       func() time.Time
}

func New(path string) (*Store, error) {
	s := &Store{path: path, models: make(map[string]*ModelStats), now: time.Now}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func NewMemory() *Store {
	s, _ := New("")
	return s
}

func (s *Store) Path() string { return s.path }

func (s *Store) Record(attempt Attempt) error {
	if attempt.Model == "" {
		return errors.New("statistics attempt model must not be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	key := attempt.Model + "\x00" + attempt.Capability
	item := s.models[key]
	if item == nil {
		item = &ModelStats{Model: attempt.Model, Provider: attempt.Provider, Capability: attempt.Capability}
		s.models[key] = item
	}
	item.Provider = attempt.Provider
	item.Requests++
	if attempt.Success {
		item.Successes++
	} else {
		item.Failures++
	}
	item.SuccessRate = float64(item.Successes) / float64(item.Requests)
	if attempt.Usage != nil {
		item.InputTokens += attempt.Usage.InputTokens
		item.OutputTokens += attempt.Usage.OutputTokens
		total := attempt.Usage.TotalTokens
		if total == 0 {
			total = attempt.Usage.InputTokens + attempt.Usage.OutputTokens
		}
		item.TotalTokens += total
		item.UsageReported++
	} else if attempt.MissingUsage {
		item.UsageMissing++
	}
	if attempt.Latency > 0 {
		item.latencyTotalMicros += uint64(attempt.Latency.Microseconds())
		item.AverageLatencyMS = float64(item.latencyTotalMicros) / float64(item.Requests) / 1000
	}
	item.LastStatus = attempt.StatusCode
	item.LastUsedAt = attempt.At
	if item.LastUsedAt.IsZero() {
		item.LastUsedAt = s.now()
	}
	s.updatedAt = item.LastUsedAt
	return s.persistLocked()
}

func (s *Store) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	models := make([]ModelStats, 0, len(s.models))
	for _, item := range s.models {
		models = append(models, *item)
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].Requests != models[j].Requests {
			return models[i].Requests > models[j].Requests
		}
		if models[i].Model != models[j].Model {
			return models[i].Model < models[j].Model
		}
		return models[i].Capability < models[j].Capability
	})
	var updatedAt *time.Time
	if !s.updatedAt.IsZero() {
		value := s.updatedAt
		updatedAt = &value
	}
	return Snapshot{UpdatedAt: updatedAt, Models: models}
}

func (s *Store) load() error {
	if s.path == "" {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var state persisted
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode statistics: %w", err)
	}
	if state.SchemaVersion != schemaVersion {
		return fmt.Errorf("unsupported statistics schema version %d", state.SchemaVersion)
	}
	s.updatedAt = state.UpdatedAt
	for i := range state.Models {
		item := state.Models[i].ModelStats
		item.latencyTotalMicros = state.Models[i].LatencyTotalMicros
		s.models[item.Model+"\x00"+item.Capability] = &item
	}
	return nil
}

func (s *Store) persistLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	models := make([]persistedModel, 0, len(s.models))
	for _, item := range s.models {
		models = append(models, persistedModel{ModelStats: *item, LatencyTotalMicros: item.latencyTotalMicros})
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].ModelStats.Model != models[j].ModelStats.Model {
			return models[i].ModelStats.Model < models[j].ModelStats.Model
		}
		return models[i].ModelStats.Capability < models[j].ModelStats.Capability
	})
	data, err := json.MarshalIndent(persisted{SchemaVersion: schemaVersion, UpdatedAt: s.updatedAt, Models: models}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".statistics-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(s.path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
