package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/sjzsdu/free-router/internal/provider"
)

// ProbeRunner builds and executes minimal capability-specific inference
// requests. It is independent from catalog persistence and publication.
type ProbeRunner struct {
	client *http.Client
}

func NewProbeRunner(client *http.Client) *ProbeRunner { return &ProbeRunner{client: client} }

func (p *ProbeRunner) Run(ctx context.Context, model Model, spec provider.Spec, function string) (ModelProbeResult, error) {
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
	resp, err := p.client.Do(req)
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
