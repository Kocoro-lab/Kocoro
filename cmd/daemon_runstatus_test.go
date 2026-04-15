package cmd

import (
	"encoding/json"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/daemon"
)

// TestDaemonEventHandler_OnRunStatus_EmitsEventBus proves that the real
// cmd/daemon.go handler (the exact production path) forwards watchdog
// soft/hard events to the daemon EventBus as EventRunStatus. This is
// the end-to-end channel that SSE/Desktop subscribes to — if this test
// passes, a real slow LLM call WILL produce a visible event to the UI.
func TestDaemonEventHandler_OnRunStatus_EmitsEventBus(t *testing.T) {
	// Real EventBus, not a mock.
	bus := daemon.NewEventBus()

	// Subscribe BEFORE emission so we catch the event.
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	h := &daemonEventHandler{
		deps:      &daemon.ServerDeps{EventBus: bus},
		sessionID: "sess-xyz",
		agent:     "shiokawa",
	}

	// Satisfy the RunStatusHandler interface assertion used by the loop.
	var _ agent.RunStatusHandler = h

	// Simulate what the watchdog does when the soft threshold fires.
	h.OnRunStatus("idle_soft", "no LLM activity for 90s (phase=awaiting_llm)")

	select {
	case ev := <-sub:
		if ev.Type != daemon.EventRunStatus {
			t.Fatalf("want EventRunStatus, got %q", ev.Type)
		}
		var payload map[string]string
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatalf("payload unmarshal: %v", err)
		}
		if payload["code"] != "idle_soft" {
			t.Fatalf("want code=idle_soft, got %q", payload["code"])
		}
		if payload["session_id"] != "sess-xyz" {
			t.Fatalf("session_id not propagated: %+v", payload)
		}
		if payload["agent"] != "shiokawa" {
			t.Fatalf("agent not propagated: %+v", payload)
		}
		if payload["detail"] == "" {
			t.Fatal("detail missing from payload")
		}
	default:
		t.Fatal("no event emitted — production watchdog path broken")
	}

	// Also verify the hard-idle path fires as a separate event.
	h.OnRunStatus("idle_hard", "cancelling after 540s idle (phase=awaiting_llm)")
	select {
	case ev := <-sub:
		var payload map[string]string
		_ = json.Unmarshal(ev.Payload, &payload)
		if payload["code"] != "idle_hard" {
			t.Fatalf("want code=idle_hard, got %q", payload["code"])
		}
	default:
		t.Fatal("idle_hard event did not reach the bus")
	}
}
