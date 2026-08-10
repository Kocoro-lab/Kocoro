package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/runstatus"
)

func registerIterationBoundaryTools(reg *ToolRegistry, count int) []*mockTool {
	tools := make([]*mockTool, 0, count)
	for i := 1; i <= count; i++ {
		tool := &mockTool{name: fmt.Sprintf("boundary_step_%02d", i)}
		reg.Register(tool)
		tools = append(tools, tool)
	}
	return tools
}

// TestAgentLoopIterationBoundary_TerminalOnNthTurn verifies that maxIter is an
// inclusive budget for a normal terminal response: N-1 tool rounds followed by
// an end_turn on round N completes without invoking cap synthesis.
func TestAgentLoopIterationBoundary_TerminalOnNthTurn(t *testing.T) {
	const maxIter = 4
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if requestCount < maxIter {
			_ = json.NewEncoder(w).Encode(nativeResponse(
				"", "tool_use",
				toolCall(fmt.Sprintf("boundary_step_%02d", requestCount), fmt.Sprintf(`{"step":%d}`, requestCount)),
				10, 5,
			))
			return
		}
		_ = json.NewEncoder(w).Encode(nativeResponse("completed at the boundary", "end_turn", nil, 10, 5))
	}))
	defer server.Close()

	reg := NewToolRegistry()
	tools := registerIterationBoundaryTools(reg, maxIter-1)
	loop := NewAgentLoop(client.NewGatewayClient(server.URL, ""), reg, "medium", "", maxIter, 2000, 200, nil, nil, nil)

	result, _, err := loop.Run(context.Background(), "finish in exactly four model turns", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "completed at the boundary" {
		t.Fatalf("result = %q", result)
	}
	if requestCount != maxIter {
		t.Fatalf("completion requests = %d, want %d", requestCount, maxIter)
	}
	for i, tool := range tools {
		if got := tool.runs.Load(); got != 1 {
			t.Errorf("tool %d executions = %d, want 1", i+1, got)
		}
	}
	status := loop.LastRunStatus()
	if status.Partial || status.FailureCode != runstatus.CodeNone || status.IterationCount != maxIter {
		t.Fatalf("run status = %+v, want clean completion at iteration %d", status, maxIter)
	}
}

// TestAgentLoopIterationBoundary_ToolOnNthTurnBlocksNthPlusOne verifies that a
// tool call consumes round N. The loop must not grant an ordinary N+1 turn to
// claim success; it may only issue the tool-disabled partial synthesis turn.
func TestAgentLoopIterationBoundary_ToolOnNthTurnBlocksNthPlusOne(t *testing.T) {
	const maxIter = 4
	ordinaryRequests := 0
	synthesisRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req client.CompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode completion request: %v", err)
		}
		if len(req.Tools) == 0 {
			synthesisRequests++
			_ = json.NewEncoder(w).Encode(nativeResponse(
				"**Outcome** — partial\n**Pending / blocked** — terminal answer needed another turn.",
				"end_turn", nil, 10, 5,
			))
			return
		}
		ordinaryRequests++
		_ = json.NewEncoder(w).Encode(nativeResponse(
			"", "tool_use",
			toolCall(fmt.Sprintf("boundary_step_%02d", ordinaryRequests), fmt.Sprintf(`{"step":%d}`, ordinaryRequests)),
			10, 5,
		))
	}))
	defer server.Close()

	reg := NewToolRegistry()
	tools := registerIterationBoundaryTools(reg, maxIter)
	loop := NewAgentLoop(client.NewGatewayClient(server.URL, ""), reg, "medium", "", maxIter, 2000, 200, nil, nil, nil)

	result, _, err := loop.Run(context.Background(), "need one more turn after four tool rounds", nil, nil)
	if !errors.Is(err, ErrMaxIterReached) {
		t.Fatalf("Run error = %v, want ErrMaxIterReached", err)
	}
	if ordinaryRequests != maxIter {
		t.Fatalf("ordinary completion requests = %d, want %d", ordinaryRequests, maxIter)
	}
	if synthesisRequests != 1 {
		t.Fatalf("synthesis requests = %d, want 1", synthesisRequests)
	}
	if result == "" {
		t.Fatal("cap synthesis returned no partial report")
	}
	for i, tool := range tools {
		if got := tool.runs.Load(); got != 1 {
			t.Errorf("tool %d executions = %d, want 1", i+1, got)
		}
	}
	status := loop.LastRunStatus()
	if !status.Partial || status.FailureCode != runstatus.CodeIterationLimit || status.IterationCount != maxIter {
		t.Fatalf("run status = %+v, want partial iteration-limit at iteration %d", status, maxIter)
	}
}

// TestAgentLoopIterationBoundary_GUIActivationOnNthTurn verifies that a real
// GUI tool executed on round 25 activates the intended 75-turn budget before
// the next loop-boundary check, allowing a terminal response on round 26.
func TestAgentLoopIterationBoundary_GUIActivationOnNthTurn(t *testing.T) {
	const baseMaxIter = 25
	requestCount := 0
	synthesisRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req client.CompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode completion request: %v", err)
		}
		if len(req.Tools) == 0 {
			synthesisRequests++
			_ = json.NewEncoder(w).Encode(nativeResponse("unexpected cap synthesis", "end_turn", nil, 10, 5))
			return
		}
		requestCount++
		switch {
		case requestCount < baseMaxIter:
			_ = json.NewEncoder(w).Encode(nativeResponse(
				"", "tool_use",
				toolCall(fmt.Sprintf("boundary_step_%02d", requestCount), fmt.Sprintf(`{"step":%d}`, requestCount)),
				10, 5,
			))
		case requestCount == baseMaxIter:
			_ = json.NewEncoder(w).Encode(nativeResponse(
				"", "tool_use", toolCall("screenshot", `{"step":25}`), 10, 5,
			))
		default:
			_ = json.NewEncoder(w).Encode(nativeResponse("completed after GUI activation", "end_turn", nil, 10, 5))
		}
	}))
	defer server.Close()

	reg := NewToolRegistry()
	tools := registerIterationBoundaryTools(reg, baseMaxIter-1)
	screenshot := &mockTool{name: "screenshot"}
	reg.Register(screenshot)
	loop := NewAgentLoop(client.NewGatewayClient(server.URL, ""), reg, "medium", "", baseMaxIter, 2000, 200, nil, nil, nil)

	result, _, err := loop.Run(context.Background(), "use GUI at the base budget boundary", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "completed after GUI activation" {
		t.Fatalf("result = %q", result)
	}
	if requestCount != baseMaxIter+1 || synthesisRequests != 0 {
		t.Fatalf("ordinary requests = %d, synthesis requests = %d; want 26 and 0", requestCount, synthesisRequests)
	}
	for i, tool := range tools {
		if got := tool.runs.Load(); got != 1 {
			t.Errorf("tool %d executions = %d, want 1", i+1, got)
		}
	}
	if got := screenshot.runs.Load(); got != 1 {
		t.Fatalf("screenshot executions = %d, want 1", got)
	}
	status := loop.LastRunStatus()
	if status.Partial || status.FailureCode != runstatus.CodeNone || status.IterationCount != baseMaxIter+1 {
		t.Fatalf("run status = %+v, want clean completion at iteration %d", status, baseMaxIter+1)
	}
}

type approvalBoundaryTool struct{ *mockTool }

func (*approvalBoundaryTool) RequiresApproval() bool { return true }

// TestAgentLoopIterationBoundary_RejectedGUIRequestDoesNotExtendBudget keeps
// the dynamic GUI allowance tied to dispatch, not merely to a model-emitted
// tool name. Rejected calls still belong in request telemetry and loop-detector
// history, but cannot purchase another 50 model turns without executing.
func TestAgentLoopIterationBoundary_RejectedGUIRequestDoesNotExtendBudget(t *testing.T) {
	const baseMaxIter = 25
	tests := []struct {
		name      string
		setupTool func(*ToolRegistry) *mockTool
		handler   func() EventHandler
	}{
		{
			name: "unregistered",
			setupTool: func(*ToolRegistry) *mockTool {
				return nil
			},
		},
		{
			name: "validation rejected",
			setupTool: func(reg *ToolRegistry) *mockTool {
				tool := &mockTool{name: "screenshot", required: []string{"description"}}
				reg.Register(tool)
				return tool
			},
		},
		{
			name: "approval denied",
			setupTool: func(reg *ToolRegistry) *mockTool {
				tool := &mockTool{name: "screenshot"}
				reg.Register(&approvalBoundaryTool{mockTool: tool})
				return tool
			},
			handler: func() EventHandler {
				return &mockHandler{approveResult: false}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ordinaryRequests := 0
			synthesisRequests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req client.CompletionRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Errorf("decode completion request: %v", err)
				}
				if len(req.Tools) == 0 {
					synthesisRequests++
					_ = json.NewEncoder(w).Encode(nativeResponse(
						"**Outcome** — partial\n**Pending / blocked** — GUI call did not dispatch before the base cap.",
						"end_turn", nil, 10, 5,
					))
					return
				}

				ordinaryRequests++
				if ordinaryRequests < baseMaxIter {
					_ = json.NewEncoder(w).Encode(nativeResponse(
						"", "tool_use",
						toolCall(fmt.Sprintf("boundary_step_%02d", ordinaryRequests), fmt.Sprintf(`{"step":%d}`, ordinaryRequests)),
						10, 5,
					))
					return
				}
				if ordinaryRequests == baseMaxIter {
					_ = json.NewEncoder(w).Encode(nativeResponse("", "tool_use", toolCall("screenshot", `{}`), 10, 5))
					return
				}
				_ = json.NewEncoder(w).Encode(nativeResponse("incorrectly continued", "end_turn", nil, 10, 5))
			}))
			defer server.Close()

			reg := NewToolRegistry()
			registerIterationBoundaryTools(reg, baseMaxIter-1)
			guiTool := tt.setupTool(reg)
			loop := NewAgentLoop(client.NewGatewayClient(server.URL, ""), reg, "medium", "", baseMaxIter, 2000, 200, nil, nil, nil)
			if tt.handler != nil {
				loop.SetHandler(tt.handler())
			}

			_, _, err := loop.Run(context.Background(), "reject GUI at the base budget boundary", nil, nil)
			if !errors.Is(err, ErrMaxIterReached) {
				t.Fatalf("Run error = %v, want ErrMaxIterReached", err)
			}
			if ordinaryRequests != baseMaxIter || synthesisRequests != 1 {
				t.Fatalf("ordinary requests = %d, synthesis requests = %d; want 25 and 1", ordinaryRequests, synthesisRequests)
			}
			if guiTool != nil && guiTool.runs.Load() != 0 {
				t.Fatalf("rejected screenshot executions = %d, want 0", guiTool.runs.Load())
			}
			status := loop.LastRunStatus()
			if !status.Partial || status.FailureCode != runstatus.CodeIterationLimit || status.IterationCount != baseMaxIter {
				t.Fatalf("run status = %+v, want partial iteration-limit at iteration %d", status, baseMaxIter)
			}
		})
	}
}

type progressingBoundaryTool struct{ runs int }

func (*progressingBoundaryTool) Info() ToolInfo {
	return ToolInfo{
		Name:        "boundary_progress",
		Description: "advance one dependent boundary-test step",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

func (t *progressingBoundaryTool) Run(context.Context, string) (ToolResult, error) {
	t.runs++
	return ToolResult{Content: fmt.Sprintf("step %02d complete; next token token-%02d", t.runs, t.runs)}, nil
}

func (*progressingBoundaryTool) RequiresApproval() bool { return false }

func (*progressingBoundaryTool) TrustsDistinctOutcomeProgress() bool { return true }

// TestAgentLoopIterationBoundary_ChangingSameToolProgressReachesTerminal locks
// the real long-chain shape: one stateful tool, strictly changing arguments,
// and a distinct successful receipt for every dependent step. Generic
// NoProgress nudges must not accumulate into an early detector force-stop.
func TestAgentLoopIterationBoundary_ChangingSameToolProgressReachesTerminal(t *testing.T) {
	const (
		maxIter   = 25
		toolSteps = maxIter - 1
	)
	requestCount := 0
	synthesisRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req client.CompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode completion request: %v", err)
		}
		if len(req.Tools) == 0 {
			synthesisRequests++
			_ = json.NewEncoder(w).Encode(nativeResponse("unexpected detector synthesis", "end_turn", nil, 10, 5))
			return
		}
		requestCount++
		if requestCount <= toolSteps {
			_ = json.NewEncoder(w).Encode(nativeResponse(
				"", "tool_use",
				toolCall("boundary_progress", fmt.Sprintf(`{"step":%d,"token":"token-%02d"}`, requestCount, requestCount-1)),
				10, 5,
			))
			return
		}
		_ = json.NewEncoder(w).Encode(nativeResponse("all 24 dependent steps completed", "end_turn", nil, 10, 5))
	}))
	defer server.Close()

	tool := &progressingBoundaryTool{}
	reg := NewToolRegistry()
	reg.Register(tool)
	loop := NewAgentLoop(client.NewGatewayClient(server.URL, ""), reg, "medium", "", maxIter, 2000, 200, nil, nil, nil)

	result, _, err := loop.Run(context.Background(), "complete all dependent steps", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "all 24 dependent steps completed" {
		t.Fatalf("result = %q", result)
	}
	if requestCount != maxIter || synthesisRequests != 0 || tool.runs != toolSteps {
		t.Fatalf("requests=%d synthesis=%d tool runs=%d; want 25, 0, 24", requestCount, synthesisRequests, tool.runs)
	}
	status := loop.LastRunStatus()
	if status.Partial || status.FailureCode != runstatus.CodeNone || status.IterationCount != maxIter {
		t.Fatalf("run status = %+v, want clean completion at iteration %d", status, maxIter)
	}
}
