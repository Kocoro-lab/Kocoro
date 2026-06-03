package tools

import (
	"context"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/daemon/desktop_rpc"
)

// CalendarListSourcesTool wraps calendar.list_sources — enumerates every
// calendar the user has configured through macOS Internet Accounts (iCloud,
// Google, Exchange, etc.). Read-only, no approval.
type CalendarListSourcesTool struct {
	Broker *desktop_rpc.DesktopRPCBroker
}

func (t *CalendarListSourcesTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "calendar_list_sources",
		Description: "List all calendar sources configured in macOS Internet Accounts: iCloud / Google / Exchange / Outlook / Local / subscription / other. " +
			"Each source has an id (use for calendar_ids filter in calendar_list_events), title, account_type, color_hex, and writable flag. " +
			"Read-only — does not modify any data.",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Required: []string{},
	}
}

func (t *CalendarListSourcesTool) RequiresApproval() bool { return false }

func (t *CalendarListSourcesTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	if t.Broker == nil {
		return agent.ToolResult{
			Content: "calendar_list_sources: Desktop RPC broker not available",
			IsError: true,
		}, nil
	}
	return callDesktopRPC(ctx, t.Broker, desktop_rpc.MethodCalendarListSources, struct{}{}, 0)
}
