package tools

import "github.com/Kocoro-lab/ShanClaw/internal/agent"

func definitiveLocalResourceError(message string) agent.ToolResult {
	result := agent.BusinessError(message)
	result.StopFurtherTools = true
	return result
}
