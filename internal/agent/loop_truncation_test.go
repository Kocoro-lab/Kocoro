package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestIsMaxTokensTruncation(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"max_tokens", true},
		{"length", true},
		{"end_turn_max_tokens", true},
		{"end_turn", false},
		{"tool_use", false},
		{"stop", false},
		{"", false},
		{"MAX_TOKENS", false},
	}
	for _, tc := range cases {
		if got := isMaxTokensTruncation(tc.in); got != tc.want {
			t.Errorf("isMaxTokensTruncation(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestArgsLookTruncated(t *testing.T) {
	twoReq := ToolInfo{Required: []string{"path", "content"}}
	noReq := ToolInfo{Required: nil}

	cases := []struct {
		name string
		args string
		info ToolInfo
		want bool
	}{
		{"empty string", "", twoReq, true},
		{"empty object", "{}", twoReq, true},
		{"null", "null", twoReq, true},
		{"malformed JSON", `{"path":`, twoReq, true},
		{"missing required field", `{"path":"/tmp/x"}`, twoReq, true},
		{"required field is empty string", `{"path":"/tmp/x","content":""}`, twoReq, false},
		{"all required present", `{"path":"/tmp/x","content":"hi"}`, twoReq, false},
		{"no requireds, empty object still flagged", "{}", noReq, true},
		{"no requireds, populated args ok", `{"foo":"bar"}`, noReq, false},
		{"non-string required value is acceptable", `{"path":"/tmp/x","content":42}`, twoReq, false},
	}
	for _, tc := range cases {
		if got := argsLookTruncated(tc.args, tc.info); got != tc.want {
			t.Errorf("%s: argsLookTruncated(%q) = %v, want %v", tc.name, tc.args, got, tc.want)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return string(b)
}

// Trailing tool_use with stop_reason=max_tokens AND truncated args (missing
// declared required field) must NOT be dispatched. The loop emits a synthetic
// tool_result paired with the truncated tool_use so the next request keeps
// tool_use/tool_result pairing (Anthropic 400 otherwise) and so the model
// sees a "you were cut off" message instead of a bare validation error.
func TestAgentLoop_MaxTokens_TruncatedTrailingCallSuppressed(t *testing.T) {
	callCount := 0
	var secondCallBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			json.NewEncoder(w).Encode(nativeResponse("", "max_tokens",
				toolCall("mock_tool", `{"path":"/tmp/x"}`), 10, 5))
			return
		}
		secondCallBody, _ = io.ReadAll(r.Body)
		json.NewEncoder(w).Encode(nativeResponse("pivoted to a smaller approach", "end_turn", nil, 20, 10))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	mt := &mockTool{name: "mock_tool", required: []string{"path", "content"}}
	reg.Register(mt)
	loop := NewAgentLoop(gw, reg, "medium", "", 25, 2000, 200, nil, nil, nil)

	result, _, err := loop.Run(context.Background(), "write a big file", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "pivoted to a smaller approach" {
		t.Errorf("unexpected result: %q", result)
	}
	if got := mt.runs.Load(); got != 0 {
		t.Errorf("mock_tool.Run was invoked %d times; expected 0 (truncated call must not dispatch)", got)
	}
	if callCount != 2 {
		t.Errorf("expected 2 LLM calls (truncated → pivot), got %d", callCount)
	}
	if !strings.Contains(string(secondCallBody), "output_truncated") {
		t.Errorf("second-call body missing synthetic tool_result — tool_use/tool_result pairing not preserved.\nbody: %s", string(secondCallBody))
	}
}

func TestAgentLoop_MaxTokens_TruncatedNativeCallPreservesToolUseID(t *testing.T) {
	callCount := 0
	var secondReq client.CompletionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			json.NewEncoder(w).Encode(nativeResponse("", "max_tokens",
				toolCallWithID("mock_tool", `{"path":"/tmp/x"}`, "toolu_truncated"), 10, 5))
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&secondReq); err != nil {
			t.Fatalf("decode second request: %v", err)
		}
		json.NewEncoder(w).Encode(nativeResponse("pivoted", "end_turn", nil, 20, 10))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	mt := &mockTool{name: "mock_tool", required: []string{"path", "content"}}
	reg.Register(mt)
	loop := NewAgentLoop(gw, reg, "medium", "", 25, 2000, 200, nil, nil, nil)

	result, _, err := loop.Run(context.Background(), "write a big file", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "pivoted" {
		t.Errorf("unexpected result: %q", result)
	}
	if got := mt.runs.Load(); got != 0 {
		t.Errorf("mock_tool.Run was invoked %d times; expected 0 (truncated call must not dispatch)", got)
	}

	var sawUse bool
	var sawResult bool
	for _, msg := range secondReq.Messages {
		for _, block := range msg.Content.Blocks() {
			switch block.Type {
			case "tool_use":
				if block.ID == "toolu_truncated" {
					sawUse = true
				}
			case "tool_result":
				if block.ToolUseID == "toolu_truncated" {
					sawResult = true
					if !block.IsError {
						t.Error("truncated native tool_result should be marked as an error")
					}
					if content, _ := block.ToolContent.(string); !strings.Contains(content, "output_truncated") {
						t.Errorf("truncated native tool_result missing output_truncated content: %#v", block.ToolContent)
					}
				}
			}
		}
	}
	if !sawUse {
		t.Fatal("second request missing original native tool_use block")
	}
	if !sawResult {
		t.Fatal("second request missing paired native tool_result block")
	}
}

func TestAgentLoop_MaxTokens_TextContinuationPromptIsInjected(t *testing.T) {
	callCount := 0
	var secondReq client.CompletionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		switch callCount {
		case 1:
			json.NewEncoder(w).Encode(nativeResponse("part one ", "max_tokens", nil, 10, 2000))
		case 2:
			if err := json.NewDecoder(r.Body).Decode(&secondReq); err != nil {
				t.Fatalf("decode second request: %v", err)
			}
			json.NewEncoder(w).Encode(nativeResponse("part two", "end_turn", nil, 20, 10))
		default:
			t.Fatalf("unexpected LLM call %d", callCount)
		}
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	loop := NewAgentLoop(gw, NewToolRegistry(), "medium", "", 25, 2000, 200, nil, nil, nil)

	result, _, err := loop.Run(context.Background(), "write a long answer", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "part one part two" {
		t.Fatalf("result = %q, want stitched continuation", result)
	}
	if callCount != 2 {
		t.Fatalf("LLM calls = %d, want 2", callCount)
	}
	if !strings.Contains(mustJSON(t, secondReq.Messages), "Your response was cut off. Continue from where you stopped.") {
		t.Fatalf("same-turn continuation prompt missing from second request")
	}

	runMsgs := loop.RunMessages()
	runInjected := loop.RunMessageInjected()
	if len(runMsgs) != len(runInjected) {
		t.Fatalf("RunMessages and RunMessageInjected length drift: %d vs %d", len(runMsgs), len(runInjected))
	}
	var foundInjected bool
	for i, msg := range runMsgs {
		if strings.Contains(msg.Content.Text(), "Your response was cut off. Continue from where you stopped.") {
			if !runInjected[i] {
				t.Fatalf("continuation prompt at run message %d was not marked injected", i)
			}
			foundInjected = true
		}
	}
	if !foundInjected {
		t.Fatalf("RunMessages missing continuation prompt; test no longer exercises persistence boundary")
	}
}

// When the model keeps emitting truncated trailing tool_use blocks past
// maxTruncationRecoveries (=3), the loop must stop instead of burning
// output budget at the same boundary indefinitely. The synthetic
// tool_result still fires on the exhausting attempt so the persisted
// transcript keeps the Anthropic tool_use/tool_result pairing intact.
func TestAgentLoop_MaxTokens_RecoveryCounterExhausts(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		// First 4 calls each return a truncated trailing tool_use. After the
		// 4th the recovery budget (3) is exhausted and the loop must trigger
		// runForceStopTurn, which omits tools and asks for a final text reply.
		// Call #5 (the force-stop turn) returns a clean end_turn so the loop
		// can produce a non-empty final answer.
		if callCount >= 5 {
			json.NewEncoder(w).Encode(nativeResponse(
				"output too large to emit in one response; please retry with chunked instructions",
				"end_turn", nil, 20, 10))
			return
		}
		json.NewEncoder(w).Encode(nativeResponse("", "max_tokens",
			toolCall("mock_tool", `{"path":"/tmp/x"}`), 10, 5))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	mt := &mockTool{name: "mock_tool", required: []string{"path", "content"}}
	reg.Register(mt)
	loop := NewAgentLoop(gw, reg, "medium", "", 25, 2000, 200, nil, nil, nil)

	result, _, err := loop.Run(context.Background(), "write a big file", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := mt.runs.Load(); got != 0 {
		t.Errorf("mock_tool.Run was invoked %d times; expected 0 (every truncated call must be suppressed)", got)
	}
	// Upper bound: 4 truncated + 1 force-stop final + small slack for any
	// unrelated synthesis call. Anything past ~7 means we did NOT actually
	// force-stop and the model is in an infinite loop — the very bug we are
	// guarding against.
	if callCount > 7 {
		t.Errorf("expected loop to force-stop after %d recoveries; got %d LLM calls (infinite loop?)",
			3, callCount)
	}
	if callCount < 5 {
		t.Errorf("expected at least 5 LLM calls (4 truncated + 1 force-stop final); got %d", callCount)
	}
	if result == "" {
		t.Error("expected non-empty final response after force-stop, got empty")
	}
}

// Same finish_reason but args are well-formed. Dispatch must proceed normally;
// the suppression rule must not over-fire on calls that happened to land at
// the cap with intact JSON. Guards against a regression that would silently
// drop legitimate trailing tool calls.
func TestAgentLoop_MaxTokens_WellFormedCallStillDispatches(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			json.NewEncoder(w).Encode(nativeResponse("", "max_tokens",
				toolCall("mock_tool", `{"path":"/tmp/x","content":"hi"}`), 10, 5))
			return
		}
		json.NewEncoder(w).Encode(nativeResponse("done", "end_turn", nil, 20, 10))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	mt := &mockTool{name: "mock_tool", required: []string{"path", "content"}}
	reg.Register(mt)
	loop := NewAgentLoop(gw, reg, "medium", "", 25, 2000, 200, nil, nil, nil)

	result, _, err := loop.Run(context.Background(), "do the thing", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "done" {
		t.Errorf("unexpected result: %q", result)
	}
	if got := mt.runs.Load(); got != 1 {
		t.Errorf("mock_tool.Run was invoked %d times; expected 1 (well-formed args must dispatch)", got)
	}
}
