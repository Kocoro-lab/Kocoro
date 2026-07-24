package agent

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

type streamingProbeTool struct {
	name     string
	readOnly bool
	started  chan struct{}
	release  chan struct{}
	runs     atomic.Int32
}

func (t *streamingProbeTool) Info() ToolInfo {
	return ToolInfo{
		Name:       t.name,
		Parameters: map[string]any{"path": map[string]any{"type": "string"}},
		Required:   []string{"path"},
	}
}
func (t *streamingProbeTool) RequiresApproval() bool { return false }
func (t *streamingProbeTool) IsReadOnlyCall(string) bool {
	return t.readOnly
}
func (t *streamingProbeTool) Run(ctx context.Context, _ string) (ToolResult, error) {
	if t.runs.Add(1) == 1 {
		close(t.started)
	}
	select {
	case <-t.release:
		return ToolResult{Content: "probe result"}, nil
	case <-ctx.Done():
		return ToolResult{}, ctx.Err()
	}
}

type streamingSequenceClient struct {
	mu           sync.Mutex
	call         client.FunctionCall
	beforeReturn func()
	streamCalls  int
}

func (c *streamingSequenceClient) Complete(context.Context, client.CompletionRequest) (*client.CompletionResponse, error) {
	return &client.CompletionResponse{OutputText: "done"}, nil
}

func (c *streamingSequenceClient) CompleteStream(_ context.Context, _ client.CompletionRequest, onDelta func(client.StreamDelta)) (*client.CompletionResponse, error) {
	c.mu.Lock()
	c.streamCalls++
	n := c.streamCalls
	c.mu.Unlock()
	if n == 1 {
		call := c.call
		onDelta(client.StreamDelta{ToolCall: &call})
		if c.beforeReturn != nil {
			c.beforeReturn()
		}
		return &client.CompletionResponse{
			FinishReason: "tool_use",
			ToolCalls:    []client.FunctionCall{call},
		}, nil
	}
	return &client.CompletionResponse{OutputText: "done"}, nil
}

type streamingToolHandler struct {
	starts atomic.Int32
}

func (h *streamingToolHandler) OnToolCall(string, string, string) { h.starts.Add(1) }
func (h *streamingToolHandler) OnToolResult(string, string, string, ToolResult, time.Duration) {
}
func (h *streamingToolHandler) OnText(string)                        {}
func (h *streamingToolHandler) OnPreamble(string)                    {}
func (h *streamingToolHandler) OnStreamDelta(string)                 {}
func (h *streamingToolHandler) OnApprovalNeeded(string, string) bool { return false }
func (h *streamingToolHandler) OnUsage(TurnUsage)                    {}
func (h *streamingToolHandler) OnCloudAgent(string, string, string)  {}
func (h *streamingToolHandler) OnCloudProgress(int, int)             {}
func (h *streamingToolHandler) OnCloudPlan(string, string, bool)     {}

func newStreamingToolLoop(t *testing.T, tool Tool, llm client.LLMClient, handler EventHandler) *AgentLoop {
	t.Helper()
	reg := NewToolRegistry()
	reg.Register(tool)
	loop := NewAgentLoop(llm, reg, "medium", t.TempDir(), 4, 0, 0, nil, nil, nil)
	loop.SetEnableStreaming(true)
	loop.SetHandler(handler)
	return loop
}

func TestStreamingToolStartup_ReadOnlyCallStartsBeforeFinalResponse(t *testing.T) {
	tool := &streamingProbeTool{
		name:     "probe_read",
		readOnly: true,
		started:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	call := client.FunctionCall{
		ID:        "tool-1",
		Name:      tool.name,
		Arguments: json.RawMessage(`{"path":"README.md"}`),
	}
	handler := &streamingToolHandler{}
	llm := &streamingSequenceClient{call: call}
	llm.beforeReturn = func() {
		select {
		case <-tool.started:
			if handler.starts.Load() != 0 {
				t.Fatal("speculative tool card was visible before the final response committed the call")
			}
			close(tool.release)
		case <-time.After(time.Second):
			t.Fatal("read-only tool did not start before the stream returned its final response")
		}
	}
	loop := newStreamingToolLoop(t, tool, llm, handler)

	result, _, err := loop.Run(context.Background(), "inspect it", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "done" {
		t.Fatalf("result = %q, want done", result)
	}
	if got := tool.runs.Load(); got != 1 {
		t.Fatalf("tool runs = %d, want 1", got)
	}
	if got := handler.starts.Load(); got != 1 {
		t.Fatalf("OnToolCall starts = %d, want 1 (no post-stream duplicate)", got)
	}
}

func TestStreamingToolStartup_WriteCapableCallWaitsForFinalResponse(t *testing.T) {
	tool := &streamingProbeTool{
		name:     "probe_write",
		readOnly: false,
		started:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	call := client.FunctionCall{
		ID:        "tool-2",
		Name:      tool.name,
		Arguments: json.RawMessage(`{"path":"output.txt"}`),
	}
	var startedEarly atomic.Bool
	llm := &streamingSequenceClient{call: call}
	llm.beforeReturn = func() {
		select {
		case <-tool.started:
			startedEarly.Store(true)
		case <-time.After(50 * time.Millisecond):
		}
	}
	go func() {
		<-tool.started
		close(tool.release)
	}()
	handler := &streamingToolHandler{}
	loop := newStreamingToolLoop(t, tool, llm, handler)

	if _, _, err := loop.Run(context.Background(), "write it", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if startedEarly.Load() {
		t.Fatal("write-capable tool started before the final response committed the call")
	}
	if got := tool.runs.Load(); got != 1 {
		t.Fatalf("tool runs = %d, want 1", got)
	}
}
