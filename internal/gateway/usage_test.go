package gateway

import "testing"

func TestUsageCollectorParsesOpenAIJSON(t *testing.T) {
	collector := &usageCollector{}
	collector.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":5,"total_tokens":17}}`))
	usage := collector.Usage(false)
	if usage == nil || usage.InputTokens != 12 || usage.OutputTokens != 5 || usage.TotalTokens != 17 {
		t.Fatalf("usage=%+v", usage)
	}
}

func TestUsageCollectorParsesStreamingUsage(t *testing.T) {
	collector := &usageCollector{}
	collector.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
	collector.Write([]byte("data: {\"choices\":[],\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}\n\ndata: [DONE]\n\n"))
	usage := collector.Usage(true)
	if usage == nil || usage.InputTokens != 7 || usage.OutputTokens != 3 {
		t.Fatalf("usage=%+v", usage)
	}
}

func TestUsageCollectorTreatsAbsentAndOversizedUsageAsMissing(t *testing.T) {
	collector := &usageCollector{}
	collector.Write([]byte(`{"choices":[]}`))
	if usage := collector.Usage(false); usage != nil {
		t.Fatalf("unexpected usage=%+v", usage)
	}
	collector = &usageCollector{}
	collector.Write(make([]byte, maxUsageCaptureBytes+1))
	if usage := collector.Usage(false); usage != nil {
		t.Fatalf("unexpected truncated usage=%+v", usage)
	}
}
