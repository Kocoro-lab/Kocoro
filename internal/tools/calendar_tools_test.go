package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/daemon/desktop_rpc"
)

// brokerFake wires a DesktopRPCBroker to a controllable handler so we can
// drive each tool through real broker.Request paths without spinning up a
// listener / sock. The handler decides per-call what to return: success
// (echo params back), structured RPC error, or transport error.
func brokerFake(t *testing.T, handler func(method string, params json.RawMessage) (*desktop_rpc.RPCResult, error)) *desktop_rpc.DesktopRPCBroker {
	t.Helper()
	b := desktop_rpc.NewDesktopRPCBroker()
	b.SetSendFn(func(req *desktop_rpc.RPCRequest) error {
		// Simulate Desktop async response by spawning a goroutine.
		go func() {
			res, err := handler(req.Method, req.Params)
			if err != nil {
				// Transport error path: leave the result hanging (broker times out).
				return
			}
			res.RequestID = req.RequestID
			b.Resolve(req.RequestID, res)
		}()
		return nil
	})
	return b
}

func successResult(t *testing.T, body any) *desktop_rpc.RPCResult {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal test body: %v", err)
	}
	return &desktop_rpc.RPCResult{OK: true, Result: raw}
}

func errorResult(code, message string) *desktop_rpc.RPCResult {
	return &desktop_rpc.RPCResult{
		OK: false,
		Error: &desktop_rpc.RPCError{
			Code:    code,
			Message: message,
		},
	}
}

// ---------------- calendar_check_permission ----------------

func TestCalendarCheckPermission_NoBroker(t *testing.T) {
	t.Parallel()
	tool := &CalendarCheckPermissionTool{Broker: nil}
	res, err := tool.Run(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.IsError {
		t.Error("nil broker should return IsError=true")
	}
}

func TestCalendarCheckPermission_HappyPath(t *testing.T) {
	t.Parallel()
	tool := &CalendarCheckPermissionTool{
		Broker: brokerFake(t, func(method string, _ json.RawMessage) (*desktop_rpc.RPCResult, error) {
			if method != desktop_rpc.MethodCalendarCheckPermission {
				t.Errorf("wrong method: %q", method)
			}
			return successResult(t, map[string]string{"status": "granted"}), nil
		}),
	}
	res, err := tool.Run(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "granted") {
		t.Errorf("result should include status: %s", res.Content)
	}
}

// ---------------- calendar_request_permission ----------------

func TestCalendarRequestPermission_MissingDescription(t *testing.T) {
	t.Parallel()
	tool := &CalendarRequestPermissionTool{Broker: brokerFake(t, nil)}
	res, _ := tool.Run(context.Background(), `{}`)
	if !res.IsError || !strings.Contains(res.Content, "description") {
		t.Errorf("expected ValidationError on missing description, got: %s", res.Content)
	}
}

func TestCalendarRequestPermission_HasExtendedTimeout(t *testing.T) {
	t.Parallel()
	// Verify that calendar_request_permission uses the 5-min timeout, not default.
	// We assert this by checking the broker received the right TimeoutMs.
	var capturedTimeout int
	b := desktop_rpc.NewDesktopRPCBroker()
	b.SetSendFn(func(req *desktop_rpc.RPCRequest) error {
		capturedTimeout = req.TimeoutMs
		go b.Resolve(req.RequestID, successResult(t, map[string]string{"status": "granted"}))
		return nil
	})
	tool := &CalendarRequestPermissionTool{Broker: b}
	res, _ := tool.Run(context.Background(), `{"description":"ask user for cal access"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if capturedTimeout != requestPermissionTimeoutMs {
		t.Errorf("timeout: got %dms, want %dms (5 min, spec §5.2)", capturedTimeout, requestPermissionTimeoutMs)
	}
}

// ---------------- calendar_list_events ----------------

func TestCalendarListEvents_MissingStart(t *testing.T) {
	t.Parallel()
	tool := &CalendarListEventsTool{Broker: brokerFake(t, nil)}
	res, _ := tool.Run(context.Background(), `{"end":"2026-05-26T23:59:59+08:00"}`)
	if !res.IsError || !strings.Contains(res.Content, "start") {
		t.Errorf("expected start validation error, got: %s", res.Content)
	}
}

func TestCalendarListEvents_InvalidTimeFormat(t *testing.T) {
	t.Parallel()
	tool := &CalendarListEventsTool{Broker: brokerFake(t, nil)}
	// Naked datetime without offset is forbidden per spec §3.3.
	res, _ := tool.Run(context.Background(), `{"start":"2026-05-26T09:00:00","end":"2026-05-26T23:59:59+08:00"}`)
	if !res.IsError || !strings.Contains(res.Content, "start") {
		t.Errorf("expected start RFC3339 validation error, got: %s", res.Content)
	}
}

func TestCalendarListEvents_LimitClamp(t *testing.T) {
	t.Parallel()
	var capturedLimit int
	tool := &CalendarListEventsTool{
		Broker: brokerFake(t, func(_ string, params json.RawMessage) (*desktop_rpc.RPCResult, error) {
			var p struct {
				Limit int `json:"limit"`
			}
			_ = json.Unmarshal(params, &p)
			capturedLimit = p.Limit
			return successResult(t, map[string]any{"events": []any{}, "truncated": false}), nil
		}),
	}
	res, _ := tool.Run(context.Background(), `{"start":"2026-05-26T00:00:00+08:00","end":"2026-05-26T23:59:59+08:00","limit":99999}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if capturedLimit != 2000 {
		t.Errorf("limit not clamped: got %d, want 2000", capturedLimit)
	}
}

func TestCalendarListEvents_PermissionDeniedMapping(t *testing.T) {
	t.Parallel()
	tool := &CalendarListEventsTool{
		Broker: brokerFake(t, func(_ string, _ json.RawMessage) (*desktop_rpc.RPCResult, error) {
			return errorResult(desktop_rpc.ErrCodePermissionDenied, "TCC denied"), nil
		}),
	}
	res, _ := tool.Run(context.Background(), `{"start":"2026-05-26T00:00:00+08:00","end":"2026-05-26T23:59:59+08:00"}`)
	if !res.IsError {
		t.Fatal("expected error result")
	}
	if !strings.Contains(res.Content, "Kocoro Desktop") {
		t.Errorf("permission_denied message should reference Desktop UI; got: %s", res.Content)
	}
}

// ---------------- calendar_get_event ----------------

func TestCalendarGetEvent_NotFound(t *testing.T) {
	t.Parallel()
	tool := &CalendarGetEventTool{
		Broker: brokerFake(t, func(_ string, _ json.RawMessage) (*desktop_rpc.RPCResult, error) {
			return errorResult(desktop_rpc.ErrCodeNotFound, "event gone"), nil
		}),
	}
	res, _ := tool.Run(context.Background(), `{"id":"evt_abc"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
	if !strings.Contains(res.Content, "not found") {
		t.Errorf("not_found message check: %s", res.Content)
	}
}

// ---------------- calendar_create_event ----------------

func TestCalendarCreateEvent_MissingRequiredFields(t *testing.T) {
	t.Parallel()
	tool := &CalendarCreateEventTool{Broker: brokerFake(t, nil)}
	for _, tc := range []struct {
		name  string
		args  string
		field string
	}{
		{"no title", `{"start":"2026-05-26T00:00:00+08:00","end":"2026-05-26T23:59:59+08:00","description":"d"}`, "title"},
		{"no start", `{"title":"t","end":"2026-05-26T23:59:59+08:00","description":"d"}`, "start"},
		{"no end", `{"title":"t","start":"2026-05-26T00:00:00+08:00","description":"d"}`, "end"},
		{"no description", `{"title":"t","start":"2026-05-26T00:00:00+08:00","end":"2026-05-26T23:59:59+08:00"}`, "description"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, _ := tool.Run(context.Background(), tc.args)
			if !res.IsError || !strings.Contains(res.Content, tc.field) {
				t.Errorf("expected validation error mentioning %q, got: %s", tc.field, res.Content)
			}
		})
	}
}

func TestCalendarCreateEvent_StripDescription(t *testing.T) {
	t.Parallel()
	var capturedParams string
	tool := &CalendarCreateEventTool{
		Broker: brokerFake(t, func(_ string, params json.RawMessage) (*desktop_rpc.RPCResult, error) {
			capturedParams = string(params)
			return successResult(t, map[string]any{
				"id":                  "evt_new",
				"pending_remote_sync": true,
				"invitations_sent":    false,
			}), nil
		}),
	}
	args := `{"title":"t","start":"2026-05-26T00:00:00+08:00","end":"2026-05-26T23:59:59+08:00","description":"create test event"}`
	res, _ := tool.Run(context.Background(), args)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	// description must be stripped from RPC params sent to Desktop.
	if strings.Contains(capturedParams, "description") {
		t.Errorf("description should be stripped from RPC params, got: %s", capturedParams)
	}
	// title/start/end must remain.
	if !strings.Contains(capturedParams, "title") {
		t.Errorf("title missing from RPC params: %s", capturedParams)
	}
}

// ---------------- calendar_update_event ----------------

func TestCalendarUpdateEvent_RejectScopeAll(t *testing.T) {
	t.Parallel()
	tool := &CalendarUpdateEventTool{Broker: brokerFake(t, nil)}
	args := `{"id":"evt_1","scope":"all","description":"update test","patch":{"title":"new"}}`
	res, _ := tool.Run(context.Background(), args)
	if !res.IsError {
		t.Fatal("scope=all should be rejected for update_event (spec §5.5.8)")
	}
	if !strings.Contains(res.Content, "scope") {
		t.Errorf("error should mention scope: %s", res.Content)
	}
}

func TestCalendarUpdateEvent_AcceptValidScope(t *testing.T) {
	t.Parallel()
	for _, scope := range []string{"this", "this_and_future"} {
		t.Run(scope, func(t *testing.T) {
			tool := &CalendarUpdateEventTool{
				Broker: brokerFake(t, func(_ string, _ json.RawMessage) (*desktop_rpc.RPCResult, error) {
					return successResult(t, map[string]any{"id": "evt_1", "pending_remote_sync": true, "invitations_sent": false}), nil
				}),
			}
			args := `{"id":"evt_1","scope":"` + scope + `","description":"u","patch":{"title":"new"}}`
			res, _ := tool.Run(context.Background(), args)
			if res.IsError {
				t.Errorf("scope=%s: unexpected error: %s", scope, res.Content)
			}
		})
	}
}

// ---------------- calendar_delete_event ----------------

func TestCalendarDeleteEvent_AcceptAllScopes(t *testing.T) {
	t.Parallel()
	for _, scope := range []string{"this", "this_and_future", "all"} {
		t.Run(scope, func(t *testing.T) {
			tool := &CalendarDeleteEventTool{
				Broker: brokerFake(t, func(_ string, _ json.RawMessage) (*desktop_rpc.RPCResult, error) {
					return successResult(t, map[string]any{"ok": true, "pending_remote_sync": true}), nil
				}),
			}
			args := `{"id":"evt_1","scope":"` + scope + `","description":"d"}`
			res, _ := tool.Run(context.Background(), args)
			if res.IsError {
				t.Errorf("scope=%s: unexpected error: %s", scope, res.Content)
			}
		})
	}
}

func TestCalendarDeleteEvent_RejectBadScope(t *testing.T) {
	t.Parallel()
	tool := &CalendarDeleteEventTool{Broker: brokerFake(t, nil)}
	args := `{"id":"evt_1","scope":"forever","description":"bad scope"}`
	res, _ := tool.Run(context.Background(), args)
	if !res.IsError {
		t.Fatal("bogus scope should be rejected")
	}
}

// ---------------- RegisterCalendarTools ----------------

func TestRegisterCalendarTools_NoBrokerSkips(t *testing.T) {
	t.Parallel()
	reg := agent.NewToolRegistry()
	RegisterCalendarTools(reg, nil)
	// Confirm no calendar_* tools registered. Use the registry's All() method
	// if available; otherwise scan via Get.
	for _, name := range []string{
		"calendar_check_permission", "calendar_request_permission",
		"calendar_list_sources", "calendar_list_events", "calendar_get_event",
		"calendar_create_event", "calendar_update_event", "calendar_delete_event",
	} {
		if toolExists(reg, name) {
			t.Errorf("calendar tool %q registered despite nil broker", name)
		}
	}
}

func TestRegisterCalendarTools_WithBrokerRegistersAll(t *testing.T) {
	t.Parallel()
	reg := agent.NewToolRegistry()
	broker := desktop_rpc.NewDesktopRPCBroker()
	RegisterCalendarTools(reg, broker)
	for _, name := range []string{
		"calendar_check_permission", "calendar_request_permission",
		"calendar_list_sources", "calendar_list_events", "calendar_get_event",
		"calendar_create_event", "calendar_update_event", "calendar_delete_event",
	} {
		if !toolExists(reg, name) {
			t.Errorf("calendar tool %q not registered", name)
		}
	}
}

func toolExists(reg *agent.ToolRegistry, name string) bool {
	_, ok := reg.Get(name)
	return ok
}

// TestCalendarTools_SurviveExtractPostOverlays locks down a load-bearing
// invariant: calendar tools must end up in PostOverlays so they survive
// the registry rebuilds triggered by MCP supervisor health changes (see
// cmd/daemon.go ~line 282/301 deps.Registry swap path).
//
// Regression for the 2026-05-26 bug where calendar tools were registered
// AFTER ExtractPostOverlays ran, leaving them stranded in the discarded
// initial registry while deps.Registry pointed at a rebuilt one without
// them. Agent loop never saw the tools.
func TestCalendarTools_SurviveExtractPostOverlays(t *testing.T) {
	t.Parallel()
	// Simulate the cmd/daemon.go startup sequence: build baseline, register
	// calendar tools, extract overlays, rebuild — calendar tools must appear
	// in the rebuilt registry.
	baseline := agent.NewToolRegistry()
	// Baseline has no calendar tools (RegisterLocalTools doesn't add them
	// unconditionally — they go in via RegisterCalendarTools).
	full := agent.NewToolRegistry()
	// Copy baseline into full (mirrors how cmd/daemon.go works after
	// RegisterAllWithBaseline).
	for _, b := range baseline.All() {
		full.Register(b)
	}

	broker := desktop_rpc.NewDesktopRPCBroker()
	RegisterCalendarTools(full, broker)

	// Extract post overlays — calendar tools should land here because
	// they're in `full` but not in `baseline`, and aren't MCP/Server tools.
	overlays := ExtractPostOverlays(full, baseline)

	wantNames := map[string]bool{
		"calendar_check_permission":   false,
		"calendar_request_permission": false,
		"calendar_list_sources":       false,
		"calendar_list_events":        false,
		"calendar_get_event":          false,
		"calendar_create_event":       false,
		"calendar_update_event":       false,
		"calendar_delete_event":       false,
	}
	for _, o := range overlays {
		if _, expected := wantNames[o.Info().Name]; expected {
			wantNames[o.Info().Name] = true
		}
	}
	for name, found := range wantNames {
		if !found {
			t.Errorf("calendar tool %q did NOT survive ExtractPostOverlays — it would be lost on registry rebuild (see cmd/daemon.go fix 2026-05-26)", name)
		}
	}
}

// ---------------- helpers ----------------

func TestValidateRFC3339(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"with offset", "2026-05-26T09:00:00+08:00", false},
		{"with fractional", "2026-05-26T09:00:00.123+08:00", false},
		{"with Z", "2026-05-26T01:00:00Z", false},
		{"no zone", "2026-05-26T09:00:00", true},
		{"date only", "2026-05-26", true},
		{"empty", "", true},
		{"garbage", "not-a-date", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRFC3339(tc.input)
			if tc.wantErr && err == nil {
				t.Errorf("%q: expected err, got nil", tc.input)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("%q: expected ok, got %v", tc.input, err)
			}
		})
	}
}

func TestClampLimit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want int }{
		{0, 500}, {-1, 500}, {1, 1}, {500, 500}, {2000, 2000}, {2001, 2000}, {99999, 2000},
	} {
		if got := clampLimit(tc.in); got != tc.want {
			t.Errorf("clampLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestStripDescription(t *testing.T) {
	t.Parallel()
	in := []byte(`{"title":"t","description":"d","start":"x"}`)
	out := stripDescription(in)
	if strings.Contains(string(out), "description") {
		t.Errorf("description not stripped: %s", out)
	}
	// Round-trip preserves other fields.
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("stripped output invalid JSON: %v", err)
	}
	if m["title"] != "t" || m["start"] != "x" {
		t.Errorf("other fields lost: %+v", m)
	}
}
