package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

type toolSearchE2ETool struct {
	name      string
	desc      string
	params    map[string]any
	source    ToolSource
	exposure  ToolExposure
	namespace string
	result    string
	runs      int
	lastArgs  string
}

func (t *toolSearchE2ETool) Info() ToolInfo {
	return ToolInfo{
		Name:        t.name,
		Description: t.desc,
		Parameters:  t.params,
	}
}

func (t *toolSearchE2ETool) Run(_ context.Context, args string) (ToolResult, error) {
	t.runs++
	t.lastArgs = args
	return ToolResult{Content: t.result}, nil
}

func (t *toolSearchE2ETool) RequiresApproval() bool { return false }
func (t *toolSearchE2ETool) ToolSource() ToolSource { return t.source }
func (t *toolSearchE2ETool) ToolExposure() ToolExposure {
	return t.exposure
}
func (t *toolSearchE2ETool) ToolSearchNamespace() string { return t.namespace }

func TestToolSearchE2E_LegacyFallbackLoadsBM25MatchAndContinues(t *testing.T) {
	direct := &toolSearchE2ETool{
		name:     "web_search",
		desc:     "Search the web.",
		source:   SourceGateway,
		exposure: ToolExposureDirect,
		params:   map[string]any{"type": "object"},
	}
	deferred := &toolSearchE2ETool{
		name:      "calendar_create_event",
		desc:      "Create an event.",
		source:    SourceIntegration,
		namespace: "google_calendar",
		params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"event_title": map[string]any{
					"type":        "string",
					"description": "Title for the calendar appointment.",
				},
				"description": DescriptionFieldSpec,
			},
		},
		result: "event-created",
	}
	reg := NewToolRegistry()
	reg.Register(direct)
	reg.Register(deferred)

	var requests []client.CompletionRequest
	var requestsMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req client.CompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode completion request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		requestsMu.Lock()
		requests = append(requests, req)
		requestNumber := len(requests)
		requestsMu.Unlock()
		switch requestNumber {
		case 1:
			_ = json.NewEncoder(w).Encode(nativeResponse("", "tool_use",
				toolCallWithID("tool_search", `{"query":"google calendar appointment title"}`, "toolu_search"), 10, 5))
		case 2:
			_ = json.NewEncoder(w).Encode(nativeResponse("", "tool_use",
				toolCallWithID("calendar_create_event", `{"event_title":"Planning","description":"Create the planning calendar event."}`, "toolu_calendar"), 10, 5))
		case 3:
			_ = json.NewEncoder(w).Encode(nativeResponse("Created.", "end_turn", nil, 10, 5))
		default:
			t.Errorf("unexpected extra completion request %d", requestNumber)
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	ws := NewWorkingSet()
	handler := &preambleHandler{}
	loop := NewAgentLoop(client.NewGatewayClient(server.URL, ""), reg, "medium", "", 8, 2000, 200, nil, nil, nil)
	loop.SetWorkingSet(ws)
	loop.SetHandler(handler)
	result, _, err := loop.Run(context.Background(), "Create a planning appointment in Google Calendar.", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "Created." {
		t.Fatalf("result = %q, want Created.", result)
	}
	requestsMu.Lock()
	requests = append([]client.CompletionRequest(nil), requests...)
	requestsMu.Unlock()
	if len(requests) != 3 {
		t.Fatalf("completion request count = %d, want 3", len(requests))
	}
	assertToolExposureInRequest(t, requests[0], "web_search", true, false)
	assertToolExposureInRequest(t, requests[0], "tool_search", true, false)
	assertToolExposureInRequest(t, requests[0], "calendar_create_event", false, false)
	assertToolExposureInRequest(t, requests[1], "calendar_create_event", true, false)
	if !requestHasToolResultText(requests[1], "LOADED:calendar_create_event") ||
		!requestHasToolResultText(requests[1], `"event_title"`) {
		wire, _ := json.Marshal(requests[1].Messages)
		t.Fatalf("legacy continuation request is missing the loaded full schema: %s", wire)
	}
	if deferred.runs != 1 {
		t.Fatalf("calendar_create_event runs = %d, want 1", deferred.runs)
	}
	if !ws.Contains("calendar_create_event") {
		t.Fatal("loaded Deferred schema was not warmed in the session WorkingSet")
	}
	if len(handler.preambleCalls) != 0 {
		t.Fatalf("external tool description must not become a fallback preamble, got %#v", handler.preambleCalls)
	}
}

func TestToolSearchE2E_ExplicitSonnetUsesLegacyContinuation(t *testing.T) {
	direct := &toolSearchE2ETool{
		name:     "web_search",
		desc:     "Search the web.",
		source:   SourceGateway,
		exposure: ToolExposureDirect,
		params:   map[string]any{"type": "object"},
	}
	deferred := &toolSearchE2ETool{
		name:      "browser_navigate",
		desc:      "Navigate a browser page.",
		source:    SourceMCP,
		namespace: "playwright",
		params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url":         map[string]any{"type": "string", "description": "Destination URL."},
				"description": DescriptionFieldSpec,
			},
		},
		result: "navigated",
	}
	reg := NewToolRegistry()
	reg.Register(direct)
	reg.Register(deferred)

	var requests []client.CompletionRequest
	var requestsMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req client.CompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode completion request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		requestsMu.Lock()
		requests = append(requests, req)
		requestNumber := len(requests)
		requestsMu.Unlock()
		switch requestNumber {
		case 1:
			_ = json.NewEncoder(w).Encode(nativeResponse("", "tool_use",
				toolCallWithID("tool_search", `{"query":"playwright navigate destination"}`, "toolu_search"), 10, 5))
		case 2:
			_ = json.NewEncoder(w).Encode(nativeResponse("", "tool_use",
				toolCallWithID("browser_navigate", `{"url":"https://example.com","description":"Open example.com in the browser."}`, "toolu_browser"), 10, 5))
		case 3:
			_ = json.NewEncoder(w).Encode(nativeResponse("Opened.", "end_turn", nil, 10, 5))
		default:
			t.Errorf("unexpected extra completion request %d", requestNumber)
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	handler := &preambleHandler{}
	loop := NewAgentLoop(client.NewGatewayClient(server.URL, ""), reg, "medium", "", 8, 2000, 200, nil, nil, nil)
	loop.SetSpecificModel("claude-sonnet-4-6")
	loop.SetHandler(handler)
	result, _, err := loop.Run(context.Background(), "Open example.com in the browser.", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "Opened." {
		t.Fatalf("result = %q, want Opened.", result)
	}
	requestsMu.Lock()
	requests = append([]client.CompletionRequest(nil), requests...)
	requestsMu.Unlock()
	if len(requests) != 3 {
		t.Fatalf("completion request count = %d, want 3", len(requests))
	}
	assertToolExposureInRequest(t, requests[0], "web_search", true, false)
	assertToolExposureInRequest(t, requests[0], "tool_search", true, false)
	assertToolExposureInRequest(t, requests[0], "browser_navigate", false, false)
	assertToolExposureInRequest(t, requests[1], "browser_navigate", true, false)
	if !requestHasToolResultText(requests[1], "LOADED:browser_navigate") ||
		!requestHasToolResultText(requests[1], `"url"`) {
		wire, _ := json.Marshal(requests[1].Messages)
		t.Fatalf("legacy continuation request is missing browser_navigate schema: %s", wire)
	}
	if requestHasToolReference(requests[1], "browser_navigate") {
		t.Fatal("explicit Sonnet continuation unexpectedly used tool_reference")
	}
	if deferred.runs != 1 {
		t.Fatalf("browser_navigate runs = %d, want 1", deferred.runs)
	}
	if len(handler.preambleCalls) != 0 {
		t.Fatalf("external tool description must not become a fallback preamble, got %#v", handler.preambleCalls)
	}
}

func TestToolSearchE2E_ColdDeferredDirectCallIsRejected(t *testing.T) {
	deferred := &toolSearchE2ETool{
		name:     "calendar_create_event",
		desc:     "Create a calendar event.",
		source:   SourceIntegration,
		exposure: ToolExposureDeferred,
		params:   map[string]any{"type": "object"},
		result:   "event-created",
	}
	reg := NewToolRegistry()
	reg.Register(deferred)

	llm := &budgetCaptureLLMClient{responses: []*client.CompletionResponse{
		{
			FinishReason: "tool_use",
			ToolCalls: []client.FunctionCall{{
				ID: "toolu_cold", Name: deferred.name, Arguments: json.RawMessage(`{}`),
			}},
		},
		{OutputText: "recovered", FinishReason: "end_turn"},
	}}
	loop := NewAgentLoop(llm, reg, "medium", "", 4, 2000, 200, nil, nil, nil)

	result, _, err := loop.Run(context.Background(), "Create an event.", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "recovered" {
		t.Fatalf("result = %q, want recovered", result)
	}
	if deferred.runs != 0 {
		t.Fatalf("cold Deferred tool ran %d times, want 0", deferred.runs)
	}
	if len(llm.requests) != 2 {
		t.Fatalf("completion requests = %d, want 2", len(llm.requests))
	}
	assertToolExposureInRequest(t, llm.requests[0], deferred.name, false, false)
	if !requestHasToolResultText(llm.requests[1], "deferred tool is not loaded") {
		t.Fatal("continuation is missing the fail-closed Deferred rejection")
	}
}

func TestToolSearchE2E_SameResponseSearchDoesNotAuthorizeColdCall(t *testing.T) {
	deferred := &toolSearchE2ETool{
		name:     "calendar_create_event",
		desc:     "Create a calendar event.",
		source:   SourceIntegration,
		exposure: ToolExposureDeferred,
		params:   map[string]any{"type": "object"},
		result:   "event-created",
	}
	reg := NewToolRegistry()
	reg.Register(deferred)

	llm := &budgetCaptureLLMClient{responses: []*client.CompletionResponse{
		{
			FinishReason: "tool_use",
			ToolCalls: []client.FunctionCall{
				{ID: "toolu_search", Name: "tool_search", Arguments: json.RawMessage(`{"query":"select:calendar_create_event"}`)},
				{ID: "toolu_cold", Name: deferred.name, Arguments: json.RawMessage(`{}`)},
			},
		},
		{OutputText: "continuing", FinishReason: "end_turn"},
		{OutputText: "recovered", FinishReason: "end_turn"},
	}}
	loop := NewAgentLoop(llm, reg, "medium", "", 8, 2000, 200, nil, nil, nil)

	result, _, err := loop.Run(context.Background(), "Create an event.", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "recovered" {
		t.Fatalf("result = %q, want recovered", result)
	}
	if deferred.runs != 0 {
		t.Fatalf("same-response cold Deferred tool ran %d times, want 0", deferred.runs)
	}
	if len(llm.requests) != 3 {
		t.Fatalf("completion requests = %d, want 3", len(llm.requests))
	}
	assertToolExposureInRequest(t, llm.requests[1], deferred.name, true, false)
	if !requestHasToolResultText(llm.requests[1], "LOADED:calendar_create_event") {
		t.Fatal("tool_search result was not recorded")
	}
	if !requestHasToolResultText(llm.requests[1], "deferred tool is not loaded") {
		t.Fatal("same-response cold call was not rejected")
	}
}

func TestToolSearchE2E_DirectOpenerDoesNotLoadDeferredSchema(t *testing.T) {
	direct := &toolSearchE2ETool{
		name:     "web_search",
		desc:     "Search the web.",
		source:   SourceGateway,
		exposure: ToolExposureDirect,
		params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string"},
			},
		},
		result: "search-result",
	}
	deferred := &toolSearchE2ETool{
		name:      "calendar_create_event",
		desc:      "Create an event.",
		source:    SourceIntegration,
		namespace: "google_calendar",
		params:    map[string]any{"type": "object"},
		result:    "event-created",
	}
	question := &toolSearchE2ETool{
		name:     "ask_user_question",
		desc:     "Ask the user a structured clarifying question.",
		source:   SourceLocal,
		exposure: ToolExposureDirect,
		params:   map[string]any{"type": "object"},
		result:   `{"answer":"Tokyo"}`,
	}
	reg := NewToolRegistry()
	reg.Register(direct)
	reg.Register(deferred)
	reg.Register(question)

	var requests []client.CompletionRequest
	var requestsMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req client.CompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode completion request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		requestsMu.Lock()
		requests = append(requests, req)
		requestNumber := len(requests)
		requestsMu.Unlock()
		switch requestNumber {
		case 1:
			_ = json.NewEncoder(w).Encode(nativeResponse("", "tool_use",
				toolCall("web_search", `{"query":"current weather"}`), 10, 5))
		case 2:
			_ = json.NewEncoder(w).Encode(nativeResponse("Sunny.", "end_turn", nil, 10, 5))
		default:
			t.Errorf("unexpected extra completion request %d", requestNumber)
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	ws := NewWorkingSet()
	loop := NewAgentLoop(client.NewGatewayClient(server.URL, ""), reg, "medium", "", 8, 2000, 200, nil, nil, nil)
	loop.SetWorkingSet(ws)
	result, _, err := loop.Run(context.Background(), "Search the web for the current weather.", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "Sunny." {
		t.Fatalf("result = %q, want Sunny.", result)
	}
	requestsMu.Lock()
	requests = append([]client.CompletionRequest(nil), requests...)
	requestsMu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("completion request count = %d, want 2", len(requests))
	}
	assertToolExposureInRequest(t, requests[0], "web_search", true, false)
	assertToolExposureInRequest(t, requests[0], "tool_search", true, false)
	assertToolExposureInRequest(t, requests[0], "ask_user_question", true, false)
	assertToolExposureInRequest(t, requests[0], "calendar_create_event", false, false)
	if direct.runs != 1 {
		t.Fatalf("web_search runs = %d, want 1", direct.runs)
	}
	if deferred.runs != 0 {
		t.Fatalf("calendar_create_event runs = %d, want 0", deferred.runs)
	}
	if question.runs != 0 {
		t.Fatalf("ask_user_question runs = %d, want 0", question.runs)
	}
	if ws.Len() != 0 {
		t.Fatalf("WorkingSet length = %d, want 0", ws.Len())
	}
	for _, request := range requests {
		if requestHasToolResultText(request, "LOADED:") {
			t.Fatal("Direct opener unexpectedly loaded a Deferred schema")
		}
	}
}

func TestToolSearchE2E_CommonCalendarReadStaysDirect(t *testing.T) {
	read := &toolSearchE2ETool{
		name:     "calendar_list_events",
		desc:     "List calendar events.",
		source:   SourceLocal,
		exposure: ToolExposureDirect,
		params:   map[string]any{"type": "object"},
		result:   "No events.",
	}
	mutation := &toolSearchE2ETool{
		name:      "calendar_create_event",
		desc:      "Create a calendar event.",
		source:    SourceIntegration,
		namespace: "google_calendar",
		params:    map[string]any{"type": "object"},
		result:    "event-created",
	}
	reg := NewToolRegistry()
	reg.Register(read)
	reg.Register(mutation)

	var requests []client.CompletionRequest
	var requestsMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req client.CompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode completion request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		requestsMu.Lock()
		requests = append(requests, req)
		requestNumber := len(requests)
		requestsMu.Unlock()
		switch requestNumber {
		case 1:
			_ = json.NewEncoder(w).Encode(nativeResponse("", "tool_use",
				toolCall("calendar_list_events", `{}`), 10, 5))
		case 2:
			_ = json.NewEncoder(w).Encode(nativeResponse("No events.", "end_turn", nil, 10, 5))
		default:
			t.Errorf("unexpected extra completion request %d", requestNumber)
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	ws := NewWorkingSet()
	loop := NewAgentLoop(client.NewGatewayClient(server.URL, ""), reg, "medium", "", 8, 2000, 200, nil, nil, nil)
	loop.SetWorkingSet(ws)
	result, _, err := loop.Run(context.Background(), "What is on my calendar today?", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "No events." {
		t.Fatalf("result = %q, want No events.", result)
	}
	requestsMu.Lock()
	requests = append([]client.CompletionRequest(nil), requests...)
	requestsMu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("completion request count = %d, want 2", len(requests))
	}
	assertToolExposureInRequest(t, requests[0], "calendar_list_events", true, false)
	assertToolExposureInRequest(t, requests[0], "tool_search", true, false)
	assertToolExposureInRequest(t, requests[0], "calendar_create_event", false, false)
	if read.runs != 1 {
		t.Fatalf("calendar_list_events runs = %d, want 1", read.runs)
	}
	if mutation.runs != 0 {
		t.Fatalf("calendar_create_event runs = %d, want 0", mutation.runs)
	}
	if ws.Len() != 0 {
		t.Fatalf("WorkingSet length = %d, want 0", ws.Len())
	}
}

func TestToolSearchE2E_ClarificationUsesDirectQuestionWithoutSearch(t *testing.T) {
	question := &toolSearchE2ETool{
		name:     "ask_user_question",
		desc:     "Ask the user a structured clarifying question.",
		source:   SourceLocal,
		exposure: ToolExposureDirect,
		params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"question": map[string]any{"type": "string"},
			},
		},
		result: `{"answer":"Tokyo"}`,
	}
	deferred := &toolSearchE2ETool{
		name:      "calendar_create_event",
		desc:      "Create a calendar event.",
		source:    SourceIntegration,
		namespace: "google_calendar",
		params:    map[string]any{"type": "object"},
		result:    "event-created",
	}
	reg := NewToolRegistry()
	reg.Register(question)
	reg.Register(deferred)

	var requests []client.CompletionRequest
	var requestsMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req client.CompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode completion request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		requestsMu.Lock()
		requests = append(requests, req)
		requestNumber := len(requests)
		requestsMu.Unlock()
		switch requestNumber {
		case 1:
			_ = json.NewEncoder(w).Encode(nativeResponse("", "tool_use",
				toolCall("ask_user_question", `{"question":"Which city?"}`), 10, 5))
		case 2:
			_ = json.NewEncoder(w).Encode(nativeResponse("Tokyo selected.", "end_turn", nil, 10, 5))
		default:
			t.Errorf("unexpected extra completion request %d", requestNumber)
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	ws := NewWorkingSet()
	loop := NewAgentLoop(client.NewGatewayClient(server.URL, ""), reg, "medium", "", 8, 2000, 200, nil, nil, nil)
	loop.SetWorkingSet(ws)
	result, _, err := loop.Run(context.Background(), "Check the weather, but I have not said which city.", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "Tokyo selected." {
		t.Fatalf("result = %q, want Tokyo selected.", result)
	}
	requestsMu.Lock()
	requests = append([]client.CompletionRequest(nil), requests...)
	requestsMu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("completion request count = %d, want 2", len(requests))
	}
	assertToolExposureInRequest(t, requests[0], "ask_user_question", true, false)
	assertToolExposureInRequest(t, requests[0], "tool_search", true, false)
	assertToolExposureInRequest(t, requests[0], "calendar_create_event", false, false)
	if question.runs != 1 {
		t.Fatalf("ask_user_question runs = %d, want 1", question.runs)
	}
	if deferred.runs != 0 {
		t.Fatalf("calendar_create_event runs = %d, want 0", deferred.runs)
	}
	if ws.Len() != 0 {
		t.Fatalf("WorkingSet length = %d, want 0", ws.Len())
	}
}

func assertToolExposureInRequest(
	t *testing.T,
	request client.CompletionRequest,
	name string,
	wantPresent bool,
	wantDeferred bool,
) {
	t.Helper()
	for _, tool := range request.Tools {
		if schemaToolName(tool) != name {
			continue
		}
		if !wantPresent {
			t.Fatalf("tool %q unexpectedly present in first-turn schemas", name)
		}
		if tool.DeferLoading != wantDeferred {
			t.Fatalf("tool %q defer_loading = %v, want %v", name, tool.DeferLoading, wantDeferred)
		}
		return
	}
	if wantPresent {
		t.Fatalf("tool %q missing from request schemas", name)
	}
}

func requestHasToolResultText(request client.CompletionRequest, substring string) bool {
	for _, message := range request.Messages {
		for _, block := range message.Content.Blocks() {
			if block.Type != "tool_result" {
				continue
			}
			if text, ok := block.ToolContent.(string); ok && strings.Contains(text, substring) {
				return true
			}
		}
	}
	return false
}

func requestHasToolReference(request client.CompletionRequest, name string) bool {
	for _, message := range request.Messages {
		for _, block := range message.Content.Blocks() {
			if block.Type != "tool_result" {
				continue
			}
			nested, ok := block.ToolContent.([]client.ContentBlock)
			if !ok {
				continue
			}
			for _, child := range nested {
				if child.Type == "tool_reference" && child.ToolName == name {
					return true
				}
			}
		}
	}
	return false
}
