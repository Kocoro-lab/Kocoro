package tools

import (
	"context"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/daemon/desktop_rpc"
)

// CalendarCheckPermissionTool wraps the calendar.check_permission RPC.
// Read-only, no approval — used internally by the agent to decide whether
// to ask the user to authorize before calling other calendar tools.
type CalendarCheckPermissionTool struct {
	Broker *desktop_rpc.DesktopRPCBroker
}

func (t *CalendarCheckPermissionTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "calendar_check_permission",
		Description: "Check whether the user has granted Kocoro Desktop calendar (EventKit) access. " +
			"Returns one of: not_determined / restricted / denied / granted / write_only. " +
			"Use this before any other calendar_* tool to decide if you need to ask the user to authorize.",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Required: []string{},
	}
}

func (t *CalendarCheckPermissionTool) RequiresApproval() bool { return false }

// IsReadOnlyCall lets the partitioner batch this with other reads. Each call
// is an independent RPC (unique request_id); the broker is concurrency-safe
// and WriteFrame is serialized by the listener's writeMu, so concurrent reads
// have no correctness cost.
func (t *CalendarCheckPermissionTool) IsReadOnlyCall(string) bool { return true }

func (t *CalendarCheckPermissionTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	if t.Broker == nil {
		return agent.ToolResult{
			Content: "calendar_check_permission: Desktop RPC broker not available (TUI / one-shot / scheduled mode does not support calendar tools)",
			IsError: true,
		}, nil
	}
	return callDesktopRPC(ctx, t.Broker, desktop_rpc.MethodCalendarCheckPermission, struct{}{}, 0)
}
