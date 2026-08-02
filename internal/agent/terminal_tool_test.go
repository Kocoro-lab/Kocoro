package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

type terminalTestTool struct{}

func (terminalTestTool) Info() ToolInfo {
	return ToolInfo{Name: "terminal_card", Parameters: map[string]any{"type": "object"}}
}

func assertNativeToolPairs(t *testing.T, messages []client.Message) {
	t.Helper()
	uses := map[string]int{}
	results := map[string]int{}
	for _, msg := range messages {
		for _, block := range msg.Content.Blocks() {
			switch block.Type {
			case "tool_use":
				uses[block.ID]++
			case "tool_result":
				results[block.ToolUseID]++
			}
		}
	}
	for id, count := range uses {
		if count != 1 || results[id] != 1 {
			t.Fatalf("tool pair id=%q uses=%d results=%d", id, count, results[id])
		}
	}
	for id, count := range results {
		if count != 1 || uses[id] != 1 {
			t.Fatalf("orphan result id=%q uses=%d results=%d", id, uses[id], count)
		}
	}
}
func (terminalTestTool) RequiresApproval() bool { return false }
func (terminalTestTool) StopsAgentLoop() bool   { return true }
func (terminalTestTool) Run(context.Context, string) (ToolResult, error) {
	return ToolResult{Content: "wait for user", StopAgentLoop: true}, nil
}

type terminalSideEffectTool struct{ runs atomic.Int32 }

func (t *terminalSideEffectTool) Info() ToolInfo {
	return ToolInfo{Name: "must_not_run", Parameters: map[string]any{"type": "object"}}
}
func (*terminalSideEffectTool) RequiresApproval() bool { return false }
func (t *terminalSideEffectTool) Run(context.Context, string) (ToolResult, error) {
	t.runs.Add(1)
	return ToolResult{Content: "ran"}, nil
}

type terminalTestClient struct{ calls atomic.Int32 }

func (c *terminalTestClient) Complete(context.Context, client.CompletionRequest) (*client.CompletionResponse, error) {
	c.calls.Add(1)
	return &client.CompletionResponse{FinishReason: "tool_use", ToolCalls: []client.FunctionCall{
		{ID: "terminal", Name: "terminal_card", Arguments: json.RawMessage(`{}`)},
		{ID: "side-effect", Name: "must_not_run", Arguments: json.RawMessage(`{}`)},
	}}, nil
}
func (c *terminalTestClient) CompleteStream(ctx context.Context, req client.CompletionRequest, _ func(client.StreamDelta)) (*client.CompletionResponse, error) {
	return c.Complete(ctx, req)
}

func TestAgentLoop_TerminalToolStopsBatchAndFurtherLLMCalls(t *testing.T) {
	llm := &terminalTestClient{}
	sideEffect := &terminalSideEffectTool{}
	reg := NewToolRegistry()
	reg.Register(terminalTestTool{})
	reg.Register(sideEffect)
	loop := NewAgentLoop(llm, reg, "medium", t.TempDir(), 4, 2000, 200, nil, nil, nil)
	result, _, err := loop.Run(context.Background(), "need a card", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result != "wait for user" {
		t.Fatalf("result=%q", result)
	}
	if llm.calls.Load() != 1 {
		t.Fatalf("LLM calls=%d", llm.calls.Load())
	}
	if sideEffect.runs.Load() != 0 {
		t.Fatalf("side-effect runs=%d", sideEffect.runs.Load())
	}
	var terminalUses, sideEffectUses, terminalResults int
	for _, msg := range loop.RunMessages() {
		for _, block := range msg.Content.Blocks() {
			switch block.Type + ":" + block.Name {
			case "tool_use:terminal_card":
				terminalUses++
			case "tool_use:must_not_run":
				sideEffectUses++
			case "tool_result:":
				if block.ToolUseID == "terminal" {
					terminalResults++
				}
			}
		}
	}
	if terminalUses != 1 || sideEffectUses != 0 || terminalResults != 1 {
		t.Fatalf("tool transcript pairing terminal_use=%d side_effect_use=%d terminal_result=%d", terminalUses, sideEffectUses, terminalResults)
	}
	assertNativeToolPairs(t, loop.RunMessages())
}

type terminalNativeClient struct{}

func (terminalNativeClient) Complete(context.Context, client.CompletionRequest) (*client.CompletionResponse, error) {
	return &client.CompletionResponse{FinishReason: "tool_use", ContentBlocks: []client.ContentBlock{
		{Type: "thinking", Thinking: "test-thought", Signature: "test-signature"},
		{Type: "text", Text: "Preparing the capability."},
		{Type: "tool_use", ID: "terminal-native", Name: "terminal_card", Input: json.RawMessage(`{}`)},
		{Type: "redacted_thinking", Data: "opaque-test-data"},
		{Type: "tool_use", ID: "omitted-native", Name: "must_not_run", Input: json.RawMessage(`{}`)},
	}}, nil
}
func (c terminalNativeClient) CompleteStream(ctx context.Context, req client.CompletionRequest, _ func(client.StreamDelta)) (*client.CompletionResponse, error) {
	return c.Complete(ctx, req)
}

type terminalContinuationValidator struct{ checked atomic.Bool }

func (c *terminalContinuationValidator) Complete(_ context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
	asserted := false
	uses := map[string]int{}
	results := map[string]int{}
	for _, msg := range req.Messages {
		for _, block := range msg.Content.Blocks() {
			if block.Type == "tool_use" {
				uses[block.ID]++
				if block.Name == "must_not_run" {
					panic("omitted tool_use reached continuation request")
				}
			}
			if block.Type == "tool_result" {
				results[block.ToolUseID]++
			}
		}
	}
	for id, count := range uses {
		if count != 1 || results[id] != 1 {
			panic("invalid persisted tool transcript")
		}
		asserted = true
	}
	if !asserted {
		panic("continuation request had no persisted tool pair")
	}
	c.checked.Store(true)
	return &client.CompletionResponse{OutputText: "continued", FinishReason: "end_turn"}, nil
}
func (c *terminalContinuationValidator) CompleteStream(ctx context.Context, req client.CompletionRequest, _ func(client.StreamDelta)) (*client.CompletionResponse, error) {
	return c.Complete(ctx, req)
}

func TestAgentLoop_TerminalToolNativeContentBlocksPersistValidContinuation(t *testing.T) {
	sideEffect := &terminalSideEffectTool{}
	reg := NewToolRegistry()
	reg.Register(terminalTestTool{})
	reg.Register(sideEffect)
	loop := NewAgentLoop(terminalNativeClient{}, reg, "medium", t.TempDir(), 4, 2000, 200, nil, nil, nil)
	if _, _, err := loop.Run(context.Background(), "need a card", nil, nil); err != nil {
		t.Fatal(err)
	}
	messages := loop.RunMessages()
	assertNativeToolPairs(t, messages)
	var ordered []string
	for _, msg := range messages {
		for _, block := range msg.Content.Blocks() {
			if block.Type == "tool_use" && block.Name == "must_not_run" {
				t.Fatal("omitted native tool_use persisted")
			}
			if block.Type == "thinking" || block.Type == "text" || block.Type == "tool_use" || block.Type == "redacted_thinking" {
				ordered = append(ordered, block.Type)
			}
		}
	}
	want := []string{"thinking", "text", "tool_use", "redacted_thinking", "tool_result", "text"}
	if len(ordered) < 4 || ordered[0] != want[0] || ordered[1] != want[1] || ordered[2] != want[2] || ordered[3] != want[3] {
		t.Fatalf("thinking/text order changed: %v", ordered)
	}
	validator := &terminalContinuationValidator{}
	continued := NewAgentLoop(validator, NewToolRegistry(), "medium", t.TempDir(), 2, 2000, 200, nil, nil, nil)
	result, _, err := continued.Run(context.Background(), "continue", nil, messages)
	if err != nil || result != "continued" || !validator.checked.Load() {
		t.Fatalf("continuation result=%q checked=%v err=%v", result, validator.checked.Load(), err)
	}
}

type terminalLegacyClient struct{}

func (terminalLegacyClient) Complete(context.Context, client.CompletionRequest) (*client.CompletionResponse, error) {
	return &client.CompletionResponse{FinishReason: "tool_use", ToolCalls: []client.FunctionCall{
		{Name: "terminal_card", Arguments: json.RawMessage(`{}`)},
		{Name: "must_not_run", Arguments: json.RawMessage(`{}`)},
	}}, nil
}
func (c terminalLegacyClient) CompleteStream(ctx context.Context, req client.CompletionRequest, _ func(client.StreamDelta)) (*client.CompletionResponse, error) {
	return c.Complete(ctx, req)
}

func TestAgentLoop_TerminalToolLegacyToolCallsFilterTranscript(t *testing.T) {
	sideEffect := &terminalSideEffectTool{}
	reg := NewToolRegistry()
	reg.Register(terminalTestTool{})
	reg.Register(sideEffect)
	loop := NewAgentLoop(terminalLegacyClient{}, reg, "medium", t.TempDir(), 4, 2000, 200, nil, nil, nil)
	if _, _, err := loop.Run(context.Background(), "need a card", nil, nil); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(loop.RunMessages())
	if sideEffect.runs.Load() != 0 || string(data) == "" || strings.Contains(string(data), "must_not_run") {
		t.Fatalf("legacy omitted call leaked or executed: runs=%d transcript=%s", sideEffect.runs.Load(), data)
	}
	if !strings.Contains(string(data), "terminal_card") || !strings.Contains(string(data), "wait for user") {
		t.Fatalf("legacy terminal pair missing: %s", data)
	}
}

type terminalSpeculativeRead struct {
	started   chan struct{}
	cancelled chan struct{}
}

func (*terminalSpeculativeRead) Info() ToolInfo {
	return ToolInfo{Name: "speculative_read", Parameters: map[string]any{"type": "object"}}
}
func (*terminalSpeculativeRead) RequiresApproval() bool     { return false }
func (*terminalSpeculativeRead) IsReadOnlyCall(string) bool { return true }
func (*terminalSpeculativeRead) CancelableMidTurn() bool    { return true }
func (t *terminalSpeculativeRead) Run(ctx context.Context, _ string) (ToolResult, error) {
	close(t.started)
	<-ctx.Done()
	close(t.cancelled)
	return ToolResult{Content: "cancelled", IsError: true}, ctx.Err()
}

type terminalStreamingClient struct{ started <-chan struct{} }

func (c terminalStreamingClient) Complete(context.Context, client.CompletionRequest) (*client.CompletionResponse, error) {
	return nil, nil
}
func (c terminalStreamingClient) CompleteStream(_ context.Context, _ client.CompletionRequest, emit func(client.StreamDelta)) (*client.CompletionResponse, error) {
	read := client.FunctionCall{ID: "spec-read", Name: "speculative_read", Arguments: json.RawMessage(`{}`)}
	emit(client.StreamDelta{ToolCall: &read})
	select {
	case <-c.started:
	case <-time.After(time.Second):
		panic("speculative read did not start")
	}
	return &client.CompletionResponse{FinishReason: "tool_use", ToolCalls: []client.FunctionCall{
		{ID: "terminal-stream", Name: "terminal_card", Arguments: json.RawMessage(`{}`)},
		read,
	}}, nil
}

func TestAgentLoop_TerminalToolCancelsFilteredStreamingSpeculation(t *testing.T) {
	read := &terminalSpeculativeRead{started: make(chan struct{}), cancelled: make(chan struct{})}
	reg := NewToolRegistry()
	reg.Register(terminalTestTool{})
	reg.Register(read)
	loop := NewAgentLoop(terminalStreamingClient{started: read.started}, reg, "medium", t.TempDir(), 4, 2000, 200, nil, nil, nil)
	loop.SetEnableStreaming(true)
	loop.SetHandler(&streamingToolHandler{})
	if _, _, err := loop.Run(context.Background(), "need a card", nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-read.cancelled:
	case <-time.After(time.Second):
		t.Fatal("terminal admission did not cancel filtered speculative read")
	}
}
