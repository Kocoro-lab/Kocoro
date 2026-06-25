package koe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestToolDefsShape(t *testing.T) {
	defs := ToolDefs()
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
		if d.Type != "function" {
			t.Errorf("tool %q type = %q, want function", d.Name, d.Type)
		}
	}
	for _, want := range []string{"do_task", "cancel", "get_status", "control_app", "switch_agent"} {
		if !names[want] {
			t.Errorf("missing tool %q", want)
		}
	}
	if len(defs) != 5 {
		t.Errorf("got %d tools, want exactly 5", len(defs))
	}
}

func TestBurstRouteKey(t *testing.T) {
	// MUST equal the keys Plan A Task 3 pins daemon-side.
	if got := burstRouteKey("finance", "burst-123"); got != "agent:finance:koe:burst-123" {
		t.Errorf("burstRouteKey(finance,burst-123) = %q, want agent:finance:koe:burst-123", got)
	}
	if got := burstRouteKey("", "burst-123"); got != "default:koe:burst-123" {
		t.Errorf("burstRouteKey(\"\",burst-123) = %q, want default:koe:burst-123", got)
	}
}

func TestPrepareDoTaskUsesBoundAgent(t *testing.T) {
	state := NewCallState("burst-1", "finance")
	d := NewDispatcher(nil, NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	req, clarify, err := d.PrepareDoTask([]byte(`{"task":"check NVDA"}`))
	if err != nil || clarify != nil {
		t.Fatalf("PrepareDoTask err=%v clarify=%v", err, clarify)
	}
	if req.Agent != "finance" || req.ThreadID != "burst-1" || req.Text != "check NVDA" {
		t.Errorf("req = %+v", req)
	}
}

func TestPrepareDoTaskClarifyOnUnknownAgent(t *testing.T) {
	state := NewCallState("burst-1", "default")
	d := NewDispatcher(nil, NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	_, clarify, err := d.PrepareDoTask([]byte(`{"task":"x","agent":"nonexistent zzz"}`))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if clarify == nil || clarify.Status != "clarify" {
		t.Errorf("expected clarify SayResult, got %+v", clarify)
	}
}

func TestMapDoTaskOutcome(t *testing.T) {
	if got := MapDoTaskOutcome(DoTaskOutcome{Kind: OutcomeCompleted, Reply: "done"}, nil); got.Status != "ok" || got.Say != "done" {
		t.Errorf("completed → %+v", got)
	}
	// injected MUST carry an empty say so the front brain doesn't double-speak.
	if got := MapDoTaskOutcome(DoTaskOutcome{Kind: OutcomeInjected}, nil); got.Status != "injected" || got.Say != "" {
		t.Errorf("injected → %+v", got)
	}
	if got := MapDoTaskOutcome(DoTaskOutcome{Kind: OutcomeRejected, Reason: "cwd_conflict"}, nil); got.Status != "failed" {
		t.Errorf("rejected → %+v", got)
	}
	if got := MapDoTaskOutcome(DoTaskOutcome{}, fmt.Errorf("boom")); got.Status != "failed" {
		t.Errorf("transport error → %+v", got)
	}
}

func TestDispatchRejectsDoTask(t *testing.T) {
	d := NewDispatcher(nil, NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), NewCallState("b", "default"), nil)
	if _, err := d.Dispatch(context.Background(), "do_task", []byte(`{"task":"x"}`)); err == nil {
		t.Error("Dispatch(do_task) must error — do_task is async (PrepareDoTask + goroutine), not a fast tool")
	}
}

func TestDispatchCancelUsesBurstKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		json.NewDecoder(r.Body).Decode(&got)
		if got["route_key"] != "agent:finance:koe:burst-1" {
			t.Errorf("cancel route_key = %v, want agent:finance:koe:burst-1", got["route_key"])
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	state := NewCallState("burst-1", "finance")
	d := NewDispatcher(NewDaemonClient(srv.URL), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	if _, err := d.Dispatch(context.Background(), "cancel", []byte(`{"reason":"user_cancel"}`)); err != nil {
		t.Fatalf("cancel: %v", err)
	}
}

func TestDispatchSwitchAgentRebinds(t *testing.T) {
	state := NewCallState("burst-1", "default")
	d := NewDispatcher(nil, NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	if _, err := d.Dispatch(context.Background(), "switch_agent", []byte(`{"agent":"finance"}`)); err != nil {
		t.Fatalf("switch_agent: %v", err)
	}
	if got := state.BoundAgent(); got != "finance" {
		t.Errorf("bound agent = %q, want finance", got)
	}
}

func TestDispatchControlAppStub(t *testing.T) {
	var seen string
	state := NewCallState("burst-1", "default")
	d := NewDispatcher(nil, NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state,
		func(ctx context.Context, action string) error { seen = action; return nil })
	if _, err := d.Dispatch(context.Background(), "control_app", []byte(`{"action":"open_settings"}`)); err != nil {
		t.Fatalf("control_app: %v", err)
	}
	if seen != "open_settings" {
		t.Errorf("control_app action = %q, want open_settings", seen)
	}
}
