package tools

import (
	"context"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agents"
)

// SubAgentResult holds the output of a completed sub-agent run.
type SubAgentResult struct {
	Output       string
	TotalTokens  int
	InputTokens  int
	OutputTokens int
	LLMCalls     int
	CostUSD      float64
	Duration     time.Duration
	Err          error
}

// SubAgentRunner builds and executes a child AgentLoop for a named agent.
// handler is an optional EventHandler to receive child loop tool events.
type SubAgentRunner func(ctx context.Context, ag *agents.Agent, prompt string, reg *agent.ToolRegistry, handler agent.EventHandler) SubAgentResult
