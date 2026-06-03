package tools

import (
	"context"
	"encoding/json"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/daemon/desktop_rpc"
)

// CalendarGetEventTool wraps calendar.get_event — fetches a single event by
// id with full detail (recurrence_rule + alarms). Read-only, no approval.
type CalendarGetEventTool struct {
	Broker *desktop_rpc.DesktopRPCBroker
}

type calendarGetEventArgs struct {
	ID string `json:"id"`
}

func (t *CalendarGetEventTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "calendar_get_event",
		Description: "Fetch full detail of a single calendar event by id. Returns all fields from calendar_list_events plus: " +
			"recurrence_rule (frequency / interval / by_day / end_date / occurrence_count / raw_rrule RFC 5545 string), and alarms list. " +
			"Use after calendar_list_events when you need recurrence info or detailed metadata.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{
					"type":        "string",
					"description": "EKEvent.eventIdentifier from calendar_list_events",
				},
			},
		},
		Required: []string{"id"},
	}
}

func (t *CalendarGetEventTool) RequiresApproval() bool { return false }

func (t *CalendarGetEventTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	if t.Broker == nil {
		return agent.ToolResult{
			Content: "calendar_get_event: Desktop RPC broker not available",
			IsError: true,
		}, nil
	}
	var args calendarGetEventArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return invalidArgResult("calendar_get_event", "_", err.Error()), nil
	}
	if args.ID == "" {
		return agent.ValidationError("calendar_get_event: missing required `id` parameter"), nil
	}
	return callDesktopRPC(ctx, t.Broker, desktop_rpc.MethodCalendarGetEvent, args, 0)
}
