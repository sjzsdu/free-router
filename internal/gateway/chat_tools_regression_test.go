package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjzsdu/free-router/internal/catalog"
	"github.com/sjzsdu/free-router/internal/health"
	"github.com/sjzsdu/free-router/internal/provider"
	"github.com/sjzsdu/free-router/internal/routing"
)

// These tests pin the chat-tools behaviour that used to fail silently: an empty
// tool pool degraded to plain chat without telling anyone, a dialog that already
// carried tool results was replayed without its tools, and "tools": [] was
// treated as a tool request.

type upstreamCall struct {
	Model string
	Tools bool
	Body  string
}

// upstreamSpy records what the gateway actually sent upstream. Every assertion
// about routing behaviour is easier to trust than one about response headers
// alone, because the request body is what the provider sees.
type upstreamSpy struct {
	mu      sync.Mutex
	calls   []upstreamCall
	respond func(model string, body map[string]any) *http.Response
}

func (s *upstreamSpy) recorded() []upstreamCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]upstreamCall(nil), s.calls...)
}

func (s *upstreamSpy) modelsCalled(capability string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, call := range s.calls {
		if call.Model == capability {
			count++
		}
	}
	return count
}

func (s *upstreamSpy) client(modelsJSON string) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/models":
			return testResponse(http.StatusOK, modelsJSON), nil
		case "/chat/completions":
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			model, _ := body["model"].(string)
			_, hasTools := body["tools"]
			s.mu.Lock()
			s.calls = append(s.calls, upstreamCall{Model: model, Tools: hasTools, Body: string(raw)})
			s.mu.Unlock()
			if s.respond == nil {
				return testResponse(http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`), nil
			}
			return s.respond(model, body), nil
		default:
			return testResponse(http.StatusNotFound, "not found"), nil
		}
	})}
}

type gatewayFixture struct {
	models string
	// respond decides the upstream answer for one chat/completions call.
	respond func(model string, body map[string]any) *http.Response
	// configure mutates the route configuration before the gateway is built.
	configure func(*routing.Config)
	// verifiedOnly limits which capabilities are recorded as probed. Nil means
	// "every capability the model advertises", which is what a completed
	// "probe all" leaves behind.
	verifiedOnly func(catalog.Model) []string
	maxAttempts  int
	apiToken     string
}

// newGatewayForTest builds a gateway backed by a spy upstream and a route
// configuration on disk, mirroring how a real deployment is assembled.
func newGatewayForTest(t *testing.T, fixture gatewayFixture) (*Gateway, *upstreamSpy, *health.Tracker) {
	t.Helper()
	clearBuiltinKeys(t)
	spy := &upstreamSpy{respond: fixture.respond}
	client := spy.client(fixture.models)
	registry, err := provider.NewRegistry(`[{"id":"test","base_url":"https://test.invalid","api_key":"test"}]`)
	if err != nil {
		t.Fatal(err)
	}
	store := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), client)
	models, failures := store.DiscoverFromProviders(context.Background())
	if len(failures) > 0 || len(models) == 0 {
		t.Fatalf("models=%#v failures=%#v", models, failures)
	}
	for _, model := range models {
		capabilities := model.Functions
		if fixture.verifiedOnly != nil {
			capabilities = fixture.verifiedOnly(model)
		}
		for _, capability := range capabilities {
			if err := store.RecordCapabilityVerification(model.ID, capability, time.Now(), time.Millisecond); err != nil {
				t.Fatal(err)
			}
		}
	}
	routes, err := routing.New(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fixture.configure != nil {
		config := routes.Config()
		fixture.configure(&config)
		if err := routes.Update(config); err != nil {
			t.Fatal(err)
		}
	}
	tracker := health.New()
	attempts := fixture.maxAttempts
	if attempts <= 0 {
		attempts = 3
	}
	gateway := New(store, registry, Config{MaxAttempts: attempts, Routes: routes, Health: tracker, APIToken: fixture.apiToken}, client)
	return gateway, spy, tracker
}

func postChat(handler *Gateway, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	return recorder
}

func getModels(handler *Gateway) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	return recorder
}

func modelList(t *testing.T, recorder *httptest.ResponseRecorder) []struct {
	ID        string `json:"id"`
	Available *bool  `json:"available"`
} {
	t.Helper()
	var list struct {
		Data []struct {
			ID        string `json:"id"`
			Available *bool  `json:"available"`
		} `json:"data"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	return list.Data
}

const toolCallRequest = `{"model":"chat-tools","messages":[{"role":"user","content":"weather?"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`

// A capability with declared models but no probe evidence stays visible so a
// client platform can validate the configured model name; "available": false is
// what tells it the pool is empty.
func TestModelsEndpointListsChatToolsWhilePoolIsUnverified(t *testing.T) {
	handler, _, _ := newGatewayForTest(t, gatewayFixture{
		models: `{"data":[{"id":"tool-chat","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]},"supported_parameters":["tools"]}]}`,
		verifiedOnly: func(model catalog.Model) []string {
			return []string{catalog.FunctionChat}
		},
	})

	data := modelList(t, getModels(handler))
	available := make(map[string]bool, len(data))
	for _, model := range data {
		if model.Available == nil {
			t.Fatalf("model %q has no available flag: %#v", model.ID, data)
		}
		available[model.ID] = *model.Available
	}
	if got, ok := available[catalog.FunctionChatTools]; !ok || got {
		t.Fatalf("chat-tools must be listed as unavailable, not missing: %#v", data)
	}
	if got, ok := available[catalog.FunctionChat]; !ok || !got {
		t.Fatalf("verified chat capability must be available: %#v", data)
	}
}

// The degradation must be visible to the caller: a 200 without tool_calls is
// indistinguishable from a model that chose not to use a tool.
func TestLooseFallbackAnnouncesDroppedTools(t *testing.T) {
	handler, spy, _ := newGatewayForTest(t, gatewayFixture{
		models: `{"data":[{"id":"plain-chat","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]},"supported_parameters":["temperature"]}]}`,
	})

	recorder := postChat(handler, toolCallRequest)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("X-Free-Router-Phase"); got != phaseLoose {
		t.Fatalf("X-Free-Router-Phase = %q, want %q", got, phaseLoose)
	}
	if got := recorder.Header().Get("X-Free-Router-Tools-Dropped"); got != "true" {
		t.Fatalf("X-Free-Router-Tools-Dropped = %q, want true", got)
	}
	calls := spy.recorded()
	if len(calls) != 1 || calls[0].Tools {
		t.Fatalf("loose replay still carried tools: %#v", calls)
	}
}

// When the loose replay fails too, nothing was dropped from the caller's point
// of view, so the terminal error must not claim a degradation.
func TestLooseFallbackFailureOmitsDegradationHeaders(t *testing.T) {
	handler, _, _ := newGatewayForTest(t, gatewayFixture{
		models: `{"data":[{"id":"plain-chat","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]},"supported_parameters":["temperature"]}]}`,
		respond: func(string, map[string]any) *http.Response {
			return testResponse(http.StatusBadGateway, `{"error":{"message":"upstream broken"}}`)
		},
	})

	recorder := postChat(handler, toolCallRequest)
	if recorder.Code == http.StatusOK {
		t.Fatalf("status=200 body=%s", recorder.Body.String())
	}
	if got := recorder.Header().Get("X-Free-Router-Tools-Dropped"); got != "" {
		t.Fatalf("failed request claimed dropped tools: %q", got)
	}
	if got := recorder.Header().Get("X-Free-Router-Phase"); got != "" {
		t.Fatalf("failed request claimed a loose phase: %q", got)
	}
}

// Replaying a dialog that already carries tool results without its tools field
// is invalid upstream, so the loose fallback must not run at all.
func TestLooseFallbackSkippedWhenConversationHasToolHistory(t *testing.T) {
	handler, spy, _ := newGatewayForTest(t, gatewayFixture{
		models: `{"data":[
			{"id":"tool-chat","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]},"supported_parameters":["tools"]},
			{"id":"plain-chat","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]},"supported_parameters":["temperature"]}
		]}`,
		respond: func(model string, _ map[string]any) *http.Response {
			if model == "tool-chat" {
				return testResponse(http.StatusBadRequest, `{"error":{"message":"tools are not supported by this model"}}`)
			}
			return testResponse(http.StatusOK, `{"choices":[{"message":{"content":"would have been invalid"}}]}`)
		},
	})

	body := `{"model":"chat-tools","messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"sunny"}
	],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`
	recorder := postChat(handler, body)
	if recorder.Code == http.StatusOK {
		t.Fatalf("invalid replay was sent upstream and answered: body=%s", recorder.Body.String())
	}
	if spy.modelsCalled("plain-chat") != 0 {
		t.Fatalf("plain chat replay ran for a dialog with tool history: %#v", spy.recorded())
	}
	if got := recorder.Header().Get("X-Free-Router-Tools-Dropped"); got != "" {
		t.Fatalf("failed request claimed dropped tools: %q", got)
	}
}

// A model that already failed the strict pass must not be retried by the loose
// pass: it cannot succeed and spends the same free quota twice.
func TestLooseFallbackExcludesModelsThatAlreadyFailed(t *testing.T) {
	handler, spy, _ := newGatewayForTest(t, gatewayFixture{
		models: `{"data":[
			{"id":"tool-chat","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]},"supported_parameters":["tools"]},
			{"id":"plain-chat","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]},"supported_parameters":["temperature"]}
		]}`,
		respond: func(model string, _ map[string]any) *http.Response {
			if model == "tool-chat" {
				return testResponse(http.StatusBadGateway, `{"error":{"message":"upstream broken"}}`)
			}
			return testResponse(http.StatusOK, `{"choices":[{"message":{"content":"ok"}}]}`)
		},
	})

	recorder := postChat(handler, toolCallRequest)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := spy.modelsCalled("tool-chat"); got != 1 {
		t.Fatalf("tool-chat was attempted %d times, want 1: %#v", got, spy.recorded())
	}
	if got := spy.modelsCalled("plain-chat"); got != 1 {
		t.Fatalf("plain-chat was attempted %d times, want 1: %#v", got, spy.recorded())
	}
	if got := recorder.Header().Get("X-Free-Router-Tools-Dropped"); got != "true" {
		t.Fatalf("X-Free-Router-Tools-Dropped = %q, want true", got)
	}
}

// Frameworks that always serialise the field send an empty one for plain chat.
// Treating that as a tool request moved ordinary chat into the chat-tools pool.
func TestEmptyOrNullToolsDoNotSelectChatToolsRoute(t *testing.T) {
	handler, spy, tracker := newGatewayForTest(t, gatewayFixture{
		models: `{"data":[{"id":"tool-chat","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]},"supported_parameters":["tools"]}]}`,
	})

	for _, body := range []string{
		`{"model":"auto","messages":[{"role":"user","content":"hi"}],"tools":null}`,
		`{"model":"auto","messages":[{"role":"user","content":"hi"}],"tools":[]}`,
	} {
		recorder := postChat(handler, body)
		if recorder.Code != http.StatusOK {
			t.Fatalf("body=%s status=%d response=%s", body, recorder.Code, recorder.Body.String())
		}
	}

	if got := spy.modelsCalled("tool-chat"); got != 2 {
		t.Fatalf("tool-chat was attempted %d times, want 2: %#v", got, spy.recorded())
	}
	for _, state := range tracker.Snapshot() {
		if state.Model != "test/tool-chat" {
			continue
		}
		if state.Capability == catalog.FunctionChatTools && state.Requests > 0 {
			t.Fatalf("a request without tool definitions was recorded under chat-tools: %#v", state)
		}
		if state.Capability == catalog.FunctionChat && state.Requests == 0 {
			t.Fatalf("a request without tool definitions was not recorded under chat: %#v", state)
		}
	}
}

// A plain-text body makes OpenAI clients report a JSON decoding error instead of
// the real reason.
func TestUnauthorizedResponseUsesOpenAIErrorEnvelope(t *testing.T) {
	handler, _, _ := newGatewayForTest(t, gatewayFixture{
		models:   `{"data":[{"id":"tool-chat","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]},"supported_parameters":["tools"]}]}`,
		apiToken: "secret",
	})

	recorder := postChat(handler, `{"model":"chat","messages":[]}`)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("content-type = %q, want json", got)
	}
	if got := recorder.Header().Get("WWW-Authenticate"); got == "" {
		t.Fatal("WWW-Authenticate header is missing")
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("body is not a JSON error envelope: %v (%s)", err, recorder.Body.String())
	}
	if envelope.Error.Code != "unauthorized" || envelope.Error.Type != "free_router_error" || envelope.Error.Message == "" {
		t.Fatalf("unexpected error envelope: %#v", envelope.Error)
	}
}

// The mux used to answer a wrong method with a plain-text 405.
func TestWrongMethodUsesOpenAIErrorEnvelope(t *testing.T) {
	handler, _, _ := newGatewayForTest(t, gatewayFixture{
		models: `{"data":[{"id":"tool-chat","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]},"supported_parameters":["tools"]}]}`,
	})

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Allow"); got != http.MethodPost {
		t.Fatalf("Allow = %q, want POST", got)
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("body is not a JSON error envelope: %v (%s)", err, recorder.Body.String())
	}
	if envelope.Error.Code != "method_not_allowed" {
		t.Fatalf("unexpected error code: %#v", envelope)
	}
}

// A non-standard tool_calls payload is reported, never repaired: the caller gets
// exactly the bytes the upstream produced.
func TestNonStandardToolCallsAreReportedWithoutRewriting(t *testing.T) {
	upstreamBody := `{"choices":[{"message":{"tool_calls":[{"function":{"name":"get_weather","arguments":{"city":"SF"}}}]}}]}`
	handler, _, _ := newGatewayForTest(t, gatewayFixture{
		models: `{"data":[{"id":"tool-chat","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]},"supported_parameters":["tools"]}]}`,
		respond: func(string, map[string]any) *http.Response {
			return testResponse(http.StatusOK, upstreamBody)
		},
	})

	recorder := postChat(handler, toolCallRequest)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.String() != upstreamBody {
		t.Fatalf("response body was rewritten:\ngot  %s\nwant %s", recorder.Body.String(), upstreamBody)
	}
	if got := handler.metrics.ToolCallAnomalies.Load(); got != 1 {
		t.Fatalf("tool_call_anomalies = %d, want 1", got)
	}
	if got := handler.metrics.Snapshot().ToolCallAnomalies; got != 1 {
		t.Fatalf("metrics snapshot reports %d anomalies, want 1", got)
	}
}

func TestInspectToolCallsDetectsNonStandardShapes(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		stream   bool
		expected []string
	}{
		{
			name:     "arguments as object",
			body:     `{"choices":[{"message":{"tool_calls":[{"id":"call_1","function":{"name":"x","arguments":{"a":1}}}]}}]}`,
			expected: []string{ToolCallArgumentsNotString},
		},
		{
			name:     "missing id",
			body:     `{"choices":[{"message":{"tool_calls":[{"function":{"name":"x","arguments":"{}"}}]}}]}`,
			expected: []string{ToolCallMissingID},
		},
		{
			name:     "tool_calls is not an array",
			body:     `{"choices":[{"message":{"tool_calls":{"id":"call_1"}}}]}`,
			expected: []string{ToolCallNotArray},
		},
		{
			name:     "streaming delta without index",
			body:     "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":\"{}\"}}]}}]}\n\ndata: [DONE]\n\n",
			stream:   true,
			expected: []string{ToolCallMissingIndex},
		},
		{
			name:     "streaming delta with duplicate index",
			body:     "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{}\"}},{\"index\":0,\"function\":{\"arguments\":\"{}\"}}]}}]}\n\n",
			stream:   true,
			expected: []string{ToolCallDuplicateIndex},
		},
		{
			name:   "well formed",
			body:   `{"choices":[{"message":{"tool_calls":[{"id":"call_1","index":0,"function":{"name":"x","arguments":"{}"}}]}}]}`,
			stream: false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := inspectToolCalls([]byte(testCase.body), testCase.stream)
			if strings.Join(got, ",") != strings.Join(testCase.expected, ",") {
				t.Fatalf("issues = %v, want %v", got, testCase.expected)
			}
		})
	}
}

// The padded fallback tail is bounded by the attempt budget instead of growing
// to the whole pool.
func TestCandidateTailIsBoundedByAttemptBudget(t *testing.T) {
	clearBuiltinKeys(t)
	models := `{"data":[
		{"id":"m1","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]}},
		{"id":"m2","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]}},
		{"id":"m3","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]}},
		{"id":"m4","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]}}
	]}`
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return testResponse(http.StatusOK, models), nil
	})}
	registry, err := provider.NewRegistry(`[{"id":"test","base_url":"https://test.invalid","api_key":"test"}]`)
	if err != nil {
		t.Fatal(err)
	}
	store := catalog.New(registry, filepath.Join(t.TempDir(), "models.json"), client)
	discoverModelsForTest(t, store)
	routes, err := routing.New(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	config := routes.Config()
	chatRoute := config.Routes[catalog.FunctionChat]
	chatRoute.Models = []string{"test/m1"}
	config.Routes[catalog.FunctionChat] = chatRoute
	if err := routes.Update(config); err != nil {
		t.Fatal(err)
	}
	tracker := health.New()
	for _, model := range store.Models() {
		tracker.RestoreProbeSuccess(model.ID, catalog.FunctionChat, time.Now(), 1)
	}
	planner := NewCandidatePlanner(store, registry, routes, tracker, 2)

	candidates, _ := planner.CandidatesSeeded(catalog.FunctionChat, catalog.FunctionChat, false, 0)
	if len(candidates) != 3 {
		t.Fatalf("candidates = %d, want 1 configured + 2 padded: %#v", len(candidates), candidates)
	}
	if candidates[0].ID != "test/m1" {
		t.Fatalf("configured order was not preserved: %#v", candidates)
	}
}

// Skipping the loose replay must not stop the chat-tools pool from trying its
// next tool-capable model: a capability-shaped 4xx from one free source is not
// evidence that the whole pool cannot serve the request.
func TestToolHistoryStillFallsBackWithinChatToolsPool(t *testing.T) {
	handler, spy, _ := newGatewayForTest(t, gatewayFixture{
		models: `{"data":[
			{"id":"tool-a","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]},"supported_parameters":["tools"]},
			{"id":"tool-b","pricing":{"prompt":"0","completion":"0"},"architecture":{"output_modalities":["text"]},"supported_parameters":["tools"]}
		]}`,
		configure: func(config *routing.Config) {
			route := config.Routes[catalog.FunctionChatTools]
			route.Models = []string{"test/tool-a", "test/tool-b"}
			config.Routes[catalog.FunctionChatTools] = route
		},
		respond: func(model string, _ map[string]any) *http.Response {
			if model == "tool-a" {
				return testResponse(http.StatusBadRequest, `{"error":{"message":"tools are not supported by this model"}}`)
			}
			return testResponse(http.StatusOK, `{"choices":[{"message":{"tool_calls":[{"id":"call_1","index":0,"type":"function","function":{"name":"get_weather","arguments":"{}"}}]}}]}`)
		},
	})

	body := `{"model":"chat-tools","messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_0","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_0","content":"sunny"}
	],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`
	recorder := postChat(handler, body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if spy.modelsCalled("tool-a") != 1 || spy.modelsCalled("tool-b") != 1 {
		t.Fatalf("chat-tools pool was not walked in order: %#v", spy.recorded())
	}
	if !strings.Contains(recorder.Body.String(), "tool_calls") {
		t.Fatalf("response lost the tool call: %s", recorder.Body.String())
	}
}
