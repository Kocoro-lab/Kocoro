package e2e

// Paid Fast-profile boundary probe for the production AgentLoop.
//
// Run:
//
//	SHANNON_E2E_LIVE=1 KOCORO_FAST_ITERATION_BOUNDARY=1 go test ./test/e2e -run TestLive_FastIterationBoundary -count=1 -v

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/executionprofile"
	"github.com/Kocoro-lab/ShanClaw/internal/runstatus"
)

const fastIterationBoundaryGateEnv = "KOCORO_FAST_ITERATION_BOUNDARY"

type iterationBoundaryProbeTool struct {
	mu       sync.Mutex
	target   int
	calls    int
	badCalls int
}

func (t *iterationBoundaryProbeTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "iteration_boundary_probe",
		Description: fmt.Sprintf(
			"Advance a strictly sequential %d-step chain. Start with step=1 and token=INIT; use the token returned by each step for the next call.",
			t.target,
		),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"step":  map[string]any{"type": "integer"},
				"token": map[string]any{"type": "string"},
			},
		},
		Required: []string{"step", "token"},
	}
}

func (t *iterationBoundaryProbeTool) RequiresApproval() bool     { return false }
func (t *iterationBoundaryProbeTool) IsReadOnlyCall(string) bool { return false }
func (t *iterationBoundaryProbeTool) TrustsDistinctOutcomeProgress() bool {
	return true
}

func iterationBoundaryToken(step int) string {
	return fmt.Sprintf("IB-%02d-9c", step)
}

func (t *iterationBoundaryProbeTool) Run(_ context.Context, argsJSON string) (agent.ToolResult, error) {
	var args struct {
		Step  int    `json:"step"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError("step and token are required"), nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	expectedStep := t.calls + 1
	expectedToken := "INIT"
	if expectedStep > 1 {
		expectedToken = iterationBoundaryToken(expectedStep - 1)
	}
	if args.Step != expectedStep || strings.TrimSpace(args.Token) != expectedToken {
		t.badCalls++
		return agent.ValidationError(fmt.Sprintf(
			"expected step=%d token=%s", expectedStep, expectedToken,
		)), nil
	}

	t.calls++
	if t.calls == t.target {
		return agent.ToolResult{Content: "CHAIN COMPLETE. Final code: ITER-BOUNDARY-OK."}, nil
	}
	return agent.ToolResult{Content: fmt.Sprintf(
		"STEP %d OK. Call step=%d with token=%s.",
		t.calls,
		t.calls+1,
		iterationBoundaryToken(t.calls),
	)}, nil
}

func (t *iterationBoundaryProbeTool) snapshot() (calls, badCalls int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls, t.badCalls
}

func TestLive_FastIterationBoundary(t *testing.T) {
	if os.Getenv("SHANNON_E2E_LIVE") != "1" {
		t.Skip("set SHANNON_E2E_LIVE=1 to authorize real provider calls")
	}
	if os.Getenv(fastIterationBoundaryGateEnv) != "1" {
		t.Skipf("set %s=1 to run the paid boundary probe", fastIterationBoundaryGateEnv)
	}

	cfg := loadAgentLabQualityConfig(t)
	provider := client.NewGatewayClient(cfg.endpoint, cfg.apiKey)
	resolveCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	cloudProfile, resolveErr := provider.ResolveKoeExecutionProfile(resolveCtx)
	cancel()
	fastProfile := executionprofile.Resolve(executionprofile.ResolutionInput{
		RequestedMode: executionprofile.ModeFast,
		FastEnabled:   true,
		CloudProfile:  &cloudProfile,
		CloudError:    resolveErr,
	})
	if resolveErr != nil || fastProfile.ValidateFast() != nil {
		t.Fatalf("Fast profile resolve failed (resolveErr=%v validate=%v)", resolveErr, fastProfile.ValidateFast())
	}

	const maxIterations = 25
	tests := []struct {
		name         string
		steps        int
		wantCalls    int
		wantLimitErr bool
		wantOutcome  string
		wantCode     bool
	}{
		{name: "terminal_response_on_iteration_N", steps: maxIterations - 1, wantCalls: maxIterations - 1, wantCode: true},
		{name: "tool_call_on_iteration_N", steps: maxIterations, wantCalls: maxIterations, wantLimitErr: true, wantOutcome: "completed", wantCode: true},
		{name: "tool_call_N_plus_1_is_blocked", steps: maxIterations + 1, wantCalls: maxIterations, wantLimitErr: true, wantOutcome: "partial"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probe := &iterationBoundaryProbeTool{target: tt.steps}
			registry := agent.NewToolRegistry()
			registry.Register(probe)
			loop := agent.NewAgentLoop(provider, registry, "medium", t.TempDir(), maxIterations, 30_000, 200, nil, nil, nil)
			loop.SetCacheSource("fast_iteration_boundary")
			loop.SetSkillDiscovery(false)
			loop.SetMaxTokens(700)
			loop.SetTemperature(0)
			loop.SetKoeExecutionProfile(fastProfile)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			started := time.Now()
			answer, usage, err := loop.Run(ctx, fmt.Sprintf(
				"Use iteration_boundary_probe to finish all %d sequential steps. Do not skip or batch steps. After the tool reports CHAIN COMPLETE, reply with the final code.",
				tt.steps,
			), nil, nil)
			calls, badCalls := probe.snapshot()
			status := loop.LastRunStatus()
			llmCalls := 0
			costUSD := 0.0
			if usage != nil {
				llmCalls = usage.LLMCalls
				costUSD = usage.CostUSD
			}
			t.Logf(
				"steps=%d calls=%d bad_calls=%d partial=%t failure_code=%s limit_error=%t llm_calls=%d latency_ms=%d cost_usd=%.6f answer=%q",
				tt.steps, calls, badCalls, status.Partial, status.FailureCode,
				errors.Is(err, agent.ErrMaxIterReached), llmCalls,
				time.Since(started).Milliseconds(), costUSD, answer,
			)

			if tt.wantLimitErr != errors.Is(err, agent.ErrMaxIterReached) {
				t.Fatalf("ErrMaxIterReached=%t, want %t; err=%v answer=%q", errors.Is(err, agent.ErrMaxIterReached), tt.wantLimitErr, err, answer)
			}
			if calls != tt.wantCalls || badCalls != 0 {
				t.Fatalf("probe calls=%d bad_calls=%d, want calls=%d bad_calls=0", calls, badCalls, tt.wantCalls)
			}
			if tt.wantLimitErr {
				if !status.Partial || status.FailureCode != runstatus.CodeIterationLimit {
					t.Fatalf("limit status=%+v, want partial iteration_limit", status)
				}
				if strings.TrimSpace(answer) == "" {
					t.Fatal("iteration-limit synthesis returned an empty answer")
				}
				if tt.wantOutcome != "" && !strings.Contains(strings.ToLower(answer), "**outcome** — "+tt.wantOutcome) {
					t.Fatalf("iteration-limit synthesis did not report outcome %q: %q", tt.wantOutcome, answer)
				}
			} else {
				if err != nil {
					t.Fatalf("clean boundary run returned error: %v", err)
				}
				if status.Partial || status.FailureCode != runstatus.CodeNone {
					t.Fatalf("clean boundary status=%+v", status)
				}
			}
			if tt.wantCode && !strings.Contains(answer, "ITER-BOUNDARY-OK") {
				t.Fatalf("boundary answer missing evidence-backed final code: %q", answer)
			}
			if !tt.wantCode && strings.Contains(answer, "ITER-BOUNDARY-OK") {
				t.Fatalf("incomplete boundary answer invented the final code: %q", answer)
			}

			if usage == nil {
				t.Fatal("provider usage was not observed")
			}
		})
	}
}
