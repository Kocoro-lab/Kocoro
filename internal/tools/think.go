package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// ThinkTool lets the model reason or plan before acting. The model calls this
// tool instead of outputting plan text, giving the loop an explicit continuation
// signal (stop_reason: tool_use) rather than relying on text heuristics.
type ThinkTool struct{}

type thinkArgs struct {
	Thought string `json:"thought"`
}

func (t *ThinkTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        "think",
		Description: "Use this tool to think about something. It will not obtain new information or change any state — it just appends the thought to the log. Use it when complex reasoning or sequential decisions are needed (long tool chains, policy-heavy tasks). For simpler reasoning extended thinking handles it natively.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"thought": map[string]any{"type": "string", "description": "Your reasoning or plan"},
			},
		},
		Required: []string{"thought"},
	}
}

func (t *ThinkTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	var args thinkArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("invalid arguments: %v", err), IsError: true}, nil
	}
	if args.Thought == "" {
		return agent.ToolResult{Content: "thought is required", IsError: true}, nil
	}
	// Short ack instead of echoing the thought back. The thought already
	// lives in the assistant message's tool_use.input.thought field — the
	// model can reference its own past reasoning from there. Echoing into
	// the tool_result was double-counting the thought against cache
	// (assistant tool_use input + user tool_result content). Cuts ~50% of
	// think-related cache writes per session.
	return agent.ToolResult{Content: "thought logged"}, nil
}

func (t *ThinkTool) RequiresApproval() bool { return false }

func (t *ThinkTool) IsReadOnlyCall(string) bool { return true }

// SkillExempt opts think out of skill allowed-tools restriction. Pure
// reasoning, no I/O — restricting it would only force the model to substitute
// plan text into its assistant message, which is strictly worse for the loop.
func (t *ThinkTool) SkillExempt() bool { return true }
