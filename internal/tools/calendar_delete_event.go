package tools

import (
	"context"
	"encoding/json"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/daemon/desktop_rpc"
)

// CalendarDeleteEventTool wraps calendar.delete_event. Approval-required.
// Accepts all three scopes (unlike update_event):
//   - this:            delete one instance (creates an exception in recurrence)
//   - this_and_future: delete this instance + all future
//   - all:             delete the entire recurring series (Desktop resolves
//                      instance → series master internally per spec §3.3)
type CalendarDeleteEventTool struct {
	Broker *desktop_rpc.DesktopRPCBroker
}

func (t *CalendarDeleteEventTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "calendar_delete_event",
		Description: "Delete a calendar event. scope:'this' deletes one instance, 'this_and_future' deletes this and forward, 'all' deletes the entire recurring series. " +
			"For 'all', pass either the master event id or any instance — Desktop resolves the series master automatically. " +
			"Non-recurring events: any scope works (always deletes the single event)." +
			agent.DescriptionGuidance,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":          map[string]any{"type": "string", "description": "EKEvent.eventIdentifier (instance or master)"},
				"scope":       map[string]any{"type": "string", "enum": []string{"this", "this_and_future", "all"}, "description": "Deletion scope"},
				"description": agent.DescriptionFieldSpec,
			},
		},
		Required: []string{"id", "scope", "description"},
	}
}

func (t *CalendarDeleteEventTool) RequiresApproval() bool { return true }

func (t *CalendarDeleteEventTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	if t.Broker == nil {
		return agent.ToolResult{
			Content: "calendar_delete_event: Desktop RPC broker not available",
			IsError: true,
		}, nil
	}
	var args struct {
		ID          string `json:"id"`
		Scope       string `json:"scope"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return invalidArgResult("calendar_delete_event", "_", err.Error()), nil
	}
	if args.ID == "" {
		return agent.ValidationError("calendar_delete_event: missing required `id` parameter"), nil
	}
	if args.Description == "" {
		return agent.ValidationError("calendar_delete_event: missing required `description` parameter"), nil
	}
	switch args.Scope {
	case desktop_rpc.ScopeThis, desktop_rpc.ScopeThisAndFuture, desktop_rpc.ScopeAll:
		// OK
	case "":
		return agent.ValidationError("calendar_delete_event: missing required `scope` parameter (must be 'this', 'this_and_future', or 'all')"), nil
	default:
		return invalidArgResult("calendar_delete_event", "scope", "must be 'this', 'this_and_future', or 'all'"), nil
	}
	rpcParams := stripDescription([]byte(argsJSON))
	return callDesktopRPCRaw(ctx, t.Broker, desktop_rpc.MethodCalendarDeleteEvent, rpcParams, 0)
}
