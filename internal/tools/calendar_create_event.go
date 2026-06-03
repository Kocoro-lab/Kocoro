package tools

import (
	"context"
	"encoding/json"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/daemon/desktop_rpc"
)

// CalendarCreateEventTool wraps calendar.create_event. Approval-required: the
// model passes a user-facing `description` for the approval card; everything
// else is the event payload. `description` is stripped from the RPC params
// before sending (spec §5.2 schema doesn't include it).
type CalendarCreateEventTool struct {
	Broker *desktop_rpc.DesktopRPCBroker
}

func (t *CalendarCreateEventTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "calendar_create_event",
		Description: "Create a new calendar event. Times are RFC 3339 with offset; for all_day events, end is the inclusive end-of-day (e.g. 23:59:59). " +
			"calendar_id null = user's default calendar. attendees are written as metadata only — v1 does NOT send invitations (Google/Exchange limitation, see §3.3). " +
			"result.invitations_sent is always false in v1; tell the user 'event created but invitations need to be sent manually'." +
			agent.DescriptionGuidance,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"calendar_id":      map[string]any{"type": "string", "description": "Target calendar (null = default)"},
				"title":            map[string]any{"type": "string", "description": "Event title"},
				"start":            map[string]any{"type": "string", "description": "Start time, RFC 3339 with offset"},
				"end":              map[string]any{"type": "string", "description": "End time, RFC 3339 with offset"},
				"all_day":          map[string]any{"type": "boolean", "description": "All-day event (end is inclusive end-of-day)"},
				"location":         map[string]any{"type": "string", "description": "Location"},
				"notes":            map[string]any{"type": "string", "description": "Notes / description"},
				"url":              map[string]any{"type": "string", "description": "Associated URL (e.g. Zoom/Teams join link)"},
				"attendees":        map[string]any{"type": "array", "items": map[string]any{"type": "object"}, "description": "Attendees list (email + name) — metadata only, invitations not sent in v1"},
				"alarms":           map[string]any{"type": "array", "items": map[string]any{"type": "object"}, "description": "Alarms ({minutes_before: N})"},
				"recurrence_rule":  map[string]any{"type": "object", "description": "Recurrence (frequency / interval / by_day / end_date / occurrence_count / raw_rrule)"},
				"description":      agent.DescriptionFieldSpec,
			},
		},
		Required: []string{"title", "start", "end", "description"},
	}
}

func (t *CalendarCreateEventTool) RequiresApproval() bool { return true }

func (t *CalendarCreateEventTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	if t.Broker == nil {
		return agent.ToolResult{
			Content: "calendar_create_event: Desktop RPC broker not available",
			IsError: true,
		}, nil
	}
	// Lightweight parse just to validate required fields. We forward the
	// rest of the JSON body to Desktop as-is (after stripping description).
	var args struct {
		Title       string `json:"title"`
		Start       string `json:"start"`
		End         string `json:"end"`
		Description string `json:"description"`
		AllDay      bool   `json:"all_day"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return invalidArgResult("calendar_create_event", "_", err.Error()), nil
	}
	if args.Title == "" {
		return agent.ValidationError("calendar_create_event: missing required `title` parameter"), nil
	}
	if args.Start == "" {
		return agent.ValidationError("calendar_create_event: missing required `start` parameter"), nil
	}
	if args.End == "" {
		return agent.ValidationError("calendar_create_event: missing required `end` parameter"), nil
	}
	if args.Description == "" {
		return agent.ValidationError("calendar_create_event: missing required `description` parameter"), nil
	}
	if err := validateRFC3339(args.Start); err != nil {
		return invalidArgResult("calendar_create_event", "start", err.Error()), nil
	}
	if err := validateRFC3339(args.End); err != nil {
		return invalidArgResult("calendar_create_event", "end", err.Error()), nil
	}
	// Strip description before forwarding (spec §5.2 schema doesn't include it).
	rpcParams := stripDescription([]byte(argsJSON))
	return callDesktopRPCRaw(ctx, t.Broker, desktop_rpc.MethodCalendarCreateEvent, rpcParams, 0)
}
