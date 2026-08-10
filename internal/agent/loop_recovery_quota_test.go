package agent

// Decision-support pins for the critical-loop recovery quota
// (criticalLoopRecoveryUsed in loop.go). The quota is per-run and one-shot:
// the FIRST atomic batch veto gets a recovery response with normal tools; any
// later veto — even for an unrelated stall much further into the run — force
// stops immediately. These tests pin that behavior and stage the evidence for
// the "should the quota be per-window instead?" decision: the fake provider
// scripts a productive continuation the second stall would have taken if it
// had been offered the same recovery chance the first one got, and asserts it
// is never requested.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestAgentLoopSecondUnrelatedStallGetsNoRecoveryTurn(t *testing.T) {
	llmCallCount := 0
	secondRecoveryConsumed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		llmCallCount++
		switch {
		case llmCallCount <= 5:
			// First stall: alpha_status polls the same args with a stable
			// outcome five times (all execute), so the sixth is provably stuck.
			json.NewEncoder(w).Encode(nativeResponseWithID("", "tool_use",
				toolCallWithID("alpha_status", `{"id":"job-a"}`, fmt.Sprintf("alpha_%d", llmCallCount)), 10, 5))
		case llmCallCount == 6:
			// Sixth identical call → batch veto → this run's single recovery
			// turn is spent here.
			json.NewEncoder(w).Encode(nativeResponseWithID("", "tool_use",
				toolCallWithID("alpha_status", `{"id":"job-a"}`, "alpha_6"), 10, 5))
		case llmCallCount <= 11:
			// Recovery succeeds: five productive steps with distinct args.
			json.NewEncoder(w).Encode(nativeResponseWithID("", "tool_use",
				toolCallWithID("worker_step", fmt.Sprintf(`{"n":%d}`, llmCallCount-6), fmt.Sprintf("work_%d", llmCallCount)), 10, 5))
		case llmCallCount <= 16:
			// Second, unrelated stall: beta_status polls stable five times.
			json.NewEncoder(w).Encode(nativeResponseWithID("", "tool_use",
				toolCallWithID("beta_status", `{"id":"job-b"}`, fmt.Sprintf("beta_%d", llmCallCount)), 10, 5))
		case llmCallCount == 17:
			// Sixth identical beta call → second veto. Current behavior: the
			// quota is spent, so this force-stops with NO recovery turn.
			json.NewEncoder(w).Encode(nativeResponseWithID("", "tool_use",
				toolCallWithID("beta_status", `{"id":"job-b"}`, "beta_6"), 10, 5))
		case llmCallCount == 18:
			// Force-stop synthesis of the final answer.
			json.NewEncoder(w).Encode(nativeResponse("partial: stopped after second stall", "end_turn", nil, 10, 5))
		default:
			// Productive continuation the second stall WOULD take if it were
			// offered a recovery turn (per-window quota). Never requested
			// under the current per-run quota — the assertion below is the
			// decision evidence.
			secondRecoveryConsumed = true
			json.NewEncoder(w).Encode(nativeResponseWithID("", "tool_use",
				toolCallWithID("worker_step", `{"n":99}`, "work_recover"), 10, 5))
		}
	}))
	defer server.Close()

	alphaTool := &changingReadOnlyTool{name: "alpha_status", results: []string{"running"}}
	betaTool := &changingReadOnlyTool{name: "beta_status", results: []string{"pending"}}
	workerTool := &mockTool{name: "worker_step"}
	registry := NewToolRegistry()
	registry.Register(alphaTool)
	registry.Register(betaTool)
	registry.Register(workerTool)
	loop := NewAgentLoop(client.NewGatewayClient(server.URL, ""), registry, "medium", "", 40, 2000, 200, nil, nil, nil)
	loop.SetEnableStreaming(false)
	loop.SetHandler(&mockHandler{approveResult: true})

	result, _, err := loop.Run(context.Background(), "finish both jobs", nil, nil)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if result != "partial: stopped after second stall" {
		t.Fatalf("expected force-stop synthesis, got %q", result)
	}
	if alphaTool.runs != 5 || betaTool.runs != 5 {
		t.Fatalf("blocked sixth calls must not execute: alpha=%d beta=%d", alphaTool.runs, betaTool.runs)
	}
	if workerTool.runs.Load() != 5 {
		t.Fatalf("productive middle should have run 5 times, got %d", workerTool.runs.Load())
	}
	// 18 = 5 alpha polls + veto turn + 5 productive + 5 beta polls + second
	// veto turn + force-stop synthesis. A per-window quota would instead show
	// >= 19 (a recovery request after the second veto).
	if llmCallCount != 18 {
		t.Fatalf("expected 18 provider calls (no second recovery turn), got %d", llmCallCount)
	}
	if secondRecoveryConsumed {
		t.Fatal("second stall unexpectedly received a recovery turn — quota is no longer per-run; update the recovery-quota decision record")
	}
}
