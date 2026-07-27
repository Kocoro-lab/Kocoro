package tools

import (
	"context"
	"encoding/json"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/daemon/desktop_rpc"
)

// CalendarUpdateEventTool wraps calendar.update_event. Approval-required.
//
// patch semantics (spec §5.2 v0.5.1 钉死) — Desktop side enforces:
//   - missing key / null      = don't change
//   - "" or [] for clearable  = clear field
//   - attendees/alarms non-empty = full replace (not merge)
//   - start/end cannot be cleared (return invalid_argument)
//
// To remove recurrence, pass top-level clear_recurrence: true (not patch
// recurrence_rule: null).
type CalendarUpdateEventTool struct {
	Broker *desktop_rpc.DesktopRPCBroker
}

func (t *CalendarUpdateEventTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "calendar_update_event",
		Description: "Update an existing calendar event. scope is 'this' or 'this_and_future' (no 'all' — use calendar_delete_event scope='all' + calendar_create_event instead). " +
			"⚠️ scope='this_and_future' splits the recurring series — the result.id may be a NEW id; use the returned id going forward. " +
			"patch follows v0.5.1 semantics: missing/null = no change, empty string/array = clear (except start/end can't be cleared), lists are replaced not merged. " +
			"To remove recurrence, set clear_recurrence: true (not patch.recurrence_rule)." +
			agent.DescriptionGuidance,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":               map[string]any{"type": "string", "description": "EKEvent.eventIdentifier"},
				"scope":            map[string]any{"type": "string", "enum": []string{"this", "this_and_future"}, "description": "Update scope (no 'all' — see description)"},
				"patch":            map[string]any{"type": "object", "description": "Fields to change (only what's named)"},
				"clear_recurrence": map[string]any{"type": "boolean", "description": "If true, removes recurrence (overrides patch.recurrence_rule)"},
				"description":      agent.DescriptionFieldSpec,
			},
		},
		Required: []string{"id", "scope", "description"},
	}
}

func (t *CalendarUpdateEventTool) RequiresApproval() bool { return true }

func (t *CalendarUpdateEventTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	if result, valid := agent.ValidateToolArguments(t.Info(), argsJSON); !valid {
		return result, nil
	}
	if t.Broker == nil {
		return agent.ToolResult{
			Content: "calendar_update_event: Desktop RPC broker not available",
			IsError: true,
		}, nil
	}
	var args struct {
		ID          string `json:"id"`
		Scope       string `json:"scope"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return invalidArgResult("calendar_update_event", "_", err.Error()), nil
	}
	switch args.Scope {
	case desktop_rpc.ScopeThis, desktop_rpc.ScopeThisAndFuture:
		// OK
	case desktop_rpc.ScopeAll:
		// Spec §5.5.8: update_event does NOT support scope='all'. Catch
		// client-side so we don't even RPC.
		return invalidArgResult("calendar_update_event", "scope", "'all' is not supported for update; use calendar_delete_event(scope='all') + calendar_create_event"), nil
	default:
		return invalidArgResult("calendar_update_event", "scope", "must be 'this' or 'this_and_future'"), nil
	}
	rpcParams := stripDescription([]byte(argsJSON))
	return callDesktopRPCRaw(ctx, t.Broker, desktop_rpc.MethodCalendarUpdateEvent, rpcParams, 0)
}
