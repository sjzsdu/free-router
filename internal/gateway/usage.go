package gateway

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/sjzsdu/free-router/internal/statistics"
)

const maxUsageCaptureBytes = 4 << 20

type usageCollector struct {
	data      []byte
	truncated bool
}

func (c *usageCollector) Write(p []byte) {
	if c.truncated {
		return
	}
	remaining := maxUsageCaptureBytes - len(c.data)
	if remaining <= 0 {
		c.truncated = true
		return
	}
	if len(p) > remaining {
		c.data = append(c.data, p[:remaining]...)
		c.truncated = true
		return
	}
	c.data = append(c.data, p...)
}

func (c *usageCollector) Usage(eventStream bool) *statistics.Usage {
	if c.truncated || len(c.data) == 0 {
		return nil
	}
	if !eventStream {
		return parseUsageObject(c.data)
	}
	var latest *statistics.Usage
	for _, line := range bytes.Split(c.data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		if usage := parseUsageObject(payload); usage != nil {
			latest = usage
		}
	}
	return latest
}

func parseUsageObject(data []byte) *statistics.Usage {
	var envelope struct {
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(data, &envelope) != nil || len(envelope.Usage) == 0 || string(envelope.Usage) == "null" {
		return nil
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(envelope.Usage, &raw) != nil {
		return nil
	}
	input, inputOK := usageNumber(raw, "prompt_tokens", "input_tokens")
	output, outputOK := usageNumber(raw, "completion_tokens", "output_tokens")
	total, totalOK := usageNumber(raw, "total_tokens")
	if !inputOK && !outputOK && !totalOK {
		return nil
	}
	return &statistics.Usage{InputTokens: input, OutputTokens: output, TotalTokens: total}
}

func usageNumber(raw map[string]json.RawMessage, keys ...string) (uint64, bool) {
	for _, key := range keys {
		value, ok := raw[key]
		if !ok {
			continue
		}
		var number uint64
		if json.Unmarshal(value, &number) == nil {
			return number, true
		}
		var floatValue float64
		if json.Unmarshal(value, &floatValue) == nil && floatValue >= 0 {
			return uint64(floatValue), true
		}
	}
	return 0, false
}

func responseMayReportUsage(contentType string) bool {
	contentType = strings.ToLower(contentType)
	return strings.Contains(contentType, "application/json") || strings.Contains(contentType, "text/event-stream")
}
