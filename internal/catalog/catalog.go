package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sjzsdu/free-router/internal/provider"
)

type Model struct {
	ID                  string       `json:"id"`
	Provider            string       `json:"provider"`
	UpstreamID          string       `json:"upstream_id"`
	Name                string       `json:"name,omitempty"`
	Description         string       `json:"description,omitempty"`
	OwnedBy             string       `json:"owned_by,omitempty"`
	Created             int64        `json:"created,omitempty"`
	Type                string       `json:"type"`
	Free                bool         `json:"free"`
	Tier                string       `json:"tier,omitempty"`
	ContextLength       int          `json:"context_length,omitempty"`
	MaxOutputTokens     int          `json:"max_output_tokens,omitempty"`
	InputModalities     []string     `json:"input_modalities,omitempty"`
	OutputModalities    []string     `json:"output_modalities,omitempty"`
	SupportedParameters []string     `json:"supported_parameters,omitempty"`
	SupportedEndpoints  []string     `json:"supported_endpoints,omitempty"`
	Capabilities        Capabilities `json:"capabilities"`
	Pricing             Pricing      `json:"pricing,omitempty"`
}

type Capabilities struct {
	ToolCall       bool `json:"tool_call"`
	ToolCallKnown  bool `json:"tool_call_known"`
	Reasoning      bool `json:"reasoning"`
	ReasoningKnown bool `json:"reasoning_known"`
	Vision         bool `json:"vision"`
	VisionKnown    bool `json:"vision_known"`
	Streaming      bool `json:"streaming"`
}

type Pricing struct {
	Prompt     string `json:"prompt,omitempty"`
	Completion string `json:"completion,omitempty"`
}

type upstreamModel struct {
	ID                  string       `json:"id"`
	Name                string       `json:"name"`
	Description         string       `json:"description"`
	OwnedBy             string       `json:"owned_by"`
	Created             int64        `json:"created"`
	Type                string       `json:"type"`
	SubType             string       `json:"sub_type"`
	ContextLength       int          `json:"context_length"`
	ContextWindow       int          `json:"context_window"`
	MaxOutputTokens     int          `json:"max_output_tokens"`
	MaxCompletionTokens int          `json:"max_completion_tokens"`
	InputModalities     []string     `json:"input_modalities"`
	OutputModalities    []string     `json:"output_modalities"`
	SupportedParameters []string     `json:"supported_parameters"`
	SupportedEndpoints  []string     `json:"supported_endpoints"`
	Tools               flexibleBool `json:"tools"`
	ToolCall            flexibleBool `json:"tool_call"`
	Reasoning           flexibleBool `json:"reasoning"`
	Vision              flexibleBool `json:"vision"`
	Pricing             struct {
		Prompt     string `json:"prompt"`
		Completion string `json:"completion"`
	} `json:"pricing"`
	Architecture struct {
		InputModalities  []string `json:"input_modalities"`
		OutputModalities []string `json:"output_modalities"`
	} `json:"architecture"`
	TopProvider struct {
		MaxCompletionTokens int `json:"max_completion_tokens"`
	} `json:"top_provider"`
}

type modelsResponse struct {
	Data   []upstreamModel `json:"data"`
	Models []upstreamModel `json:"models"`
	Result []upstreamModel `json:"result"`
}

type Store struct {
	registry *provider.Registry
	cache    string
	client   *http.Client
	mu       sync.RWMutex
	models   []Model
}

func New(registry *provider.Registry, cache string, client *http.Client) *Store {
	return &Store{registry: registry, cache: cache, client: client}
}

func (s *Store) Bootstrap(ctx context.Context) error {
	if err := s.Refresh(ctx); err == nil {
		return nil
	} else if cacheErr := s.loadCache(); cacheErr != nil {
		return errors.Join(err, cacheErr)
	}
	slog.Warn("using cached model catalog because every provider refresh failed")
	return nil
}

// Refresh keeps successful providers fresh while retaining the last good models for failed providers.
func (s *Store) Refresh(ctx context.Context) error {
	type result struct {
		provider string
		models   []Model
		err      error
	}
	providers := s.registry.All()
	results := make(chan result, len(providers))
	for _, spec := range providers {
		go func() {
			models, err := s.fetch(ctx, spec)
			results <- result{provider: spec.ID, models: models, err: err}
		}()
	}

	current := s.Models()
	byProvider := make(map[string][]Model)
	for _, model := range current {
		byProvider[model.Provider] = append(byProvider[model.Provider], model)
	}
	successes := 0
	var refreshErrors []error
	for range providers {
		result := <-results
		if result.err != nil {
			refreshErrors = append(refreshErrors, fmt.Errorf("%s: %w", result.provider, result.err))
			slog.Warn("provider model refresh failed", "provider", result.provider, "error", result.err)
			continue
		}
		successes++
		byProvider[result.provider] = result.models
	}
	if successes == 0 && len(current) == 0 {
		return errors.Join(refreshErrors...)
	}

	merged := make([]Model, 0)
	for _, models := range byProvider {
		merged = append(merged, models...)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].ID < merged[j].ID })
	s.set(merged)
	if err := s.saveCache(merged); err != nil {
		slog.Warn("could not save model cache", "error", err)
	}
	return nil
}

func (s *Store) fetch(ctx context.Context, spec provider.Spec) ([]Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, spec.ModelsEndpoint(), nil)
	if err != nil {
		return nil, err
	}
	headers := cloneMap(spec.Headers)
	spec.ApplyAuth(headers)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models endpoint returned %s", resp.Status)
	}
	var payload modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}
	candidates := payload.Data
	if len(candidates) == 0 {
		candidates = payload.Models
	}
	if len(candidates) == 0 {
		candidates = payload.Result
	}
	models := make([]Model, 0, len(candidates))
	for _, candidate := range candidates {
		if spec.UseNameAsID && candidate.Name != "" {
			candidate.ID = candidate.Name
		}
		if candidate.ID == "" || !allowed(spec.AllowedModels, candidate.ID) || (spec.Filter == provider.FilterZeroPrice && !zeroPriced(candidate)) {
			continue
		}
		input := candidate.InputModalities
		output := candidate.OutputModalities
		if len(input) == 0 {
			input = candidate.Architecture.InputModalities
		}
		if len(output) == 0 {
			output = candidate.Architecture.OutputModalities
		}
		parameters := candidate.SupportedParameters
		if candidate.Tools.Value && !contains(parameters, "tools") {
			parameters = append(parameters, "tools")
		}
		contextLength := firstPositive(candidate.ContextLength, candidate.ContextWindow)
		maxOutputTokens := firstPositive(candidate.MaxOutputTokens, candidate.MaxCompletionTokens, candidate.TopProvider.MaxCompletionTokens)
		modelType := classifyModel(candidate, input, output)
		capabilities := inferCapabilities(candidate, modelType, input, parameters)
		models = append(models, Model{
			ID: spec.ID + "/" + candidate.ID, Provider: spec.ID, UpstreamID: candidate.ID,
			Name: candidate.Name, Description: candidate.Description, OwnedBy: candidate.OwnedBy, Created: candidate.Created,
			Type: modelType, Free: true, Tier: spec.Tier,
			ContextLength: contextLength, MaxOutputTokens: maxOutputTokens,
			InputModalities: input, OutputModalities: output, SupportedParameters: parameters,
			SupportedEndpoints: candidate.SupportedEndpoints, Capabilities: capabilities,
			Pricing: Pricing{Prompt: candidate.Pricing.Prompt, Completion: candidate.Pricing.Completion},
		})
	}
	if len(models) == 0 {
		return nil, errors.New("no eligible models returned")
	}
	return models, nil
}

func (s *Store) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.Refresh(ctx); err != nil {
					slog.Warn("model catalog refresh failed", "error", err)
				}
			}
		}
	}()
}

func (s *Store) Models() []Model {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Model(nil), s.models...)
}

func (s *Store) Find(id string) (Model, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, model := range s.models {
		if model.ID == id || model.UpstreamID == id {
			if _, enabled := s.registry.Get(model.Provider); enabled {
				return model, true
			}
		}
	}
	return Model{}, false
}

func (model Model) IsTextChat() bool {
	return model.Type == "normal"
}

func (model Model) Supports(parameter string) bool {
	if parameter == "tools" && model.Capabilities.ToolCallKnown {
		return model.Capabilities.ToolCall
	}
	if len(model.SupportedParameters) == 0 {
		return true
	}
	return contains(model.SupportedParameters, parameter)
}

func (s *Store) set(models []Model) {
	s.mu.Lock()
	s.models = models
	s.mu.Unlock()
}

func (s *Store) loadCache() error {
	data, err := os.ReadFile(s.cache)
	if err != nil {
		return fmt.Errorf("read cache: %w", err)
	}
	var cached []Model
	if err := json.Unmarshal(data, &cached); err != nil {
		return fmt.Errorf("decode cache: %w", err)
	}
	models := make([]Model, 0, len(cached))
	for _, model := range cached {
		if model.Provider == "" { // v0.1 cache migration.
			model.Provider, model.UpstreamID = "openrouter", model.ID
			model.ID = "openrouter/" + model.ID
		}
		if _, enabled := s.registry.Get(model.Provider); enabled {
			if model.Type == "" {
				model.Type = classifyID(model.UpstreamID)
				model.Free = true
				model.Capabilities = inferCachedCapabilities(model)
			}
			models = append(models, model)
		}
	}
	if len(models) == 0 {
		return errors.New("cache has no models for configured providers")
	}
	s.set(models)
	return nil
}

func (s *Store) saveCache(models []Model) error {
	if err := os.MkdirAll(filepath.Dir(s.cache), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(models, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.cache + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.cache)
}

func zeroPriced(model upstreamModel) bool {
	return isZero(model.Pricing.Prompt) && isZero(model.Pricing.Completion)
}

func isZero(value string) bool {
	if value == "" {
		return false
	}
	price, err := strconv.ParseFloat(value, 64)
	return err == nil && price == 0
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func allowed(allowlist []string, model string) bool {
	return len(allowlist) == 0 || contains(allowlist, model)
}

func cloneMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source)+1)
	for key, value := range source {
		result[key] = value
	}
	return result
}

func classifyModel(candidate upstreamModel, input, output []string) string {
	for _, value := range []string{candidate.SubType, candidate.Type} {
		if modelType := normalizeType(value); modelType != "" {
			return modelType
		}
	}
	for _, endpoint := range candidate.SupportedEndpoints {
		if modelType := normalizeType(endpoint); modelType != "" {
			return modelType
		}
	}
	for _, modality := range output {
		if modality == "video" || modality == "image" || modality == "audio" {
			return modality
		}
	}
	return classifyID(candidate.ID)
}

func classifyID(id string) string {
	value := strings.ToLower(id)
	checks := []struct {
		typeName string
		markers  []string
	}{
		{"video", []string{"video", "text-to-video", "image-to-video", "kling", "veo", "hunyuanvideo", "wan", "t2v"}},
		{"image", []string{"image", "text-to-image", "stable-diffusion", "sdxl", "flux", "kolors"}},
		{"rerank", []string{"rerank", "reranker"}},
		{"embedding", []string{"embed", "/bge-", "e5-", "gte-"}},
		{"audio", []string{"audio", "whisper", "transcri", "speech", "tts", "voice", "asr"}},
		{"moderation", []string{"moderation", "guard", "safety"}},
	}
	for _, check := range checks {
		for _, marker := range check.markers {
			if strings.Contains(value, marker) {
				return check.typeName
			}
		}
	}
	return "normal"
}

func normalizeType(value string) string {
	value = strings.ToLower(value)
	switch {
	case strings.Contains(value, "rerank"):
		return "rerank"
	case strings.Contains(value, "embed"):
		return "embedding"
	case strings.Contains(value, "video"):
		return "video"
	case strings.Contains(value, "image"):
		return "image"
	case strings.Contains(value, "audio") || strings.Contains(value, "speech") || strings.Contains(value, "transcri"):
		return "audio"
	case strings.Contains(value, "moderation") || strings.Contains(value, "safety"):
		return "moderation"
	case value == "text" || value == "chat" || strings.Contains(value, "chat/completions"):
		return "normal"
	default:
		return ""
	}
}

func inferCapabilities(candidate upstreamModel, modelType string, input, parameters []string) Capabilities {
	id := strings.ToLower(candidate.ID)
	return Capabilities{
		ToolCall:       candidate.Tools.Value || candidate.ToolCall.Value || contains(parameters, "tools") || contains(parameters, "tool_choice"),
		ToolCallKnown:  candidate.Tools.Known || candidate.ToolCall.Known || len(parameters) > 0,
		Reasoning:      candidate.Reasoning.Value || contains(parameters, "reasoning") || contains(parameters, "reasoning_effort") || strings.Contains(id, "reasoning") || strings.Contains(id, "deepseek-r1") || strings.Contains(id, "qwq"),
		ReasoningKnown: candidate.Reasoning.Known || len(parameters) > 0,
		Vision:         candidate.Vision.Value || contains(input, "image") || contains(input, "video") || strings.Contains(id, "vision") || strings.Contains(id, "-vl"),
		VisionKnown:    candidate.Vision.Known || len(input) > 0,
		Streaming:      modelType == "normal",
	}
}

func inferCachedCapabilities(model Model) Capabilities {
	return Capabilities{
		ToolCall:       contains(model.SupportedParameters, "tools") || contains(model.SupportedParameters, "tool_choice"),
		ToolCallKnown:  len(model.SupportedParameters) > 0,
		Reasoning:      contains(model.SupportedParameters, "reasoning") || contains(model.SupportedParameters, "reasoning_effort"),
		ReasoningKnown: len(model.SupportedParameters) > 0,
		Vision:         contains(model.InputModalities, "image") || contains(model.InputModalities, "video"),
		VisionKnown:    len(model.InputModalities) > 0,
		Streaming:      model.Type == "normal",
	}
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

type flexibleBool struct {
	Value bool
	Known bool
}

func (value *flexibleBool) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}
	value.Known = true
	var boolean bool
	if err := json.Unmarshal(data, &boolean); err == nil {
		value.Value = boolean
		return nil
	}
	// Some catalogs expose a capability configuration object instead of a bool.
	value.Value = len(data) > 0 && string(data) != "{}" && string(data) != "[]"
	return nil
}
