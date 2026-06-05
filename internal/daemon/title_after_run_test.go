package daemon

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// fireTitleAfterRun must be a safe no-op when gated out (nil deps, nil mgr,
// or a turn count outside the trigger set) — it must not panic or spawn work.
func TestFireTitleAfterRun_GatingIsNoOp(t *testing.T) {
	fireTitleAfterRun(nil, nil, "", "", "", "", nil, 2)                         // nil deps
	fireTitleAfterRun(&ServerDeps{}, nil, "s1", "slack", "Wayland", "", nil, 1) // nil mgr + nil GW
	fireTitleAfterRun(&ServerDeps{}, nil, "s1", "slack", "Wayland", "", nil, 2) // turn not in {1,3}
}

// TestFireTitleAfterRun_EmitsSessionTitleUpdated exercises the success path the
// gating test deliberately skips: a real gateway + manager drive the async
// upgrade to completion, and the daemon must emit exactly one
// session_title_updated event carrying the DECORATED title.
//
// The payload-key asserts (raw "session_id" / "title", snake_case) are the
// cross-repo wire-contract guard — Desktop's session-list refresh binds to
// those exact field names, so a silent Go-side rename must fail here.
func TestFireTitleAfterRun_EmitsSessionTitleUpdated(t *testing.T) {
	gw := &fakeGatewayBackend{reply: "Fix The Bug"}
	ts := httptest.NewServer(gw.handler())
	defer ts.Close()

	mgr := session.NewManager(t.TempDir())
	defer mgr.Close()

	// A freshly created placeholder: auto-derived (TitleAuto=true) at turn 0,
	// so the turn-1 upgrade's straggler guard (TitleTurns 0 <= 1) lets it write.
	sess := mgr.NewSession()
	id := sess.ID
	sess.Title = "placeholder"
	sess.TitleAuto = true
	sess.TitleTurns = 0
	if err := mgr.Save(); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	bus := NewEventBus()
	deps := &ServerDeps{
		GW:       client.NewGatewayClient(ts.URL, "test-key"),
		EventBus: bus,
	}

	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("fix the bug")},
		{Role: "assistant", Content: client.NewTextContent("done")},
	}

	// turns=1 is a TitleTriggerTurns value, so the async upgrade fires.
	fireTitleAfterRun(deps, mgr, id, "slack", "Wayland", "", msgs, 1)

	// Wait on a real signal (the persisted upgrade) instead of sleeping: poll
	// the store until the placeholder has been replaced, bounded so a hung
	// goroutine fails the test rather than hanging the suite.
	const want = "Slack · Wayland · Fix The Bug"
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := mgr.Load(id)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if got.Title == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("title not upgraded within deadline; got %q, want %q", got.Title, want)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Exactly one session_title_updated event, with the decorated title and
	// snake_case payload keys.
	var titleEvents []json.RawMessage
	for _, evt := range bus.EventsSince(0) {
		if evt.Type == EventSessionTitleUpdated {
			titleEvents = append(titleEvents, evt.Payload)
		}
	}
	if len(titleEvents) != 1 {
		t.Fatalf("expected exactly 1 %s event, got %d", EventSessionTitleUpdated, len(titleEvents))
	}

	// Assert the raw JSON key NAMES (the wire contract), not just the values.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(titleEvents[0], &raw); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if _, ok := raw["session_id"]; !ok {
		t.Errorf("payload missing snake_case key \"session_id\": %s", titleEvents[0])
	}
	if _, ok := raw["title"]; !ok {
		t.Errorf("payload missing snake_case key \"title\": %s", titleEvents[0])
	}

	var payload struct {
		SessionID string `json:"session_id"`
		Title     string `json:"title"`
	}
	if err := json.Unmarshal(titleEvents[0], &payload); err != nil {
		t.Fatalf("unmarshal payload into struct: %v", err)
	}
	if payload.SessionID != id {
		t.Errorf("payload.session_id = %q, want %q", payload.SessionID, id)
	}
	if payload.Title != want {
		t.Errorf("payload.title = %q, want %q", payload.Title, want)
	}
}
