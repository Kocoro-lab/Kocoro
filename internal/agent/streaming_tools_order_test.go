package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// sharedStateWriteTool mutates shared state; sharedStateReadTool observes it.
// Together they prove response-order semantics: a read that appears AFTER a
// write in the same assistant response must observe the write's effect.
type sharedState struct{ v atomic.Int32 }

type sharedStateWriteTool struct{ st *sharedState }

func (t *sharedStateWriteTool) Info() ToolInfo {
	return ToolInfo{Name: "order_write", Parameters: map[string]any{"path": map[string]any{"type": "string"}}, Required: []string{"path"}}
}
func (t *sharedStateWriteTool) RequiresApproval() bool     { return false }
func (t *sharedStateWriteTool) IsReadOnlyCall(string) bool { return false }
func (t *sharedStateWriteTool) Run(context.Context, string) (ToolResult, error) {
	t.st.v.Store(1)
	return ToolResult{Content: "wrote"}, nil
}

type sharedStateReadTool struct {
	st       *sharedState
	observed atomic.Int32 // 100+v marks "ran", so 100 = pre-write, 101 = post-write
}

func (t *sharedStateReadTool) Info() ToolInfo {
	return ToolInfo{Name: "order_read", Parameters: map[string]any{"path": map[string]any{"type": "string"}}, Required: []string{"path"}}
}
func (t *sharedStateReadTool) RequiresApproval() bool     { return false }
func (t *sharedStateReadTool) IsReadOnlyCall(string) bool { return true }
func (t *sharedStateReadTool) Run(context.Context, string) (ToolResult, error) {
	v := t.st.v.Load()
	t.observed.Store(v + 100)
	return ToolResult{Content: fmt.Sprintf("state=%d", v)}, nil
}

// orderStreamClient streams a write call then a read call, then returns a
// final response committing both in that order.
type orderStreamClient struct {
	writeCall, readCall client.FunctionCall
	streamCalls         atomic.Int32
}

func (c *orderStreamClient) Complete(context.Context, client.CompletionRequest) (*client.CompletionResponse, error) {
	return &client.CompletionResponse{OutputText: "done"}, nil
}

func (c *orderStreamClient) CompleteStream(_ context.Context, _ client.CompletionRequest, onDelta func(client.StreamDelta)) (*client.CompletionResponse, error) {
	if c.streamCalls.Add(1) == 1 {
		w, r := c.writeCall, c.readCall
		onDelta(client.StreamDelta{ToolCall: &w})
		onDelta(client.StreamDelta{ToolCall: &r})
		// Give any (incorrect) speculative read a moment to run pre-write.
		time.Sleep(50 * time.Millisecond)
		return &client.CompletionResponse{
			FinishReason: "tool_use",
			ToolCalls:    []client.FunctionCall{w, r},
		}, nil
	}
	return &client.CompletionResponse{OutputText: "done"}, nil
}

type resultRecordingHandler struct {
	streamingToolHandler
	mu      sync.Mutex
	results []string
}

func (h *resultRecordingHandler) OnToolResult(name, _ string, _ string, res ToolResult, _ time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.results = append(h.results, name+":"+res.Content)
}

func (h *resultRecordingHandler) recorded() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.results...)
}

// A read-only call that streams AFTER a write-capable call in the same
// response must not start speculatively: it would observe pre-write state.
func TestStreamingToolStartup_ReadAfterWriteObservesWriteEffect(t *testing.T) {
	st := &sharedState{}
	wt := &sharedStateWriteTool{st: st}
	rt := &sharedStateReadTool{st: st}

	llm := &orderStreamClient{
		writeCall: client.FunctionCall{ID: "w1", Name: "order_write", Arguments: json.RawMessage(`{"path":"a"}`)},
		readCall:  client.FunctionCall{ID: "r1", Name: "order_read", Arguments: json.RawMessage(`{"path":"a"}`)},
	}
	handler := &resultRecordingHandler{}

	reg := NewToolRegistry()
	reg.Register(wt)
	reg.Register(rt)
	loop := NewAgentLoop(llm, reg, "medium", t.TempDir(), 4, 0, 0, nil, nil, nil)
	loop.SetEnableStreaming(true)
	loop.SetHandler(handler)

	if _, _, err := loop.Run(context.Background(), "write then verify", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := rt.observed.Load(); got != 101 {
		t.Fatalf("read observed state=%d, want 1 — the read ran before the same-response write", got-100)
	}
}

// A claimed speculative result must still emit OnToolResult so UI cards
// complete and downstream consumers see a terminal event.
func TestStreamingToolStartup_ClaimedResultEmitsOnToolResult(t *testing.T) {
	tool := &streamingProbeTool{
		name:     "probe_read",
		readOnly: true,
		started:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	close(tool.release) // run immediately
	call := client.FunctionCall{
		ID:        "tool-1",
		Name:      tool.name,
		Arguments: json.RawMessage(`{"path":"README.md"}`),
	}
	handler := &resultRecordingHandler{}
	llm := &streamingSequenceClient{call: call}
	llm.beforeReturn = func() { <-tool.started }

	reg := NewToolRegistry()
	reg.Register(tool)
	loop := NewAgentLoop(llm, reg, "medium", t.TempDir(), 4, 0, 0, nil, nil, nil)
	loop.SetEnableStreaming(true)
	loop.SetHandler(handler)

	if _, _, err := loop.Run(context.Background(), "inspect it", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sawProbeResult bool
	for _, r := range handler.recorded() {
		if strings.HasPrefix(r, "probe_read:") {
			sawProbeResult = true
		}
	}
	if !sawProbeResult {
		t.Fatal("OnToolResult was never emitted for the claimed speculative call — UI cards would stay running forever")
	}
	if got := tool.runs.Load(); got != 1 {
		t.Fatalf("tool runs = %d, want 1", got)
	}
}
