package tools

import (
	"context"
	"encoding/json"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/daemon/desktop_rpc"
)

// requestPermissionTimeoutMs is the daemon-side timeout for
// calendar.request_permission (spec §5.2 v0.5.1 钉死). The default 30s is
// too short — TCC system dialog can sit waiting 1-5 minutes for the user.
const requestPermissionTimeoutMs = 5 * 60 * 1000

// CalendarRequestPermissionTool wraps calendar.request_permission. Triggers
// macOS TCC dialog via Desktop. Approval-required: the model asks the user
// (via the standard approval card) before firing the system dialog, so the
// user sees the consent in context.
type CalendarRequestPermissionTool struct {
	Broker *desktop_rpc.DesktopRPCBroker
}

type calendarRequestPermissionArgs struct {
	Description string `json:"description,omitempty"`
}

func (t *CalendarRequestPermissionTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "calendar_request_permission",
		Description: "Request macOS calendar access from the user (triggers the system TCC dialog via Kocoro Desktop). " +
			"Use only when calendar_check_permission returned 'not_determined'. " +
			"Blocks up to 5 minutes waiting for the user to dismiss the dialog. " +
			"Result includes the new status (granted/denied/restricted)." +
			agent.DescriptionGuidance,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"description": agent.DescriptionFieldSpec,
			},
		},
		Required: []string{"description"},
	}
}

func (t *CalendarRequestPermissionTool) RequiresApproval() bool { return true }

func (t *CalendarRequestPermissionTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	if t.Broker == nil {
		return agent.ToolResult{
			Content: "calendar_request_permission: Desktop RPC broker not available",
			IsError: true,
		}, nil
	}
	var args calendarRequestPermissionArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return invalidArgResult("calendar_request_permission", "_", err.Error()), nil
	}
	if args.Description == "" {
		return agent.ValidationError("calendar_request_permission: missing required `description` parameter"), nil
	}
	// RPC params payload is empty (spec §5.2 request_permission has no params).
	return callDesktopRPC(ctx, t.Broker, desktop_rpc.MethodCalendarRequestPermission, struct{}{}, requestPermissionTimeoutMs)
}
