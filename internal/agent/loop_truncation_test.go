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
