package koe

import (
	"context"
	"strings"
	"testing"
)

// TestDispatchGetStatusDuringInFlight locks get_status to the async window: while
// the deferred-ack goroutine runs (SetInFlight set, ClearInFlight not yet), a
// "is it done?" must report running + the task; after ClearInFlight (cancel or
// completion) it reports idle. C-minimal wired this but never exercised it
// mid-flight (sync do_task had no window).
func TestDispatchGetStatusDuringInFlight(t *testing.T) {
	state := NewCallState("burst-x", "")
	disp := NewDispatcher(NewDaemonClient(""), NewAgentResolver(nil, NoopSemanticMatcher{}), state, nil)

	state.SetInFlight("remind me") // the goroutine sets this before DoTask
	out, err := disp.Dispatch(context.Background(), "get_status", []byte(`{}`))
	if err != nil {
		t.Fatalf("get_status: %v", err)
	}
	if !strings.Contains(string(out), "running") || !strings.Contains(string(out), "remind me") {
		t.Errorf("get_status should report the in-flight task as running; got %s", out)
	}

	state.ClearInFlight() // the goroutine clears this after DoTask returns
	out, _ = disp.Dispatch(context.Background(), "get_status", []byte(`{}`))
	if !strings.Contains(string(out), "idle") {
		t.Errorf("get_status should be idle after ClearInFlight; got %s", out)
	}
}
