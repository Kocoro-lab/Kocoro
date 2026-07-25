package tools

import (
	"context"
	"encoding/json"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/daemon/desktop_rpc"
)

// CalendarListEventsTool wraps calendar.list_events — fetches events in a
// time window across one or more calendar sources. Read-only, no approval.
type CalendarListEventsTool struct {
	Broker *desktop_rpc.DesktopRPCBroker
}

// calendarListEventsArgs mirrors the spec §5.2 params schema. Optional fields
// are pointer / nil-eligible so we can distinguish "absent" from "explicit
// zero/empty" where it matters.
type calendarListEventsArgs struct {
	Start       string    `json:"start"`
	End         string    `json:"end"`
	CalendarIDs *[]string `json:"calendar_ids,omitempty"`
	Query       *string   `json:"query,omitempty"`
	Limit       int       `json:"limit,omitempty"`
}

func (t *CalendarListEventsTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "calendar_list_events",
		Description: "List calendar events in a time window. Times must be RFC 3339 with timezone offset (e.g. 2026-05-26T09:00:00+08:00). " +
			"Optional calendar_ids filter (null = all calendars, [] = empty result, [ids...] = specified). " +
			"Optional query for substring match against title + notes. " +
			"limit default 500, max 2000. Returns events list + truncated:bool. " +
			"For recurring instances, series_master_id points to the series master event id (used with calendar_delete_event scope='all').",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"start": map[string]any{
					"type":        "string",
					"description": "Window start, RFC 3339 with timezone offset",
				},
				"end": map[string]any{
					"type":        "string",
					"description": "Window end, RFC 3339 with timezone offset",
				},
				"calendar_ids": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Filter to specific calendar ids; null/omitted = all",
				},
				"query": map[string]any{
					"type":        "string",
					"description": "Optional case-insensitive substring filter (title + notes)",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Max events (default 500, max 2000)",
				},
			},
		},
		Required: []string{"start", "end"},
	}
}

func (t *CalendarListEventsTool) RequiresApproval() bool { return false }

// IsReadOnlyCall — see CalendarCheckPermissionTool; concurrent reads are safe.
func (t *CalendarListEventsTool) IsReadOnlyCall(string) bool { return true }

func (t *CalendarListEventsTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	if result, valid := agent.ValidateToolArguments(t.Info(), argsJSON); !valid {
		return result, nil
	}
	if t.Broker == nil {
		return agent.ToolResult{
			Content: "calendar_list_events: Desktop RPC broker not available",
			IsError: true,
		}, nil
	}
	var args calendarListEventsArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return invalidArgResult("calendar_list_events", "_", err.Error()), nil
	}
	if err := validateRFC3339(args.Start); err != nil {
		return invalidArgResult("calendar_list_events", "start", err.Error()), nil
	}
	if err := validateRFC3339(args.End); err != nil {
		return invalidArgResult("calendar_list_events", "end", err.Error()), nil
	}
	args.Limit = clampLimit(args.Limit)
	return callDesktopRPC(ctx, t.Broker, desktop_rpc.MethodCalendarListEvents, args, 0)
}
