package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

type runTraceRecorder struct {
	mockHandler
	events []RunTraceEvent
}

func (h *runTraceRecorder) OnRunTrace(event RunTraceEvent) {
	h.events = append(h.events, event)
}

func toolRunTraceEvents(events []RunTraceEvent) []RunTraceEvent {
	var tools []RunTraceEvent
	for _, event := range events {
		if event.Type == RunTraceEventToolOutcome {
			tools = append(tools, event)
		}
	}
	return tools
}

type runTraceTool struct {
	name     string
	delay    time.Duration
	result   ToolResult
	readOnly bool
	required string
}

func (t *runTraceTool) Info() ToolInfo {
	info := ToolInfo{
		Name:        t.name,
		Description: "run trace test tool",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"value": map[string]any{"type": "string"}},
		},
	}
	if t.required != "" {
		info.Required = []string{t.required}
	}
	return info
}

func (t *runTraceTool) Run(ctx context.Context, _ string) (ToolResult, error) {
	select {
	case <-time.After(t.delay):
		return t.result, nil
	case <-ctx.Done():
		return ToolResult{}, ctx.Err()
	}
}

func (t *runTraceTool) RequiresApproval() bool { return false }
func (t *runTraceTool) IsReadOnlyCall(string) bool {
	return t.readOnly
}

func runTraceServer(t *testing.T, calls []client.FunctionCall) *httptest.Server {
	t.Helper()
	request := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		request++
		if request == 1 {
			resp := nativeResponse("", "tool_use", nil, 10, 5)
			resp.ToolCalls = calls
			if err := json.NewEncoder(w).Encode(resp); err != nil {
				t.Errorf("encode tool response: %v", err)
			}
			return
		}
		if err := json.NewEncoder(w).Encode(nativeResponse("done", "end_turn", nil, 10, 5)); err != nil {
			t.Errorf("encode terminal response: %v", err)
		}
	}))
}

func TestRunTrace_ParallelBatchPreservesProviderOrderAndCanonicalHashes(t *testing.T) {
	server := runTraceServer(t, []client.FunctionCall{
		*toolCallWithID("slow_read", `{ "value": "private-alpha" }`, "call-slow"),
		*toolCallWithID("fast_read", `{"value":"private-beta"}`, "call-fast"),
	})
	defer server.Close()

	reg := NewToolRegistry()
	reg.Register(&runTraceTool{name: "slow_read", delay: 25 * time.Millisecond, result: ToolResult{Content: "slow secret result"}, readOnly: true})
	reg.Register(&runTraceTool{name: "fast_read", delay: time.Millisecond, result: ToolResult{Content: "fast secret result"}, readOnly: true})
	handler := &runTraceRecorder{}
	loop := NewAgentLoop(client.NewGatewayClient(server.URL, ""), reg, "medium", "", 4, 2000, 200, nil, nil, nil)
	loop.SetHandler(handler)

	if _, _, err := loop.Run(context.Background(), "trace parallel reads", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(handler.events) != 5 {
		t.Fatalf("trace events = %d, want 5 (two model responses, two tools, terminal)", len(handler.events))
	}
	for i, event := range handler.events {
		if event.Seq != int64(i+1) {
			t.Fatalf("event[%d] sequence = %+v", i, event)
		}
	}
	toolEvents := toolRunTraceEvents(handler.events)
	if len(toolEvents) != 2 {
		t.Fatalf("tool trace events = %d, want 2", len(toolEvents))
	}
	for i, event := range toolEvents {
		if event.Iteration != 1 || event.Type != RunTraceEventToolOutcome {
			t.Fatalf("event[%d] envelope = %+v", i, event)
		}
		if event.Tool == nil || event.Tool.ModelBatchIndex != i || event.Tool.ModelBatchSize != 2 {
			t.Fatalf("event[%d] model batch = %+v", i, event.Tool)
		}
		if event.Tool.ExecutionBatchIndex == nil || *event.Tool.ExecutionBatchIndex != 0 ||
			event.Tool.ExecutionBatchSize != 2 || !event.Tool.ExecutionParallel || event.Tool.MaxConcurrency != 2 {
			t.Fatalf("event[%d] execution batch = %+v", i, event.Tool)
		}
		if !event.Tool.Executed || event.Tool.Outcome != "succeeded" || len(event.Tool.ArgumentsHMACSHA256) != 64 || len(event.Tool.ResultHMACSHA256) != 64 {
			t.Fatalf("event[%d] outcome = %+v", i, event.Tool)
		}
	}
	if toolEvents[0].Tool.Name != "slow_read" || toolEvents[1].Tool.Name != "fast_read" {
		t.Fatalf("provider order lost: %+v", toolEvents)
	}

	wire, err := json.Marshal(handler.events)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-alpha", "private-beta", "slow secret result", "fast secret result"} {
		if strings.Contains(string(wire), secret) {
			t.Fatalf("trace leaked raw payload %q: %s", secret, wire)
		}
	}
	if strings.Contains(string(wire), "tool_call_id") || strings.Contains(string(wire), "args_sha256") || strings.Contains(string(wire), "result_sha256") {
		t.Fatalf("trace retained stable identifiers or unkeyed digest fields: %s", wire)
	}
	if toolEvents[0].Tool.ArgumentsHMACSHA256 == toolArgumentsDigest(json.RawMessage(`{"value":"private-alpha"}`)) {
		t.Fatal("release trace reused the stable legacy execution-evidence digest")
	}
	terminal := handler.events[len(handler.events)-1]
	if terminal.Type != RunTraceEventTerminal || terminal.Terminal == nil ||
		terminal.Terminal.Partial || terminal.Terminal.FailureCode != "" || terminal.Terminal.IterationCount != 2 {
		t.Fatalf("terminal event = %+v", terminal)
	}
}

func TestRunTrace_ResolvedAndExecutedCallsShareProviderOrder(t *testing.T) {
	server := runTraceServer(t, []client.FunctionCall{
		*toolCallWithID("write_one", `{"value":"ok"}`, "call-write"),
		*toolCallWithID("needs_value", `{}`, "call-invalid"),
		*toolCallWithID("write_one", `{"value":"ok"}`, "call-duplicate"),
		*toolCallWithID("missing_tool", `{"value":"x"}`, "call-missing"),
	})
	defer server.Close()

	reg := NewToolRegistry()
	reg.Register(&runTraceTool{name: "write_one", result: ToolResult{Content: "written"}})
	reg.Register(&runTraceTool{name: "needs_value", required: "value", result: ToolResult{Content: "unexpected"}})
	handler := &runTraceRecorder{}
	loop := NewAgentLoop(client.NewGatewayClient(server.URL, ""), reg, "medium", "", 4, 2000, 200, nil, nil, nil)
	loop.SetHandler(handler)

	if _, _, err := loop.Run(context.Background(), "trace resolved calls", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(handler.events) != 7 {
		t.Fatalf("trace events = %d, want 7 (two model responses, four tools, terminal)", len(handler.events))
	}
	toolEvents := toolRunTraceEvents(handler.events)
	if len(toolEvents) != 4 {
		t.Fatalf("tool trace events = %d, want 4", len(toolEvents))
	}
	wantNames := []string{"write_one", "needs_value", "write_one", "missing_tool"}
	wantOutcomes := []string{"succeeded", "rejected", "duplicate", "rejected"}
	wantExecuted := []bool{true, false, false, false}
	for i, event := range toolEvents {
		if event.Tool == nil || event.Tool.Name != wantNames[i] || event.Tool.Outcome != wantOutcomes[i] || event.Tool.Executed != wantExecuted[i] {
			t.Fatalf("event[%d] = %+v", i, event.Tool)
		}
		if event.Tool.Ordinal != i+1 || event.Tool.ModelBatchIndex != i || event.Tool.ModelBatchSize != 4 {
			t.Fatalf("event[%d] order metadata = %+v", i, event.Tool)
		}
		if event.Tool.Executed != (event.Tool.ExecutionBatchIndex != nil) {
			t.Fatalf("event[%d] execution batch presence = %+v", i, event.Tool)
		}
	}
	if toolEvents[0].Tool.ArgumentsHMACSHA256 != toolEvents[2].Tool.ArgumentsHMACSHA256 {
		t.Fatal("canonical equal arguments did not correlate within one run")
	}
}

type mixedRunTraceStreamingClient struct {
	calls []client.FunctionCall
	read  *streamingProbeTool
	runs  int
}

func (c *mixedRunTraceStreamingClient) Complete(context.Context, client.CompletionRequest) (*client.CompletionResponse, error) {
	return &client.CompletionResponse{OutputText: "done"}, nil
}

func (c *mixedRunTraceStreamingClient) CompleteStream(_ context.Context, _ client.CompletionRequest, emit func(client.StreamDelta)) (*client.CompletionResponse, error) {
	c.runs++
	if c.runs > 1 {
		return &client.CompletionResponse{OutputText: "done"}, nil
	}
	for index := range c.calls {
		call := c.calls[index]
		emit(client.StreamDelta{ToolCall: &call})
	}
	select {
	case <-c.read.started:
		close(c.read.release)
	case <-time.After(time.Second):
		return nil, context.DeadlineExceeded
	}
	return &client.CompletionResponse{FinishReason: "tool_use", ToolCalls: c.calls}, nil
}

func TestRunTrace_StreamClaimAndNormalExecutionUseDistinctBatchOrdinals(t *testing.T) {
	read := &streamingProbeTool{name: "trace_stream_read", readOnly: true, started: make(chan struct{}), release: make(chan struct{})}
	write := &runTraceTool{name: "trace_normal_write", result: ToolResult{Content: "written"}}
	calls := []client.FunctionCall{
		*toolCallWithID(read.name, `{"path":"a"}`, "stream-read"),
		*toolCallWithID(write.name, `{"value":"b"}`, "normal-write"),
	}
	client := &mixedRunTraceStreamingClient{calls: calls, read: read}
	registry := NewToolRegistry()
	registry.Register(read)
	registry.Register(write)
	handler := &runTraceRecorder{}
	loop := NewAgentLoop(client, registry, "medium", t.TempDir(), 4, 2000, 200, nil, nil, nil)
	loop.SetEnableStreaming(true)
	loop.SetHandler(handler)

	if _, _, err := loop.Run(context.Background(), "read then write", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	tools := toolRunTraceEvents(handler.events)
	if len(tools) != 2 || tools[0].Tool.ExecutionBatchIndex == nil || tools[1].Tool.ExecutionBatchIndex == nil {
		t.Fatalf("tool trace = %+v", tools)
	}
	if *tools[0].Tool.ExecutionBatchIndex == *tools[1].Tool.ExecutionBatchIndex {
		t.Fatalf("stream claim and normal execution shared batch ordinal: %+v", tools)
	}
}

func TestRunTrace_PartialTerminalMatchesLastRunStatus(t *testing.T) {
	server := runTraceServer(t, []client.FunctionCall{
		*toolCallWithID("one_step", `{"value":"ok"}`, "call-one"),
	})
	defer server.Close()
	registry := NewToolRegistry()
	registry.Register(&runTraceTool{name: "one_step", result: ToolResult{Content: "step complete"}})
	handler := &runTraceRecorder{}
	loop := NewAgentLoop(client.NewGatewayClient(server.URL, ""), registry, "medium", "", 1, 2000, 200, nil, nil, nil)
	loop.SetHandler(handler)

	if _, _, err := loop.Run(context.Background(), "run one step", nil, nil); err == nil {
		t.Fatal("max-iteration run unexpectedly returned nil error")
	}
	status := loop.LastRunStatus()
	if !status.Partial {
		t.Fatalf("LastRunStatus = %+v, want partial", status)
	}
	var terminals []RunTraceEvent
	for _, event := range handler.events {
		if event.Type == RunTraceEventTerminal {
			terminals = append(terminals, event)
		}
	}
	if len(terminals) != 1 || terminals[0].Terminal == nil {
		t.Fatalf("terminal events = %+v, want exactly one", terminals)
	}
	terminal := terminals[0].Terminal
	if terminal.Partial != status.Partial || terminal.FailureCode != string(status.FailureCode) ||
		terminal.LastTool != status.LastTool || terminal.RetryCount != status.RetryCount ||
		terminal.IterationCount != status.IterationCount {
		t.Fatalf("terminal=%+v LastRunStatus=%+v", terminal, status)
	}
	if terminal.ProviderDispatchesAtTerminal != 2 || terminal.HelperDispatchesAtTerminal != 0 ||
		terminal.UnknownUsageDispatchesAtTerminal != 0 || terminal.TokenExposureAtTerminal != 15 ||
		terminal.TokenLimit != requestBudgetMinimumTokenExposure || terminal.TerminalTokenExposure != 15 {
		t.Fatalf("terminal budget snapshot = %+v", terminal)
	}
}

func TestRunTrace_CompactionLifecycleIsOrderedAndContentFree(t *testing.T) {
	handler := &runTraceRecorder{}
	loop := &AgentLoop{handler: handler, runTrace: newRunTraceEmitter(handler)}
	loop.runTrace.setIteration(3)
	loop.emitCompactionStatus("compaction_started", "preflight")
	loop.emitAppliedCompaction("preflight", 7)
	loop.emitCompactionStatus("compaction_finished", "preflight")

	if len(handler.events) != 3 {
		t.Fatalf("compaction events = %+v", handler.events)
	}
	wantStatus := []string{"compaction_started", "applied", "compaction_finished"}
	for index, event := range handler.events {
		if event.Seq != int64(index+1) || event.Iteration != 3 || event.Type != RunTraceEventCompaction ||
			event.Compaction == nil || event.Compaction.Phase != "preflight" || event.Compaction.Status != wantStatus[index] {
			t.Fatalf("event[%d] = %+v", index, event)
		}
	}
	if !handler.events[1].Compaction.Applied || handler.events[1].Compaction.MessagesDropped != 7 {
		t.Fatalf("applied event = %+v", handler.events[1])
	}
}
