package test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

func TestSubAgentIntegration_FullRoundTrip(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")

		switch callCount {
		case 1:
			// Parent: calls subagent
			json.NewEncoder(w).Encode(client.CompletionResponse{
				Model: "test-model", FinishReason: "tool_use",
				FunctionCall: &client.FunctionCall{
					Name:      "subagent",
					Arguments: json.RawMessage(`{"prompt":"count files","description":"count","agent_type":"scout"}`),
				},
				Usage: client.Usage{InputTokens: 50, OutputTokens: 20, TotalTokens: 70},
			})
		case 2:
			// Child: returns result
			json.NewEncoder(w).Encode(client.CompletionResponse{
				Model: "test-model", OutputText: "42 files found.", FinishReason: "end_turn",
				Usage: client.Usage{InputTokens: 30, OutputTokens: 10, TotalTokens: 40},
			})
		default:
			// Parent: synthesizes final answer
			json.NewEncoder(w).Encode(client.CompletionResponse{
				Model: "test-model", OutputText: "Found 42 files.", FinishReason: "end_turn",
				Usage: client.Usage{InputTokens: 80, OutputTokens: 15, TotalTokens: 95},
			})
		}
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")

	// Setup agents directory with scout
	agentsDir := filepath.Join(t.TempDir(), "agents")
	os.MkdirAll(filepath.Join(agentsDir, "scout"), 0755)
	os.WriteFile(filepath.Join(agentsDir, "scout", "AGENT.md"),
		[]byte("You are Scout. Survey and report."), 0644)

	// Build registry with subagent
	reg := agent.NewToolRegistry()
	subagentTool := &tools.SubAgentTool{}
	subagentTool.SetAgentsDir(agentsDir)
	reg.Register(subagentTool)

	// Inject runner that builds a real child loop
	runner := func(ctx context.Context, ag *agents.Agent, prompt string, childReg *agent.ToolRegistry, handler agent.EventHandler) tools.SubAgentResult {
		start := time.Now()
		childLoop := agent.NewAgentLoop(gw, childReg, "medium", "", 10, 30000, 200, nil, nil, nil)
		agentDir := filepath.Join(agentsDir, ag.Name)
		childLoop.SwitchAgent(ag.Prompt, agentDir, nil, "", nil)

		childCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		output, usage, err := childLoop.Run(childCtx, prompt, nil)
		result := tools.SubAgentResult{Output: output, Duration: time.Since(start), Err: err}
		if usage != nil {
			result.InputTokens = usage.InputTokens
			result.OutputTokens = usage.OutputTokens
			result.TotalTokens = usage.TotalTokens
			result.LLMCalls = usage.LLMCalls
			result.CostUSD = usage.CostUSD
		}
		return result
	}
	subagentTool.SetRunner(runner)
	subagentTool.SetBaseRegistry(reg)

	// Run parent loop
	parentLoop := agent.NewAgentLoop(gw, reg, "medium", "", 10, 30000, 200, nil, nil, nil)
	result, _, err := parentLoop.Run(context.Background(), "How many files?", nil)
	if err != nil {
		t.Fatalf("parent loop: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}
	if callCount != 3 {
		t.Errorf("expected 3 LLM calls (parent→child→parent), got %d", callCount)
	}
}
