package catalog

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sjzsdu/free-router/internal/provider"
)

//go:embed assets/probe.wav.b64
var probeWAVBase64 string

//go:embed assets/probe.png.b64
var probePNGBase64 string

//go:embed assets/probe.mp4.b64
var probeMP4Base64 string

const (
	FunctionChat               = "chat"
	FunctionChatTools          = "chat-tools"
	FunctionImageUnderstanding = "image-understanding"
	FunctionImageGeneration    = "image-generation"
	FunctionVideoUnderstanding = "video-understanding"
	FunctionVideoGeneration    = "video-generation"
	FunctionAudioUnderstanding = "audio-understanding"
	FunctionSpeechToText       = "speech-to-text"
	FunctionTextToSpeech       = "text-to-speech"
	FunctionEmbedding          = "embedding"
	FunctionRerank             = "rerank"
	FunctionModeration         = "moderation"
)

var ErrUnverifiedInventory = errors.New("free model inventory is unverified")

type Model struct {
	ID                  string       `json:"id"`
	Provider            string       `json:"provider"`
	UpstreamID          string       `json:"upstream_id"`
	Name                string       `json:"name,omitempty"`
	Description         string       `json:"description,omitempty"`
	OwnedBy             string       `json:"owned_by,omitempty"`
	Created             int64        `json:"created,omitempty"`
	Type                string       `json:"type"`
	Functions           []string     `json:"functions"`
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

type ModelProbeResult struct {
	Status int
}

type ModelProbeError struct {
	Status  int
	Message string
}

type DiscoveryFailure struct {
	Provider string `json:"provider"`
	Error    string `json:"error"`
}

func (e *ModelProbeError) Error() string { return e.Message }

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
	registry  *provider.Registry
	cache     string
	client    *http.Client
	refreshMu sync.Mutex
	mu        sync.RWMutex
	models    []Model
	updated   time.Time
}

type cacheFile struct {
	SchemaVersion       int     `json:"schema_version"`
	ManifestGeneratedAt string  `json:"manifest_generated_at"`
	Models              []Model `json:"models"`
}

func New(registry *provider.Registry, cache string, client *http.Client) *Store {
	return &Store{registry: registry, cache: cache, client: client}
}

func (s *Store) Bootstrap(ctx context.Context) error {
	if err := s.loadCache(); err == nil {
		return nil
	}
	return s.Refresh(ctx)
}

// Refresh rebuilds the routable cache exclusively from Formula-produced inventories.
// It never contacts provider model endpoints.
func (s *Store) Refresh(ctx context.Context) error {
	_ = ctx
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	merged := make([]Model, 0)
	for _, spec := range s.registry.All() {
		merged = append(merged, modelsFromDiscovery(spec)...)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].ID < merged[j].ID })
	s.set(merged, time.Now())
	return s.saveCache(merged)
}

func cacheEligible(spec provider.Spec, model Model) bool {
	if len(spec.DiscoveredModels) == 0 {
		return false
	}
	return modelEligible(spec, model.UpstreamID, model.Pricing.Prompt, model.Pricing.Completion)
}

func modelEligible(spec provider.Spec, modelID, promptPrice, completionPrice string) bool {
	if modelID == "" || spec.DiscoveryPolicy == "unverified" {
		return false
	}
	if !allowed(spec.AllowedModels, spec.AllowedModelPatterns, modelID) {
		return false
	}
	return spec.Filter != provider.FilterZeroPrice || (isZero(promptPrice) && isZero(completionPrice))
}

// RefreshProvider reapplies one Provider's Formula inventory without network discovery.
func (s *Store) RefreshProvider(ctx context.Context, providerID string) error {
	_ = ctx
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	spec, ok := s.registry.Get(providerID)
	if !ok {
		return fmt.Errorf("provider %q is not configured", providerID)
	}
	models := modelsFromDiscovery(spec)
	merged := make([]Model, 0, len(s.Models())+len(models))
	for _, model := range s.Models() {
		if model.Provider != providerID {
			merged = append(merged, model)
		}
	}
	merged = append(merged, models...)
	sort.Slice(merged, func(i, j int) bool { return merged[i].ID < merged[j].ID })
	s.set(merged, time.Now())
	if err := s.saveCache(merged); err != nil {
		return fmt.Errorf("save model cache: %w", err)
	}
	return nil
}

// PruneDisabled removes cached models whose provider is no longer configured.
func (s *Store) PruneDisabled() error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	current := s.Models()
	models := make([]Model, 0, len(current))
	for _, model := range current {
		if _, enabled := s.registry.Get(model.Provider); enabled {
			models = append(models, model)
		}
	}
	s.set(models, time.Now())
	return s.saveCache(models)
}

// fetchUpstream is reserved for the maintainer discovery command used by the Formula.
// Runtime catalog refreshes never call it.
func (s *Store) fetchUpstream(ctx context.Context, spec provider.Spec) ([]Model, error) {
	if spec.DiscoveryPolicy == "unverified" {
		return nil, fmt.Errorf("%w; rerun the discovery formula", ErrUnverifiedInventory)
	}
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
		detail := upstreamErrorDetail(resp.Body)
		if detail != "" {
			return nil, fmt.Errorf("models endpoint returned %s: %s", resp.Status, detail)
		}
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
		if !modelEligible(spec, candidate.ID, candidate.Pricing.Prompt, candidate.Pricing.Completion) {
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
		functions := inferFunctions(candidate, modelType, input, output, parameters)
		models = append(models, Model{
			ID: spec.ID + "/" + candidate.ID, Provider: spec.ID, UpstreamID: candidate.ID,
			Name: candidate.Name, Description: candidate.Description, OwnedBy: candidate.OwnedBy, Created: candidate.Created,
			Type: modelType, Functions: functions, Free: true, Tier: spec.Tier,
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

func modelsFromDiscovery(spec provider.Spec) []Model {
	models := make([]Model, 0, len(spec.DiscoveredModels))
	for _, candidate := range spec.DiscoveredModels {
		if !modelEligible(spec, candidate.ID, candidate.Pricing.Prompt, candidate.Pricing.Completion) {
			continue
		}
		modelType := candidate.Type
		if modelType == "" {
			modelType = "normal"
		}
		functions := append([]string{}, candidate.Functions...)
		if len(functions) == 0 {
			functions = []string{FunctionChat}
		}
		capabilities := Capabilities{
			ToolCall:       contains(functions, FunctionChatTools),
			ToolCallKnown:  len(candidate.Functions) > 0,
			Vision:         contains(functions, FunctionImageUnderstanding),
			VisionKnown:    len(candidate.Functions) > 0,
			Reasoning:      contains(candidate.SupportedParameters, "reasoning"),
			ReasoningKnown: contains(candidate.SupportedParameters, "reasoning"),
			Streaming:      contains(candidate.SupportedParameters, "stream"),
		}
		models = append(models, Model{
			ID: spec.ID + "/" + candidate.ID, Provider: spec.ID, UpstreamID: candidate.ID,
			Name: candidate.Name, Description: candidate.Description, OwnedBy: candidate.OwnedBy,
			Type: modelType, Functions: functions, Free: true, Tier: spec.Tier,
			ContextLength: candidate.ContextLength, MaxOutputTokens: candidate.MaxOutputTokens,
			InputModalities:     append([]string{}, candidate.InputModalities...),
			OutputModalities:    append([]string{}, candidate.OutputModalities...),
			SupportedParameters: append([]string{}, candidate.SupportedParameters...),
			SupportedEndpoints:  append([]string{}, candidate.SupportedEndpoints...),
			Capabilities:        capabilities,
			Pricing: Pricing{
				Prompt: candidate.Pricing.Prompt, Completion: candidate.Pricing.Completion,
			},
		})
	}
	if len(models) == 0 {
		return nil
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models
}

func upstreamErrorDetail(body io.Reader) string {
	content, err := io.ReadAll(io.LimitReader(body, 16<<10))
	if err != nil || len(content) == 0 {
		return ""
	}
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(content, &payload) == nil {
		message := payload.Error.Message
		if message == "" {
			message = payload.Message
		}
		if message != "" {
			return truncateDetail(message)
		}
	}
	return truncateDetail(string(content))
}

func truncateDetail(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 500 {
		return value[:500] + "…"
	}
	return value
}

func (s *Store) Probe(ctx context.Context, providerID string) (int, error) {
	spec, ok := s.registry.Get(providerID)
	if !ok {
		return 0, fmt.Errorf("provider %q is not configured", providerID)
	}
	models, err := s.fetchUpstream(ctx, spec)
	if err != nil {
		return 0, err
	}
	return len(models), nil
}

// DiscoverFromProviders fetches fresh upstream catalogs for Formula maintenance.
// It updates only this in-memory Store and never writes the runtime model cache.
func (s *Store) DiscoverFromProviders(ctx context.Context) ([]Model, []DiscoveryFailure) {
	type result struct {
		provider string
		models   []Model
		err      error
	}
	providers := s.registry.All()
	results := make(chan result, len(providers))
	for _, spec := range providers {
		go func(spec provider.Spec) {
			models, err := s.fetchUpstream(ctx, spec)
			results <- result{provider: spec.ID, models: models, err: err}
		}(spec)
	}
	models := make([]Model, 0)
	failures := make([]DiscoveryFailure, 0)
	for range providers {
		result := <-results
		if result.err != nil {
			failures = append(failures, DiscoveryFailure{Provider: result.provider, Error: result.err.Error()})
			continue
		}
		models = append(models, result.models...)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	sort.Slice(failures, func(i, j int) bool { return failures[i].Provider < failures[j].Provider })
	s.set(models, time.Now())
	return models, failures
}

// ProbeModel sends the smallest useful inference request for one advertised function.
func (s *Store) ProbeModel(ctx context.Context, modelID, function string) (ModelProbeResult, error) {
	model, ok := s.Find(modelID)
	if !ok {
		return ModelProbeResult{}, fmt.Errorf("model %q is not in the catalog", modelID)
	}
	spec, ok := s.registry.Get(model.Provider)
	if !ok {
		return ModelProbeResult{}, fmt.Errorf("provider %q is not configured", model.Provider)
	}
	var endpoint string
	var payload []byte
	contentType := "application/json"
	var input map[string]any
	switch function {
	case FunctionChat:
		endpoint = "/chat/completions"
		input = map[string]any{"model": model.UpstreamID, "messages": []map[string]string{{"role": "user", "content": "ping"}}, "max_tokens": 1, "stream": false}
	case FunctionChatTools:
		endpoint = "/chat/completions"
		input = map[string]any{"model": model.UpstreamID, "messages": []map[string]string{{"role": "user", "content": "say ping"}}, "max_tokens": 1, "stream": false, "tools": []map[string]any{{"type": "function", "function": map[string]any{"name": "ping", "description": "return ping", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}}}
	case FunctionImageUnderstanding:
		endpoint = "/chat/completions"
		input = multimodalProbeInput(model.UpstreamID, "image_url", "data:image/png;base64,"+strings.TrimSpace(probePNGBase64))
	case FunctionVideoUnderstanding:
		endpoint = "/chat/completions"
		input = multimodalProbeInput(model.UpstreamID, "video_url", "data:video/mp4;base64,"+strings.TrimSpace(probeMP4Base64))
	case FunctionAudioUnderstanding:
		endpoint = "/chat/completions"
		input = map[string]any{"model": model.UpstreamID, "messages": []map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": "Reply with one word."}, {"type": "input_audio", "input_audio": map[string]any{"data": strings.TrimSpace(probeWAVBase64), "format": "wav"}}}}}, "max_tokens": 1, "stream": false}
	case FunctionEmbedding:
		endpoint = "/embeddings"
		input = map[string]any{"model": model.UpstreamID, "input": "ping"}
	case FunctionRerank:
		endpoint = "/rerank"
		input = map[string]any{"model": model.UpstreamID, "query": "ping", "documents": []string{"ping"}, "top_n": 1}
	case FunctionSpeechToText:
		endpoint = "/audio/transcriptions"
		var err error
		payload, contentType, err = audioProbePayload(model.UpstreamID)
		if err != nil {
			return ModelProbeResult{}, err
		}
	case FunctionTextToSpeech:
		endpoint = "/audio/speech"
		input = map[string]any{"model": model.UpstreamID, "input": "ping", "voice": "alloy", "response_format": "mp3"}
	case FunctionImageGeneration:
		if imageUsesEdit(model) {
			endpoint = "/images/edits"
			var err error
			payload, contentType, err = imageProbePayload(model.UpstreamID)
			if err != nil {
				return ModelProbeResult{}, err
			}
		} else {
			endpoint = "/images/generations"
			input = map[string]any{"model": model.UpstreamID, "prompt": "a dot", "n": 1}
		}
	case FunctionVideoGeneration:
		endpoint = "/videos/generations"
		input = map[string]any{"model": model.UpstreamID, "prompt": "a still black dot", "duration": 1}
		if videoUsesImage(model) {
			input["image"] = "data:image/png;base64," + strings.TrimSpace(probePNGBase64)
		}
	case FunctionModeration:
		endpoint = "/moderations"
		input = map[string]any{"model": model.UpstreamID, "input": "ping"}
	default:
		return ModelProbeResult{}, fmt.Errorf("automatic probing is disabled for function %q", function)
	}
	if payload == nil {
		var err error
		payload, err = json.Marshal(input)
		if err != nil {
			return ModelProbeResult{}, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, spec.APIEndpoint(endpoint), bytes.NewReader(payload))
	if err != nil {
		return ModelProbeResult{}, err
	}
	req.Header.Set("Content-Type", contentType)
	headers := cloneMap(spec.Headers)
	spec.ApplyAuth(headers)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return ModelProbeResult{}, err
	}
	defer resp.Body.Close()
	result := ModelProbeResult{Status: resp.StatusCode}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := upstreamErrorDetail(resp.Body)
		message := resp.Status
		if detail != "" {
			message += ": " + detail
		}
		return result, &ModelProbeError{Status: resp.StatusCode, Message: message}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return result, nil
}

func audioUsesTranscription(model Model) bool {
	for _, endpoint := range model.SupportedEndpoints {
		if strings.Contains(strings.ToLower(endpoint), "transcription") || strings.Contains(strings.ToLower(endpoint), "translation") {
			return true
		}
	}
	id := strings.ToLower(model.UpstreamID)
	return strings.Contains(id, "whisper") || strings.Contains(id, "transcri") || strings.Contains(id, "speech-to-text") || strings.Contains(id, "stt")
}

func multimodalProbeInput(model, contentType, dataURL string) map[string]any {
	return map[string]any{
		"model": model,
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": "Describe this in one word."},
				{"type": contentType, contentType: map[string]any{"url": dataURL}},
			},
		}},
		"max_tokens": 1,
		"stream":     false,
	}
}

func audioProbePayload(model string) ([]byte, string, error) {
	audio, err := base64.StdEncoding.DecodeString(strings.TrimSpace(probeWAVBase64))
	if err != nil {
		return nil, "", fmt.Errorf("decode embedded probe audio: %w", err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", model); err != nil {
		return nil, "", err
	}
	file, err := writer.CreateFormFile("file", "probe.wav")
	if err != nil {
		return nil, "", err
	}
	if _, err := file.Write(audio); err != nil {
		return nil, "", err
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}

func imageUsesEdit(model Model) bool {
	for _, endpoint := range model.SupportedEndpoints {
		lower := strings.ToLower(endpoint)
		if strings.Contains(lower, "image") && (strings.Contains(lower, "edit") || strings.Contains(lower, "variation")) {
			return true
		}
	}
	id := strings.ToLower(model.UpstreamID)
	return strings.Contains(id, "image-edit") || strings.Contains(id, "inpaint")
}

func videoUsesImage(model Model) bool {
	for _, endpoint := range model.SupportedEndpoints {
		lower := strings.ToLower(endpoint)
		if strings.Contains(lower, "image-to-video") || strings.Contains(lower, "i2v") {
			return true
		}
	}
	id := strings.ToLower(model.UpstreamID)
	return strings.Contains(id, "image-to-video") || strings.Contains(id, "i2v")
}

func imageProbePayload(model string) ([]byte, string, error) {
	image, err := base64.StdEncoding.DecodeString(strings.TrimSpace(probePNGBase64))
	if err != nil {
		return nil, "", fmt.Errorf("decode embedded probe image: %w", err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", model); err != nil {
		return nil, "", err
	}
	if err := writer.WriteField("prompt", "keep this image unchanged"); err != nil {
		return nil, "", err
	}
	file, err := writer.CreateFormFile("image", "probe.png")
	if err != nil {
		return nil, "", err
	}
	if _, err := file.Write(image); err != nil {
		return nil, "", err
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}

func (s *Store) Models() []Model {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Model{}, s.models...)
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

type Status struct {
	Count     int        `json:"count"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	CachePath string     `json:"cache_path"`
}

func (s *Store) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	status := Status{Count: len(s.models), CachePath: s.cache}
	if !s.updated.IsZero() {
		updated := s.updated
		status.UpdatedAt = &updated
	}
	return status
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

func (model Model) SupportsFunction(function string) bool {
	return contains(model.Functions, function)
}

func (s *Store) set(models []Model, updated time.Time) {
	s.mu.Lock()
	s.models = models
	s.updated = updated
	s.mu.Unlock()
}

// RemoveModel permanently removes a failed model from the cache for the current
// Formula manifest version. A newer manifest may introduce it again for retesting.
func (s *Store) RemoveModel(modelID string) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	current := s.Models()
	models := make([]Model, 0, len(current))
	removed := false
	for _, model := range current {
		if model.ID == modelID {
			removed = true
			continue
		}
		models = append(models, model)
	}
	if !removed {
		return nil
	}
	s.set(models, time.Now())
	return s.saveCache(models)
}

func (s *Store) loadCache() error {
	data, err := os.ReadFile(s.cache)
	if err != nil {
		return fmt.Errorf("read cache: %w", err)
	}
	var cached cacheFile
	if err := json.Unmarshal(data, &cached); err != nil {
		return fmt.Errorf("decode cache: %w", err)
	}
	if cached.SchemaVersion != 1 || cached.ManifestGeneratedAt != s.manifestGeneratedAt() {
		return errors.New("cache was produced from a different Formula manifest")
	}
	models := make([]Model, 0, len(cached.Models))
	for _, model := range cached.Models {
		if spec, enabled := s.registry.Get(model.Provider); enabled && cacheEligible(spec, model) {
			if model.Type == "" {
				model.Type = classifyID(model.UpstreamID)
				model.Free = true
				model.Capabilities = inferCachedCapabilities(model)
			}
			if len(model.Functions) == 0 {
				model.Functions = inferCachedFunctions(model)
			}
			models = append(models, model)
		}
	}
	updated := time.Now()
	if info, err := os.Stat(s.cache); err == nil {
		updated = info.ModTime()
	}
	s.set(models, updated)
	return nil
}

func (s *Store) saveCache(models []Model) error {
	if err := os.MkdirAll(filepath.Dir(s.cache), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cacheFile{SchemaVersion: 1, ManifestGeneratedAt: s.manifestGeneratedAt(), Models: models}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.cache + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.cache)
}

func (s *Store) manifestGeneratedAt() string {
	var generatedAt string
	for _, spec := range s.registry.All() {
		if spec.ManifestGeneratedAt > generatedAt {
			generatedAt = spec.ManifestGeneratedAt
		}
	}
	return generatedAt
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

func allowed(allowlist, patterns []string, model string) bool {
	if len(allowlist) == 0 && len(patterns) == 0 {
		return true
	}
	for _, allowedModel := range allowlist {
		if strings.EqualFold(allowedModel, model) {
			return true
		}
	}
	model = strings.ToLower(model)
	for _, pattern := range patterns {
		matched, err := path.Match(strings.ToLower(pattern), model)
		if err == nil && matched {
			return true
		}
	}
	return false
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

func inferFunctions(candidate upstreamModel, modelType string, input, output, parameters []string) []string {
	functions := make(map[string]bool)
	add := func(function string) { functions[function] = true }
	for _, endpoint := range candidate.SupportedEndpoints {
		value := strings.ToLower(endpoint)
		switch {
		case strings.Contains(value, "chat/completions") || strings.Contains(value, "responses"):
			add(FunctionChat)
		case strings.Contains(value, "embedding"):
			add(FunctionEmbedding)
		case strings.Contains(value, "rerank"):
			add(FunctionRerank)
		case strings.Contains(value, "audio/transcription") || strings.Contains(value, "audio/translation"):
			add(FunctionSpeechToText)
		case strings.Contains(value, "audio/speech"):
			add(FunctionTextToSpeech)
		case strings.Contains(value, "video"):
			add(FunctionVideoGeneration)
		case strings.Contains(value, "image"):
			add(FunctionImageGeneration)
		case strings.Contains(value, "moderation") || strings.Contains(value, "safety"):
			add(FunctionModeration)
		}
	}
	textOutput := contains(output, "text") || (modelType == "normal" && len(output) == 0)
	if modelType == "normal" {
		add(FunctionChat)
	}
	if textOutput && contains(input, "image") {
		add(FunctionImageUnderstanding)
	}
	if textOutput && contains(input, "video") {
		add(FunctionVideoUnderstanding)
	}
	if textOutput && contains(input, "audio") && functions[FunctionChat] {
		add(FunctionAudioUnderstanding)
	}
	if contains(output, "image") {
		add(FunctionImageGeneration)
	}
	if contains(output, "video") {
		add(FunctionVideoGeneration)
	}
	if contains(output, "audio") {
		add(FunctionTextToSpeech)
	}
	id := strings.ToLower(candidate.ID)
	switch modelType {
	case "embedding":
		add(FunctionEmbedding)
	case "rerank":
		add(FunctionRerank)
	case "image":
		add(FunctionImageGeneration)
	case "video":
		add(FunctionVideoGeneration)
	case "audio":
		switch {
		case strings.Contains(id, "whisper"), strings.Contains(id, "transcri"), strings.Contains(id, "speech-to-text"), strings.Contains(id, "stt"), strings.Contains(id, "asr"):
			add(FunctionSpeechToText)
		case strings.Contains(id, "tts"), strings.Contains(id, "text-to-speech"), strings.Contains(id, "speech-synth"), strings.Contains(id, "voice"):
			add(FunctionTextToSpeech)
		}
	case "moderation":
		add(FunctionModeration)
	}
	if functions[FunctionChat] && (candidate.Tools.Value || candidate.ToolCall.Value || contains(parameters, "tools") || contains(parameters, "tool_choice")) {
		add(FunctionChatTools)
	}
	result := make([]string, 0, len(functions))
	for _, function := range AllFunctions() {
		if functions[function] {
			result = append(result, function)
		}
	}
	return result
}

func inferCachedFunctions(model Model) []string {
	candidate := upstreamModel{ID: model.UpstreamID, Type: model.Type, SupportedEndpoints: model.SupportedEndpoints}
	candidate.Tools = flexibleBool{Value: model.Capabilities.ToolCall, Known: model.Capabilities.ToolCallKnown}
	return inferFunctions(candidate, model.Type, model.InputModalities, model.OutputModalities, model.SupportedParameters)
}

func AllFunctions() []string {
	return []string{
		FunctionChat, FunctionChatTools, FunctionImageUnderstanding, FunctionImageGeneration,
		FunctionVideoUnderstanding, FunctionVideoGeneration, FunctionAudioUnderstanding,
		FunctionSpeechToText, FunctionTextToSpeech, FunctionEmbedding, FunctionRerank, FunctionModeration,
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
