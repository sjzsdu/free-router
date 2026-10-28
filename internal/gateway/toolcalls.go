package gateway

import (
	"bytes"
	"encoding/json"
)

// Irregularities the gateway reports when an upstream returns tool calls in a
// shape that OpenAI clients do not expect.
//
// Nothing here is repaired. Successful bodies are forwarded byte for byte
// because free providers disagree on the exact shape, and rewriting one into
// another shape would break the sources that already work. What was missing was
// observability: a malformed tool_calls payload reached the caller as a plain
// status=200 with no trace on the gateway side, so the failure surfaced in the
// client's own logs with nothing to correlate against.
const (
	ToolCallArgumentsNotString = "arguments-not-string"
	ToolCallMissingID          = "missing-id"
	ToolCallMissingIndex       = "missing-index"
	ToolCallDuplicateIndex     = "duplicate-index"
	ToolCallNotArray           = "tool-calls-not-array"
)

// inspectToolCalls reports non-standard tool_calls shapes in one upstream body.
// It returns nil for bodies without tool calls and for bodies too large to
// inspect.
//
// The checks are gated behind cheap prefix/substring tests: this runs on every
// successful response, including image and audio payloads that are neither JSON
// nor relevant here.
func inspectToolCalls(body []byte, eventStream bool) []string {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 || !bytes.Contains(body, []byte(`"tool_calls"`)) {
		return nil
	}
	if eventStream {
		if !bytes.HasPrefix(trimmed, []byte("data:")) && !bytes.HasPrefix(trimmed, []byte("event:")) && !bytes.HasPrefix(trimmed, []byte("id:")) {
			return nil
		}
		return inspectStreamToolCalls(body)
	}
	if trimmed[0] != '{' {
		// Binary or non-JSON payload (images, audio, speech): nothing to inspect.
		return nil
	}
	return inspectJSONToolCalls(body)
}

type jsonToolCallEnvelope struct {
	Choices []struct {
		Message struct {
			ToolCalls json.RawMessage `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
}

type streamToolCallEnvelope struct {
	Choices []struct {
		Delta struct {
			ToolCalls json.RawMessage `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
}

// inspectJSONToolCalls checks a buffered chat completion body.
func inspectJSONToolCalls(body []byte) []string {
	var envelope jsonToolCallEnvelope
	if json.Unmarshal(body, &envelope) != nil {
		return nil
	}
	var issues []string
	for _, choice := range envelope.Choices {
		issues = append(issues, inspectToolCallList(choice.Message.ToolCalls, false)...)
	}
	return dedupeStrings(issues)
}

// inspectStreamToolCalls checks every data: frame of an SSE response.
func inspectStreamToolCalls(body []byte) []string {
	var issues []string
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var envelope streamToolCallEnvelope
		if json.Unmarshal(payload, &envelope) != nil {
			continue
		}
		for _, choice := range envelope.Choices {
			issues = append(issues, inspectToolCallList(choice.Delta.ToolCalls, true)...)
		}
	}
	return dedupeStrings(issues)
}

func inspectToolCallList(raw json.RawMessage, streaming bool) []string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	var calls []struct {
		ID       string `json:"id"`
		Index    *int   `json:"index"`
		Function struct {
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(trimmed, &calls); err != nil {
		return []string{ToolCallNotArray}
	}
	var issues []string
	seenIndex := make(map[int]bool, len(calls))
	for _, call := range calls {
		if arguments := bytes.TrimSpace(call.Function.Arguments); len(arguments) > 0 && !bytes.Equal(arguments, []byte("null")) {
			if arguments[0] != '"' {
				issues = append(issues, ToolCallArgumentsNotString)
			}
		}
		if !streaming && call.ID == "" {
			issues = append(issues, ToolCallMissingID)
		}
		if call.Index == nil {
			// Streaming deltas are assembled by index; without one the client
			// accumulates every fragment into a separate tool call.
			if streaming {
				issues = append(issues, ToolCallMissingIndex)
			}
			continue
		}
		if seenIndex[*call.Index] {
			issues = append(issues, ToolCallDuplicateIndex)
		}
		seenIndex[*call.Index] = true
	}
	return issues
}

func dedupeStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}
