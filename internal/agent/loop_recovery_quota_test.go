package agent

// Pins for the critical-loop recovery quota (criticalLoopRecoveryIteration in
// loop.go). The quota is per trailing window (nudgeWindowIters): an atomic
// batch veto more than one window after the previous recovery turn is a new
// incident and earns its own recovery response; a repeat within the window
// still force-stops. The per-run one-shot quota this replaced terminated
// recoverable second stalls in long trajectories — the first test scripts
// exactly that trajectory and asserts the second stall now completes.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestAgentLoopSecondDistantStallEarnsNewRecoveryTurn(t *testing.T) {
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
			// Sixth identical call → batch veto → recovery turn granted at
			// iteration 5.
			json.NewEncoder(w).Encode(nativeResponseWithID("", "tool_use",
				toolCallWithID("alpha_status", `{"id":"job-a"}`, "alpha_6"), 10, 5))
		case llmCallCount <= 11:
			// Recovery succeeds: five productive steps with distinct args.
			json.NewEncoder(w).Encode(nativeResponseWithID("", "tool_use",
				toolCallWithID("worker_step", fmt.Sprintf(`{"n":%d}`, llmCallCount-6), fmt.Sprintf("work_%d", llmCallCount)), 10, 5))
		case llmCallCount <= 16:
			// Second, unrelated stall: beta_status polls stable five times.
			// Its veto lands at iteration 16 — more than nudgeWindowIters
			// past the iteration-5 recovery — so it is a new incident.
			json.NewEncoder(w).Encode(nativeResponseWithID("", "tool_use",
				toolCallWithID("beta_status", `{"id":"job-b"}`, fmt.Sprintf("beta_%d", llmCallCount)), 10, 5))
		case llmCallCount == 17:
			// Sixth identical beta call → second veto → NEW recovery turn.
			json.NewEncoder(w).Encode(nativeResponseWithID("", "tool_use",
				toolCallWithID("beta_status", `{"id":"job-b"}`, "beta_6"), 10, 5))
		case llmCallCount == 18:
			// The recovery turn the per-run quota used to deny: take the
			// productive continuation.
			secondRecoveryConsumed = true
			json.NewEncoder(w).Encode(nativeResponseWithID("", "tool_use",
				toolCallWithID("worker_step", `{"n":99}`, "work_recover"), 10, 5))
		default:
			json.NewEncoder(w).Encode(nativeResponse("both jobs done", "end_turn", nil, 10, 5))
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
	if result != "both jobs done" {
		t.Fatalf("distant second stall should recover and complete, got %q", result)
	}
	if !secondRecoveryConsumed {
		t.Fatal("second stall never received its recovery turn — quota regressed to per-run")
	}
	if alphaTool.runs != 5 || betaTool.runs != 5 {
		t.Fatalf("blocked sixth calls must not execute: alpha=%d beta=%d", alphaTool.runs, betaTool.runs)
	}
	if workerTool.runs.Load() != 6 {
		t.Fatalf("productive middle (5) plus second recovery step (1) should run, got %d", workerTool.runs.Load())
	}
	// 19 = 5 alpha polls + first veto turn + 5 productive + 5 beta polls +
	// second veto turn (new recovery) + recovery step + completion.
	if llmCallCount != 19 {
		t.Fatalf("expected 19 provider calls, got %d", llmCallCount)
	}
}

func TestAgentLoopSecondStallWithinWindowStillForceStops(t *testing.T) {
	llmCallCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		llmCallCount++
		switch {
		case llmCallCount <= 5:
			json.NewEncoder(w).Encode(nativeResponseWithID("", "tool_use",
				toolCallWithID("alpha_status", `{"id":"job-a"}`, fmt.Sprintf("alpha_%d", llmCallCount)), 10, 5))
		case llmCallCount == 6:
			// First veto at iteration 5; recovery turn granted.
			json.NewEncoder(w).Encode(nativeResponseWithID("", "tool_use",
				toolCallWithID("alpha_status", `{"id":"job-a"}`, "alpha_6"), 10, 5))
		case llmCallCount <= 10:
			// Recovery does a little productive work (iterations 6-9) — just
			// enough to keep the separate 3-nudges-in-10 escalation quiet so
			// this test isolates the recovery-window rule.
			json.NewEncoder(w).Encode(nativeResponseWithID("", "tool_use",
				toolCallWithID("worker_step", fmt.Sprintf(`{"n":%d}`, llmCallCount-6), fmt.Sprintf("work_%d", llmCallCount)), 10, 5))
		case llmCallCount <= 15:
			// New stall: beta_status polls stable five times (iterations 10-14).
			json.NewEncoder(w).Encode(nativeResponseWithID("", "tool_use",
				toolCallWithID("beta_status", `{"id":"job-b"}`, fmt.Sprintf("beta_%d", llmCallCount)), 10, 5))
		case llmCallCount == 16:
			// Sixth identical beta call → veto at iteration 15, exactly
			// nudgeWindowIters after the iteration-5 recovery — still inside
			// the window (boundary inclusive) → force stop.
			json.NewEncoder(w).Encode(nativeResponseWithID("", "tool_use",
				toolCallWithID("beta_status", `{"id":"job-b"}`, "beta_6"), 10, 5))
		default:
			json.NewEncoder(w).Encode(nativeResponse("partial: stalled again inside the recovery window", "end_turn", nil, 10, 5))
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
	if result != "partial: stalled again inside the recovery window" {
		t.Fatalf("in-window second stall must force-stop, got %q", result)
	}
	if alphaTool.runs != 5 || betaTool.runs != 5 {
		t.Fatalf("blocked sixth calls must not execute: alpha=%d beta=%d", alphaTool.runs, betaTool.runs)
	}
	if workerTool.runs.Load() != 4 {
		t.Fatalf("productive spacer should run 4 times, got %d", workerTool.runs.Load())
	}
	// 17 = 5 alpha polls + first veto turn + 4 productive + 5 beta polls +
	// in-window veto turn (force stop, no recovery) + synthesis.
	if llmCallCount != 17 {
		t.Fatalf("expected 17 provider calls (no second recovery inside the window), got %d", llmCallCount)
	}
}
