package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sjzsdu/free-router/internal/provider"
)

var explicitUnitPrice = regexp.MustCompile(`(?i)\b(?:priced at|costs?)\s+(?:US\s*)?\$\s*[0-9]`)

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

type ProviderProbeError struct {
	Status  int
	Message string
}

type DiscoveryFailure struct {
	Provider string `json:"provider"`
	Error    string `json:"error"`
}

func (e *ModelProbeError) Error() string    { return e.Message }
func (e *ProviderProbeError) Error() string { return e.Message }

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
		Prompt     flexibleString `json:"prompt"`
		Completion flexibleString `json:"completion"`
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
	registry             *provider.Registry
	cache                string
	client               *http.Client
	refreshMu            sync.Mutex
	mu                   sync.RWMutex
	models               []Model
	quarantine           map[string]string
	verifiedTools        map[string]bool
	verifiedCapabilities map[string]CapabilityVerification
	updated              time.Time
}

type CapabilityVerification struct {
	Model                   string    `json:"model"`
	Capability              string    `json:"capability"`
	CatalogModelFingerprint string    `json:"catalog_model_fingerprint"`
	ModelFingerprint        string    `json:"model_fingerprint"`
	CheckedAt               time.Time `json:"checked_at"`
	LatencyMS               float64   `json:"latency_ms,omitempty"`
}

type cacheFile struct {
	SchemaVersion        int                               `json:"schema_version"`
	CatalogFingerprint   string                            `json:"catalog_fingerprint"`
	Models               []Model                           `json:"models"`
	Quarantined          map[string]string                 `json:"quarantined,omitempty"`
	VerifiedTools        map[string]bool                   `json:"verified_tools,omitempty"`
	VerifiedCapabilities map[string]CapabilityVerification `json:"verified_capabilities,omitempty"`
}

func New(registry *provider.Registry, cache string, client *http.Client) *Store {
	return &Store{
		registry: registry, cache: cache, client: client,
		quarantine: make(map[string]string), verifiedTools: make(map[string]bool),
		verifiedCapabilities: make(map[string]CapabilityVerification),
	}
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
	for _, spec := range s.registry.CatalogAll() {
		merged = append(merged, modelsFromDiscovery(spec)...)
	}
	merged = s.applyVerifiedTools(merged)
	merged = s.applyQuarantine(merged)
	sort.Slice(merged, func(i, j int) bool { return merged[i].ID < merged[j].ID })
	s.pruneCapabilityVerifications(merged)
	s.set(merged, time.Now())
	return s.saveCache(merged)
}

func cacheEligible(spec provider.Spec, model Model) bool {
	for _, discovered := range spec.DiscoveredModels {
		if discovered.ID == model.UpstreamID {
			return true
		}
	}
	return false
}

// RefreshProvider reapplies one Provider's maintained inventory without network discovery.
func (s *Store) RefreshProvider(ctx context.Context, providerID string) error {
	_ = ctx
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	spec, ok := s.registry.CatalogGet(providerID)
	if !ok {
		return fmt.Errorf("provider %q is not in the maintained model catalog", providerID)
	}
	models := modelsFromDiscovery(spec)
	models = s.applyVerifiedTools(models)
	models = s.applyQuarantine(models)
	merged := make([]Model, 0, len(s.Models())+len(models))
	for _, model := range s.Models() {
		if model.Provider != providerID {
			merged = append(merged, model)
		}
	}
	merged = append(merged, models...)
	sort.Slice(merged, func(i, j int) bool { return merged[i].ID < merged[j].ID })
	s.pruneCapabilityVerifications(merged)
	s.set(merged, time.Now())
	if err := s.saveCache(merged); err != nil {
		return fmt.Errorf("save model cache: %w", err)
	}
	return nil
}

// fetchUpstream is reserved for the maintainer discovery command used by the Formula.
// Runtime catalog refreshes never call it.
func (s *Store) fetchUpstream(ctx context.Context, spec provider.Spec) ([]Model, error) {
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
		message := "models endpoint returned " + resp.Status
		if detail != "" {
			message += ": " + detail
		}
		return nil, &ProviderProbeError{Status: resp.StatusCode, Message: message}
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
		if candidate.ID == "" {
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
		// An official catalog may include models for endpoints free-router cannot
		// route safely. Keep those models out of the authoritative inventory
		// instead of emitting a structurally invalid manifest or guessing.
		if len(functions) == 0 {
			continue
		}
		if spec.FreeModelPolicy == "zero-price" {
			if !(candidate.Pricing.Prompt.ZeroValue() && candidate.Pricing.Completion.ZeroValue()) {
				continue
			}
			// Some catalogs expose zero token prices while charging per generated
			// image, clip, song, or request. An explicit unit price in the official
			// model description overrides the incomplete token-price fields.
			if explicitUnitPrice.MatchString(candidate.Description) {
				continue
			}
		}
		models = append(models, Model{
			ID: spec.ID + "/" + candidate.ID, Provider: spec.ID, UpstreamID: candidate.ID,
			Name: candidate.Name, Description: candidate.Description, OwnedBy: candidate.OwnedBy, Created: candidate.Created,
			Type: modelType, Functions: functions, Free: true, Tier: spec.Tier,
			ContextLength: contextLength, MaxOutputTokens: maxOutputTokens,
			InputModalities: input, OutputModalities: output, SupportedParameters: parameters,
			SupportedEndpoints: candidate.SupportedEndpoints, Capabilities: capabilities,
			Pricing: Pricing{Prompt: candidate.Pricing.Prompt.Value, Completion: candidate.Pricing.Completion.Value},
		})
	}
	if len(models) == 0 {
		if spec.FreeModelPolicy == "zero-price" && len(candidates) > 0 {
			return []Model{}, nil
		}
		return nil, errors.New("no eligible models returned")
	}
	return models, nil
}

func modelsFromDiscovery(spec provider.Spec) []Model {
	models := make([]Model, 0, len(spec.DiscoveredModels))
	for _, candidate := range spec.DiscoveredModels {
		if candidate.ID == "" {
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
		toolCall, toolCallKnown := discoveredToolCapability(candidate, functions)
		if advertisesToolCandidate(functions, toolCall, toolCallKnown) && !contains(functions, FunctionChatTools) {
			functions = append(functions, FunctionChatTools)
		}
		capabilities := Capabilities{
			ToolCall:       toolCall,
			ToolCallKnown:  toolCallKnown,
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

func discoveredToolCapability(candidate provider.DiscoveredModel, functions []string) (bool, bool) {
	supported := contains(candidate.Functions, FunctionChatTools) ||
		contains(candidate.SupportedParameters, "tools") ||
		contains(candidate.SupportedParameters, "tool_choice")
	if supported {
		return true, true
	}
	if len(candidate.SupportedParameters) > 0 {
		return false, true
	}
	if contains(functions, FunctionChat) {
		return true, true
	}
	return false, false
}

func advertisesToolCandidate(functions []string, supported, known bool) bool {
	return contains(functions, FunctionChat) && (supported || !known)
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
	matched := 0
	for _, model := range models {
		if cacheEligible(spec, model) {
			matched++
		}
	}
	return matched, nil
}

// DiscoverFromProviders fetches fresh upstream catalogs for Formula maintenance.
// It updates only this in-memory Store and never writes the runtime model cache.
func (s *Store) DiscoverFromProviders(ctx context.Context, providerIDs ...string) ([]Model, []DiscoveryFailure) {
	type result struct {
		provider string
		models   []Model
		err      error
	}
	providers := s.registry.All()
	if len(providerIDs) > 0 && providerIDs[0] != "" && providerIDs[0] != "all" {
		targets := make(map[string]struct{}, len(providerIDs))
		for _, providerID := range providerIDs {
			targets[providerID] = struct{}{}
		}
		selected := make([]provider.Spec, 0, len(providerIDs))
		for _, spec := range providers {
			if _, ok := targets[spec.ID]; ok {
				selected = append(selected, spec)
			}
		}
		providers = selected
	}
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

// DiscoverFromSpecs fetches explicit provider catalog specifications. Formula
// maintenance uses this to try official /models endpoints even when inference
// credentials are absent; authenticated endpoints then report a normal fetch
// failure instead of silently falling back to agent discovery.
func (s *Store) DiscoverFromSpecs(ctx context.Context, providers []provider.Spec) ([]Model, []DiscoveryFailure) {
	type result struct {
		provider string
		models   []Model
		err      error
	}
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
		item := <-results
		if item.err != nil {
			failures = append(failures, DiscoveryFailure{Provider: item.provider, Error: item.err.Error()})
			continue
		}
		models = append(models, item.models...)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	sort.Slice(failures, func(i, j int) bool { return failures[i].Provider < failures[j].Provider })
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
		input = map[string]any{"model": model.UpstreamID, "messages": []map[string]string{{"role": "user", "content": "Call the ping tool. Do not answer directly."}}, "max_tokens": 16, "stream": false, "tools": []map[string]any{{"type": "function", "function": map[string]any{"name": "ping", "description": "return ping", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}}}
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return result, err
	}
	if function == FunctionChatTools && !containsToolCall(body) {
		return result, &ModelProbeError{Status: resp.StatusCode, Message: "successful response did not contain a tool call"}
	}
	return result, nil
}

func containsToolCall(body []byte) bool {
	var response struct {
		Choices []struct {
			Message struct {
				ToolCalls    json.RawMessage `json:"tool_calls"`
				FunctionCall json.RawMessage `json:"function_call"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &response) != nil {
		return false
	}
	for _, choice := range response.Choices {
		if hasJSONValue(choice.Message.ToolCalls) || hasJSONValue(choice.Message.FunctionCall) {
			return true
		}
	}
	return false
}

func hasJSONValue(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) && !bytes.Equal(trimmed, []byte("[]")) && !bytes.Equal(trimmed, []byte("{}"))
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

func (s *Store) ProviderConfigured(providerID string) bool {
	_, ok := s.registry.Get(providerID)
	return ok
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

// RemoveModel removes a failed model and records its metadata fingerprint. Formula
// updates do not revive the same broken model; changed metadata makes it eligible
// for retesting.
func (s *Store) RemoveModel(modelID string) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	current := s.Models()
	models := make([]Model, 0, len(current))
	removed := false
	for _, model := range current {
		if model.ID == modelID {
			removed = true
			s.mu.Lock()
			s.quarantine[model.ID] = modelFingerprint(model)
			s.mu.Unlock()
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

// RestoreModel clears a model quarantine after an explicit operator reset and
// re-adds the current Formula definition so it can be probed again.
func (s *Store) RestoreModel(modelID string) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	s.mu.RLock()
	_, quarantined := s.quarantine[modelID]
	s.mu.RUnlock()
	if !quarantined {
		return nil
	}
	var restored Model
	found := false
	for _, spec := range s.registry.CatalogAll() {
		for _, model := range modelsFromDiscovery(spec) {
			if model.ID == modelID {
				restored = model
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		return fmt.Errorf("model %q is no longer in the maintained model catalog", modelID)
	}
	models := s.Models()
	for _, model := range models {
		if model.ID == modelID {
			return nil
		}
	}
	s.mu.Lock()
	delete(s.quarantine, modelID)
	s.mu.Unlock()
	models = append(models, restored)
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
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
	if cached.SchemaVersion != 4 {
		return errors.New("unsupported model cache schema")
	}
	s.mu.Lock()
	s.quarantine = cloneMap(cached.Quarantined)
	s.verifiedTools = cloneBoolMap(cached.VerifiedTools)
	s.verifiedCapabilities = cloneCapabilityVerifications(cached.VerifiedCapabilities)
	s.mu.Unlock()
	if cached.CatalogFingerprint != s.catalogFingerprint() {
		return errors.New("cache was produced from a different free model manifest")
	}
	models := make([]Model, 0, len(cached.Models))
	for _, model := range cached.Models {
		if spec, exists := s.registry.CatalogGet(model.Provider); exists && cacheEligible(spec, model) {
			for _, candidate := range spec.DiscoveredModels {
				if candidate.ID != model.UpstreamID {
					continue
				}
				model.Functions = withoutString(model.Functions, FunctionChatTools)
				model.Capabilities.ToolCall, model.Capabilities.ToolCallKnown = discoveredToolCapability(candidate, model.Functions)
				if advertisesToolCandidate(model.Functions, model.Capabilities.ToolCall, model.Capabilities.ToolCallKnown) {
					model.Functions = append(model.Functions, FunctionChatTools)
				}
				break
			}
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
	models = s.applyVerifiedTools(models)
	s.pruneCapabilityVerifications(models)
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
	s.mu.RLock()
	quarantined := cloneMap(s.quarantine)
	s.mu.RUnlock()
	s.mu.RLock()
	verifiedTools := cloneBoolMap(s.verifiedTools)
	verifiedCapabilities := cloneCapabilityVerifications(s.verifiedCapabilities)
	s.mu.RUnlock()
	data, err := json.MarshalIndent(cacheFile{
		SchemaVersion: 4, CatalogFingerprint: s.catalogFingerprint(), Models: models,
		Quarantined: quarantined, VerifiedTools: verifiedTools, VerifiedCapabilities: verifiedCapabilities,
	}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.cache + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.cache)
}

// RecordToolSupport promotes a model after a successful tool-call probe. The
// verification is persisted separately from Formula metadata so future catalog
// refreshes can retain evidence that the upstream catalog did not provide.
func (s *Store) RecordToolSupport(modelID string) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	models := s.Models()
	found := false
	for index := range models {
		if models[index].ID != modelID {
			continue
		}
		found = true
		models[index].Capabilities.ToolCall = true
		models[index].Capabilities.ToolCallKnown = true
		if models[index].SupportsFunction(FunctionChat) && !models[index].SupportsFunction(FunctionChatTools) {
			models[index].Functions = append(models[index].Functions, FunctionChatTools)
		}
		break
	}
	if !found {
		return fmt.Errorf("model %q is not in the catalog", modelID)
	}
	s.mu.Lock()
	s.verifiedTools[modelID] = true
	s.mu.Unlock()
	s.set(models, time.Now())
	return s.saveCache(models)
}

func (s *Store) applyVerifiedTools(models []Model) []Model {
	s.mu.RLock()
	verified := cloneBoolMap(s.verifiedTools)
	s.mu.RUnlock()
	for index := range models {
		if !verified[models[index].ID] {
			continue
		}
		models[index].Capabilities.ToolCall = true
		models[index].Capabilities.ToolCallKnown = true
		if models[index].SupportsFunction(FunctionChat) && !models[index].SupportsFunction(FunctionChatTools) {
			models[index].Functions = append(models[index].Functions, FunctionChatTools)
		}
	}
	return models
}

// RecordCapabilityVerification persists a successful model capability probe.
// The model fingerprint makes the result reusable across restarts while
// preventing stale verification from surviving routing-relevant model changes.
func (s *Store) RecordCapabilityVerification(modelID, capability string, checkedAt time.Time, latency time.Duration) error {
	model, ok := findModel(s.Models(), modelID)
	if !ok {
		return fmt.Errorf("model %q is not in the catalog", modelID)
	}
	return s.RecordModelCapabilityVerification(model, capability, checkedAt, latency)
}

func (s *Store) RecordModelCapabilityVerification(model Model, capability string, checkedAt time.Time, latency time.Duration) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	catalogModel, ok := findModel(s.Models(), model.ID)
	if !ok {
		return fmt.Errorf("model %q is not in the catalog", model.ID)
	}
	if !model.SupportsFunction(capability) {
		return fmt.Errorf("model %q does not advertise capability %q", model.ID, capability)
	}
	if checkedAt.IsZero() {
		checkedAt = time.Now()
	}
	verification := CapabilityVerification{
		Model: model.ID, Capability: capability,
		CatalogModelFingerprint: modelFingerprint(catalogModel), ModelFingerprint: modelFingerprint(model),
		CheckedAt: checkedAt, LatencyMS: float64(latency.Microseconds()) / 1000,
	}
	s.mu.Lock()
	s.verifiedCapabilities[capabilityVerificationKey(model.ID, capability)] = verification
	s.mu.Unlock()
	return s.saveCache(s.Models())
}

// ResetCapabilityVerification invalidates persisted probe success for one
// capability, or for every capability when capability is empty.
func (s *Store) ResetCapabilityVerification(modelID, capability string) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	s.mu.Lock()
	if capability != "" {
		delete(s.verifiedCapabilities, capabilityVerificationKey(modelID, capability))
	} else {
		for key, verification := range s.verifiedCapabilities {
			if verification.Model == modelID {
				delete(s.verifiedCapabilities, key)
			}
		}
	}
	s.mu.Unlock()
	return s.saveCache(s.Models())
}

func (s *Store) CapabilityVerified(modelID, capability string) bool {
	s.mu.RLock()
	model, ok := findModel(s.models, modelID)
	s.mu.RUnlock()
	if !ok {
		return false
	}
	return s.ModelCapabilityVerified(model, capability)
}

func (s *Store) ModelCapabilityVerified(model Model, capability string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	catalogModel, ok := findModel(s.models, model.ID)
	if !ok {
		return false
	}
	verification, ok := s.verifiedCapabilities[capabilityVerificationKey(model.ID, capability)]
	return ok &&
		verification.CatalogModelFingerprint == modelFingerprint(catalogModel) &&
		verification.ModelFingerprint == modelFingerprint(model)
}

func (s *Store) CapabilityVerifications() []CapabilityVerification {
	s.mu.RLock()
	defer s.mu.RUnlock()

	models := make(map[string]Model, len(s.models))
	for _, model := range s.models {
		models[model.ID] = model
	}
	result := make([]CapabilityVerification, 0, len(s.verifiedCapabilities))
	for _, verification := range s.verifiedCapabilities {
		model, ok := models[verification.Model]
		if !ok || verification.CatalogModelFingerprint != modelFingerprint(model) {
			continue
		}
		result = append(result, verification)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Model == result[j].Model {
			return result[i].Capability < result[j].Capability
		}
		return result[i].Model < result[j].Model
	})
	return result
}

func (s *Store) pruneCapabilityVerifications(models []Model) {
	known := make(map[string]Model, len(models))
	for _, model := range models {
		known[model.ID] = model
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, verification := range s.verifiedCapabilities {
		model, ok := known[verification.Model]
		if !ok || verification.CatalogModelFingerprint != modelFingerprint(model) {
			delete(s.verifiedCapabilities, key)
		}
	}
}

func capabilityVerificationKey(model, capability string) string {
	return model + "\x00" + capability
}

func findModel(models []Model, id string) (Model, bool) {
	for _, model := range models {
		if model.ID == id {
			return model, true
		}
	}
	return Model{}, false
}

func (s *Store) applyQuarantine(models []Model) []Model {
	s.mu.RLock()
	defer s.mu.RUnlock()
	filtered := make([]Model, 0, len(models))
	for _, model := range models {
		fingerprint, quarantined := s.quarantine[model.ID]
		if !quarantined {
			filtered = append(filtered, model)
			continue
		}
		if fingerprint == modelFingerprint(model) {
			continue
		}
		filtered = append(filtered, model)
	}
	return filtered
}

func modelFingerprint(model Model) string {
	type fingerprint struct {
		ID                  string   `json:"id"`
		Type                string   `json:"type"`
		Functions           []string `json:"functions"`
		ContextLength       int      `json:"context_length"`
		MaxOutputTokens     int      `json:"max_output_tokens"`
		InputModalities     []string `json:"input_modalities"`
		OutputModalities    []string `json:"output_modalities"`
		SupportedParameters []string `json:"supported_parameters"`
		SupportedEndpoints  []string `json:"supported_endpoints"`
		Pricing             Pricing  `json:"pricing"`
	}
	data, _ := json.Marshal(fingerprint{
		ID: model.ID, Type: model.Type, Functions: sortedStrings(model.Functions),
		ContextLength: model.ContextLength, MaxOutputTokens: model.MaxOutputTokens,
		InputModalities: sortedStrings(model.InputModalities), OutputModalities: sortedStrings(model.OutputModalities),
		SupportedParameters: sortedStrings(model.SupportedParameters), SupportedEndpoints: sortedStrings(model.SupportedEndpoints),
		Pricing: model.Pricing,
	})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (s *Store) catalogFingerprint() string {
	type catalogEntry struct {
		Provider string   `json:"provider"`
		Models   []string `json:"models"`
	}
	entries := make([]catalogEntry, 0)
	for _, spec := range s.registry.CatalogAll() {
		entry := catalogEntry{Provider: spec.ID}
		for _, model := range modelsFromDiscovery(spec) {
			entry.Models = append(entry.Models, modelFingerprint(model))
		}
		entries = append(entries, entry)
	}
	data, _ := json.Marshal(entries)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func sortedStrings(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
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

func cloneBoolMap(source map[string]bool) map[string]bool {
	result := make(map[string]bool, len(source)+1)
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneCapabilityVerifications(source map[string]CapabilityVerification) map[string]CapabilityVerification {
	result := make(map[string]CapabilityVerification, len(source)+1)
	for key, value := range source {
		result[key] = value
	}
	return result
}

func withoutString(values []string, unwanted string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != unwanted {
			result = append(result, value)
		}
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
		case strings.Contains(value, "audio") && strings.Contains(value, "{text}"):
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

type flexibleString struct {
	Value string
	Known bool
	Zero  bool
}

func (value *flexibleString) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		value.Value = text
		value.Known = true
		parsed, err := strconv.ParseFloat(text, 64)
		value.Zero = err == nil && parsed == 0
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err == nil {
		value.Value = number.String()
		value.Known = true
		parsed, err := strconv.ParseFloat(number.String(), 64)
		value.Zero = err == nil && parsed == 0
		return nil
	}
	var tiers []struct {
		Price flexibleString `json:"price"`
	}
	if err := json.Unmarshal(data, &tiers); err == nil && len(tiers) > 0 {
		value.Known = true
		value.Zero = true
		for _, tier := range tiers {
			if !tier.Price.Known || !tier.Price.Zero {
				value.Zero = false
				break
			}
		}
	}
	return nil
}

func (value flexibleString) ZeroValue() bool { return value.Known && value.Zero }

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
