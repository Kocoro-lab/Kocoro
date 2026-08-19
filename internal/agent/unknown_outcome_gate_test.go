package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// scriptedLoopClient returns the queued responses in order, then narrates and
// ends the turn. Reset rewinds the script for a second Run on the same loop.
type scriptedLoopClient struct {
	mu        sync.Mutex
	calls     int
	responses []client.CompletionResponse
}

func (c *scriptedLoopClient) Complete(context.Context, client.CompletionRequest) (*client.CompletionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls < len(c.responses) {
		resp := c.responses[c.calls]
		c.calls++
		return &resp, nil
	}
	c.calls++
	return &client.CompletionResponse{OutputText: "done", FinishReason: "end_turn"}, nil
}

func (c *scriptedLoopClient) CompleteStream(ctx context.Context, req client.CompletionRequest, _ func(client.StreamDelta)) (*client.CompletionResponse, error) {
	return c.Complete(ctx, req)
}

func (c *scriptedLoopClient) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = 0
}

func journalToolCall(id, value string) client.FunctionCall {
	return client.FunctionCall{
		ID:        id,
		Name:      "journal_write",
		Arguments: json.RawMessage(`{"value":"` + value + `"}`),
	}
}

func newGateLoop(t *testing.T, llm client.LLMClient) (*AgentLoop, *journalWriteTool, *recordingSideEffectJournal) {
	t.Helper()
	journal := &recordingSideEffectJournal{}
	result := TransientError("lost after dispatch")
	result.SideEffectOutcomeUnknown = true
	tool := &journalWriteTool{journal: journal, result: result}
	registry := NewToolRegistry()
	registry.Register(tool)
	loop := NewAgentLoop(llm, registry, "medium", t.TempDir(), 8, 2000, 200, nil, nil, nil)
	loop.SetSideEffectExecutionJournal(journal)
	loop.SetHandler(&collectingHandler{})
	loop.SetCheckpointFunc(func(context.Context) error { return nil })
	return loop, tool, journal
}

func findToolResult(t *testing.T, loop *AgentLoop, toolUseID string) (string, bool) {
	t.Helper()
	for _, message := range loop.RunMessages() {
		for _, block := range message.Content.Blocks() {
			if block.Type == "tool_result" && block.ToolUseID == toolUseID {
				return client.ToolResultText(block), block.IsError
			}
		}
	}
	t.Fatalf("no tool_result paired for %s", toolUseID)
	return "", false
}

// TestUnknownOutcomeGate_BlocksIdenticalCallSameTurn pins the mechanical
// latch: after a material call ends outcome-unknown, a byte-identical repeat
// in the SAME user turn is rejected locally (the tool never runs again for
// those args — zero network), while a different-argument call passes through.
func TestUnknownOutcomeGate_BlocksIdenticalCallSameTurn(t *testing.T) {
	llm := &scriptedLoopClient{responses: []client.CompletionResponse{
		{FinishReason: "tool_use", ToolCalls: []client.FunctionCall{journalToolCall("c1", "same")}},
		{FinishReason: "tool_use", ToolCalls: []client.FunctionCall{
			journalToolCall("c2", "same"),      // byte-identical automatic retry
			journalToolCall("c3", "different"), // different args must pass
		}},
	}}
	loop, tool, _ := newGateLoop(t, llm)

	text, _, err := loop.Run(context.Background(), "mutate external state", nil, nil)
	if err != nil {
		t.Fatalf("Run = (%q, %v), want clean completion", text, err)
	}
	// c1 dispatched, c2 latched (no Tool.Run), c3 dispatched.
	if tool.runs.Load() != 2 {
		t.Fatalf("tool runs = %d, want 2 (identical retry must not reach the tool)", tool.runs.Load())
	}
	blockedText, blockedIsError := findToolResult(t, loop, "c2")
	if !blockedIsError ||
		!strings.Contains(blockedText, "blocked locally and NOT re-sent") ||
		!strings.Contains(blockedText, "outcome is still unknown") {
		t.Fatalf("latched call result = (%v, %s)", blockedIsError, blockedText)
	}
	// c3 executed (its own result is a fresh outcome-unknown), so it must NOT
	// carry the interception-specific wording.
	passText, _ := findToolResult(t, loop, "c3")
	if strings.Contains(passText, "byte-identical to a call earlier") {
		t.Fatalf("different-argument call was latched: %s", passText)
	}
	if !strings.Contains(passText, "may have changed external state") {
		t.Fatalf("different-argument call did not reach the tool: %s", passText)
	}
	assertNativeToolPairs(t, loop.RunMessages())
}

// TestUnknownOutcomeGate_NextUserMessageClears pins the latch lifecycle: a
// new run begins with a new user message, so the same exact call may be
// deliberately retried again.
func TestUnknownOutcomeGate_NextUserMessageClears(t *testing.T) {
	llm := &scriptedLoopClient{responses: []client.CompletionResponse{
		{FinishReason: "tool_use", ToolCalls: []client.FunctionCall{journalToolCall("r1", "same")}},
	}}
	loop, tool, _ := newGateLoop(t, llm)

	if _, _, err := loop.Run(context.Background(), "mutate external state", nil, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if tool.runs.Load() != 1 {
		t.Fatalf("tool runs after first run = %d, want 1", tool.runs.Load())
	}

	llm.Reset()
	llm.responses = []client.CompletionResponse{
		{FinishReason: "tool_use", ToolCalls: []client.FunctionCall{journalToolCall("r2", "same")}},
	}
	if _, _, err := loop.Run(context.Background(), "try again please", nil, nil); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if tool.runs.Load() != 2 {
		t.Fatalf("tool runs after second run = %d, want 2 (new user message clears the latch)", tool.runs.Load())
	}
}

// TestUnknownOutcomeGate_UnitSemantics pins the latch primitives, including
// nil-map safety (checking before anything was ever armed) so a cancellation
// racing ahead of the first arm cannot panic the loop goroutine.
func TestUnknownOutcomeGate_UnitSemantics(t *testing.T) {
	loop := &AgentLoop{}
	if loop.unknownOutcomeGateBlocks("x_create_post", `{"text":"hi"}`) {
		t.Fatal("empty latch must not block")
	}
	loop.armUnknownOutcomeGate("x_create_post", `{"text":"hi"}`)
	if !loop.unknownOutcomeGateBlocks("x_create_post", `{"text":"hi"}`) {
		t.Fatal("armed exact call must block")
	}
	if loop.unknownOutcomeGateBlocks("x_create_post", `{"text":"hello"}`) {
		t.Fatal("different arguments must not block")
	}
	if loop.unknownOutcomeGateBlocks("x_delete_post", `{"text":"hi"}`) {
		t.Fatal("different tool must not block")
	}
	loop.clearUnknownOutcomeGate()
	if loop.unknownOutcomeGateBlocks("x_create_post", `{"text":"hi"}`) {
		t.Fatal("cleared latch must not block")
	}
}

// TestAgentLoop_JournalUnavailableContinuesAsOrdinaryError pins the sibling
// branch: a call that definitively never executed (its durable execution
// record could not be prepared) returns to the model as an ordinary error and
// the run continues — this failure was always safe to retry.
func TestAgentLoop_JournalUnavailableContinuesAsOrdinaryError(t *testing.T) {
	llm := &scriptedLoopClient{responses: []client.CompletionResponse{
		{FinishReason: "tool_use", ToolCalls: []client.FunctionCall{journalToolCall("j1", "value")}},
	}}
	loop, tool, journal := newGateLoop(t, llm)
	journal.prepareErr = ErrSideEffectJournalUnavailable

	text, _, err := loop.Run(context.Background(), "mutate external state", nil, nil)
	if err != nil || text != "done" {
		t.Fatalf("Run = (%q, %v), want narrated completion", text, err)
	}
	if tool.runs.Load() != 0 {
		t.Fatalf("tool runs = %d, want 0 (prepare failed before dispatch)", tool.runs.Load())
	}
	resultText, isError := findToolResult(t, loop, "j1")
	if !isError || !strings.Contains(resultText, "durable side-effect journal was unavailable") {
		t.Fatalf("journal-unavailable result = (%v, %s)", isError, resultText)
	}
	if status := loop.LastRunStatus(); status.FailureCode != "" {
		t.Fatalf("run status = %+v, want no failure code", status)
	}
	assertNativeToolPairs(t, loop.RunMessages())
}
