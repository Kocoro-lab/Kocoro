package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/daemon/desktop_rpc"
)

// callDesktopRPC marshals params into JSON and issues an RPC via the broker,
// returning a ToolResult shaped for the agent loop. Centralized so every
// calendar tool's Run() has the same error-translation behavior.
//
// timeoutMs == 0 means "use broker default" (30s). Passes 5min for
// calendar_request_permission per spec §5.2 (TCC user-decision latency).
func callDesktopRPC(ctx context.Context, broker *desktop_rpc.DesktopRPCBroker, method string, params any, timeoutMs int) (agent.ToolResult, error) {
	rawParams, err := json.Marshal(params)
	if err != nil {
		return agent.ToolResult{
			Content: fmt.Sprintf("calendar: marshal params: %v", err),
			IsError: true,
		}, nil
	}
	return callDesktopRPCRaw(ctx, broker, method, rawParams, timeoutMs)
}

// callDesktopRPCRaw is the variant used by write tools (create/update/delete)
// which pass pre-built RPC params bytes (typically after stripDescription).
// Avoids a redundant marshal-unmarshal-marshal cycle.
func callDesktopRPCRaw(ctx context.Context, broker *desktop_rpc.DesktopRPCBroker, method string, rawParams json.RawMessage, timeoutMs int) (agent.ToolResult, error) {
	req := &desktop_rpc.RPCRequest{
		Method:    method,
		Params:    rawParams,
		TimeoutMs: timeoutMs,
		TS:        time.Now().UTC().Format(time.RFC3339),
	}
	res, err := broker.Request(ctx, req)
	if err != nil {
		if errors.Is(err, desktop_rpc.ErrNotConnected) {
			return agent.ToolResult{
				Content: "Calendar requires Kocoro Desktop to be running. Please open Kocoro Desktop and try again.",
				IsError: true,
			}, nil
		}
		return agent.ToolResult{
			Content: fmt.Sprintf("calendar: RPC error: %v", err),
			IsError: true,
		}, nil
	}
	if !res.OK {
		return mapRPCErrorToToolResult(res.Error), nil
	}
	return agent.ToolResult{Content: string(res.Result)}, nil
}

// mapRPCErrorToToolResult turns a spec §5.3 error code into a user-facing
// ToolResult. Each code gets a friendly message — the model sees this verbatim
// and relays to the user, so phrasing matters.
func mapRPCErrorToToolResult(e *desktop_rpc.RPCError) agent.ToolResult {
	if e == nil {
		return agent.ToolResult{
			Content: "calendar: unspecified RPC error",
			IsError: true,
		}
	}
	var msg string
	switch e.Code {
	case desktop_rpc.ErrCodePermissionDenied:
		// details.status distinguishes denied / restricted / write_only — spec §5.2
		status := extractDetailString(e.Details, "status")
		switch status {
		case desktop_rpc.PermissionWriteOnly:
			msg = "Calendar read access is denied (only write access is granted). The user needs to upgrade to full calendar access in Kocoro Desktop settings."
		case desktop_rpc.PermissionRestricted:
			msg = "Calendar access is restricted by system policy (parental controls or MDM). The user cannot grant access from this Mac."
		default:
			msg = "Calendar access was denied by the user. Tell them to open Kocoro Desktop → Settings → Permissions → Calendar to grant access."
		}
	case desktop_rpc.ErrCodePermissionNotDetermined:
		msg = "Calendar permission has not been requested yet. Either call calendar_request_permission, or ask the user to grant access in Kocoro Desktop settings."
	case desktop_rpc.ErrCodeNotFound:
		msg = "Calendar event or calendar source not found. The id may have been deleted or the event may belong to a different account."
	case desktop_rpc.ErrCodeInvalidArgument:
		field := extractDetailString(e.Details, "field")
		method := extractDetailString(e.Details, "method")
		switch {
		case method != "":
			msg = fmt.Sprintf("Calendar RPC: method %q is not supported by the current Kocoro Desktop. The user may need to update.", method)
		case field != "":
			msg = fmt.Sprintf("calendar: invalid argument in field %q: %s", field, e.Message)
		default:
			msg = fmt.Sprintf("calendar: invalid argument: %s", e.Message)
		}
	case desktop_rpc.ErrCodeReadOnlyCalendar:
		msg = "The target calendar is read-only (e.g., Birthdays calendar or a subscription calendar). Pick a writable calendar instead."
	case desktop_rpc.ErrCodeTimeout:
		msg = fmt.Sprintf("Calendar RPC timed out: %s. Kocoro Desktop may be slow or unresponsive.", e.Message)
	case desktop_rpc.ErrCodeDesktopDisconnected:
		msg = "Kocoro Desktop is not connected. Calendar tools require Desktop to be running."
	case desktop_rpc.ErrCodeInternal:
		msg = fmt.Sprintf("Kocoro Desktop reported an internal error: %s", e.Message)
	default:
		// Forward-compatible: any unknown code surfaces verbatim with the
		// raw message so we don't blackhole new error codes.
		msg = fmt.Sprintf("calendar: %s — %s", e.Code, e.Message)
	}
	return agent.ToolResult{Content: msg, IsError: true}
}

// extractDetailString pulls a string value from RPCError.Details JSON.
// Returns "" if missing, wrong type, or invalid JSON.
func extractDetailString(details json.RawMessage, key string) string {
	if len(details) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(details, &m); err != nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// validateRFC3339 enforces spec §3.3 — strict RFC 3339 with offset. Rejects
// naked datetime (no zone) and bare date (unless caller knows it's all_day).
// Returns the original string on success for easy chaining.
func validateRFC3339(s string) error {
	if s == "" {
		return fmt.Errorf("empty time string")
	}
	// time.Parse(time.RFC3339Nano) accepts both "Z" and "+offset" variants
	// plus optional fractional seconds (RFC3339Nano is a strict superset of
	// RFC3339, so a separate RFC3339 fallback would be dead code). It REJECTS
	// naked datetime without a timezone — exactly what spec §3.3 requires.
	if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
		return fmt.Errorf("not a valid RFC 3339 timestamp with offset: %q", s)
	}
	return nil
}

// clampLimit applies the spec §5.2 calendar.list_events limit bounds:
// default 500, max 2000, min 1. Negative or zero defaults to 500.
func clampLimit(limit int) int {
	switch {
	case limit <= 0:
		return 500
	case limit > 2000:
		return 2000
	default:
		return limit
	}
}

// stripDescription returns a copy of args (parsed from JSON) with the
// `description` field removed. Used by write tools to avoid leaking the
// approval-card description into the RPC params we send to Desktop
// (spec §5.2 schemas don't include `description`).
//
// argsJSON should be a JSON object; non-object inputs pass through.
func stripDescription(argsJSON []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(argsJSON, &m); err != nil {
		return argsJSON
	}
	if _, present := m["description"]; !present {
		return argsJSON
	}
	delete(m, "description")
	out, err := json.Marshal(m)
	if err != nil {
		return argsJSON
	}
	return out
}

// invalidArgResult is a small helper for tool-side parameter validation that
// returns a uniform ValidationError shape (per CLAUDE.md guidance).
func invalidArgResult(toolName, field, reason string) agent.ToolResult {
	return agent.ValidationError(
		fmt.Sprintf("%s: invalid `%s` argument: %s", toolName, field, reason),
	)
}
