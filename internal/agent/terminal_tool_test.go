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

type terminalErrorCardTool struct{}

func (terminalErrorCardTool) Info() ToolInfo {
	return ToolInfo{Name: "terminal_card", Parameters: map[string]any{"type": "object"}}
}
func (terminalErrorCardTool) RequiresApproval() bool { return false }
func (terminalErrorCardTool) StopsAgentLoop() bool   { return true }
func (terminalErrorCardTool) Run(context.Context, string) (ToolResult, error) {
	result := BusinessError("stream disconnected; do not continue this task")
	result.StopAgentLoop = true
	result.TerminalUserMessage = "I couldn't show the installation card. Please try again."
	return result, nil
}

// A terminal tool's Content is written at the model, so promoting it to the
// run's answer put model-directed instructions — and raw "[business error] ..."
// strings — in the user's final chat bubble and in the replayed transcript.
// TerminalUserMessage is what the user must get instead; Content must still
// reach the model as the tool_result so a resumed run keeps its context.
func TestAgentLoop_TerminalToolUserMessageReplacesModelFacingContent(t *testing.T) {
	sideEffect := &terminalSideEffectTool{}
	reg := NewToolRegistry()
	reg.Register(terminalErrorCardTool{})
	reg.Register(sideEffect)
	loop := NewAgentLoop(&terminalTestClient{}, reg, "medium", t.TempDir(), 4, 2000, 200, nil, nil, nil)
	result, _, err := loop.Run(context.Background(), "need a card", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result != "I couldn't show the installation card. Please try again." {
		t.Fatalf("run answer was not the user-facing text: %q", result)
	}
	if strings.Contains(result, "[business error]") {
		t.Fatalf("business-error prefix reached the user: %q", result)
	}
	for _, msg := range loop.RunMessages() {
		if msg.Role != "assistant" {
			continue
		}
		for _, block := range msg.Content.Blocks() {
			if block.Type != "text" {
				continue
			}
			if strings.Contains(block.Text, "[business error]") || strings.Contains(block.Text, "do not continue this task") {
				t.Fatalf("model-directed text persisted as an assistant message: %q", block.Text)
			}
		}
	}
	transcript, err := json.Marshal(loop.RunMessages())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(transcript), "do not continue this task") {
		t.Fatalf("model-facing tool_result lost its content: %s", transcript)
	}
	assertNativeToolPairs(t, loop.RunMessages())
}

type definitiveResultTool struct{ runs atomic.Int32 }

func (t *definitiveResultTool) Info() ToolInfo {
	return ToolInfo{Name: "read_named_resource", Parameters: map[string]any{"type": "object"}}
}
func (*definitiveResultTool) RequiresApproval() bool { return false }
func (t *definitiveResultTool) Run(context.Context, string) (ToolResult, error) {
	t.runs.Add(1)
	result := BusinessError("the requested file does not exist")
	result.StopFurtherTools = true
	return result, nil
}

type definitiveResultClient struct {
	calls          atomic.Int32
	loop           *AgentLoop
	synthesisPhase TurnPhase
}

func (c *definitiveResultClient) Complete(_ context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
	call := c.calls.Add(1)
	switch call {
	case 1:
		if len(req.Tools) == 0 {
			panic("initial request omitted tools")
		}
		return &client.CompletionResponse{FinishReason: "tool_use", ContentBlocks: []client.ContentBlock{
			{Type: "tool_use", ID: "missing-resource", Name: "read_named_resource", Input: json.RawMessage(`{}`)},
		}}, nil
	case 2:
		if len(req.Tools) != 0 {
			panic("terminal synthesis still exposed tools")
		}
		c.synthesisPhase, _, _ = c.loop.tracker.Current()
		return &client.CompletionResponse{OutputText: "The requested file does not exist.", FinishReason: "end_turn"}, nil
	default:
		panic("definitive result allowed an extra completion")
	}
}

func (c *definitiveResultClient) CompleteStream(ctx context.Context, req client.CompletionRequest, _ func(client.StreamDelta)) (*client.CompletionResponse, error) {
	return c.Complete(ctx, req)
}

func TestAgentLoop_DefinitiveResultGetsOneToolDisabledSynthesis(t *testing.T) {
	llm := &definitiveResultClient{}
	tool := &definitiveResultTool{}
	reg := NewToolRegistry()
	reg.Register(tool)
	loop := NewAgentLoop(llm, reg, "medium", t.TempDir(), 8, 2000, 200, nil, nil, nil)
	llm.loop = loop

	result, _, err := loop.Run(context.Background(), "read the named file", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result != "The requested file does not exist." {
		t.Fatalf("result=%q", result)
	}
	if llm.calls.Load() != 2 || tool.runs.Load() != 1 {
		t.Fatalf("completion calls=%d tool runs=%d, want 2 and 1", llm.calls.Load(), tool.runs.Load())
	}
	if llm.synthesisPhase != PhaseAwaitingLLM {
		t.Fatalf("definitive synthesis phase=%s, want awaiting_llm", llm.synthesisPhase)
	}
	status := loop.LastRunStatus()
	if status.Partial || status.FailureCode != "" {
		t.Fatalf("run status=%+v, want clean terminal synthesis", status)
	}
	for _, message := range loop.RunMessages() {
		if message.Role == "assistant" && strings.Contains(message.Content.Text(), "[business error]") {
			t.Fatalf("raw business error leaked into assistant answer: %q", message.Content.Text())
		}
	}
	assertNativeToolPairs(t, loop.RunMessages())
}

type terminalSilentCardTool struct{}

func (terminalSilentCardTool) Info() ToolInfo {
	return ToolInfo{Name: "terminal_card", Parameters: map[string]any{"type": "object"}}
}
func (terminalSilentCardTool) RequiresApproval() bool { return false }
func (terminalSilentCardTool) StopsAgentLoop() bool   { return true }
func (terminalSilentCardTool) Run(context.Context, string) (ToolResult, error) {
	return ToolResult{Content: "card delivered; stop here", StopAgentLoop: true, TerminalUserSuppressed: true}, nil
}

type recordingTextHandler struct {
	streamingToolHandler
	texts []string
}

func (h *recordingTextHandler) OnText(text string) { h.texts = append(h.texts, text) }

// When the client already rendered the boundary itself (a localized installation
// card), the run must end with no assistant bubble at all rather than an English
// sentence no client can localize. The model-facing Content still has to survive
// as the tool_result so a resumed run keeps its context.
func TestAgentLoop_TerminalToolSuppressedUserMessageEmitsNoAssistantText(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(terminalSilentCardTool{})
	reg.Register(&terminalSideEffectTool{})
	loop := NewAgentLoop(&terminalTestClient{}, reg, "medium", t.TempDir(), 4, 2000, 200, nil, nil, nil)
	handler := &recordingTextHandler{}
	loop.SetHandler(handler)
	result, _, err := loop.Run(context.Background(), "need a card", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result != "" {
		t.Fatalf("suppressed terminal result still produced an answer: %q", result)
	}
	if len(handler.texts) != 0 {
		t.Fatalf("suppressed terminal result streamed text to the client: %v", handler.texts)
	}
	messages := loop.RunMessages()
	for _, msg := range messages {
		if msg.Role != "assistant" {
			continue
		}
		for _, block := range msg.Content.Blocks() {
			if block.Type == "text" && strings.Contains(block.Text, "stop here") {
				t.Fatalf("suppressed text persisted as an assistant message: %q", block.Text)
			}
		}
	}
	transcript, err := json.Marshal(messages)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(transcript), "card delivered; stop here") {
		t.Fatalf("model-facing tool_result lost its content: %s", transcript)
	}
	assertNativeToolPairs(t, messages)
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
