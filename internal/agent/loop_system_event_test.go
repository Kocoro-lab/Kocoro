package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestDrainAndFormatSystemEvents(t *testing.T) {
	a := &AgentLoop{}

	t.Run("nil drain fn yields empty", func(t *testing.T) {
		if got := a.drainAndFormatSystemEvents(); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("drain fn output is formatted", func(t *testing.T) {
		ts := time.Date(2026, 6, 5, 9, 0, 0, 0, time.UTC)
		a.SetSystemEventDrainFn(func() []SystemEvent {
			return []SystemEvent{{Text: "kicked from #ops", Trusted: false, TS: ts}}
		})
		got := a.drainAndFormatSystemEvents()
		if !strings.Contains(got, "System (untrusted): [09:00:00] kicked from #ops") {
			t.Fatalf("missing formatted line: %q", got)
		}
		if !strings.HasPrefix(got, "<system-reminder>") {
			t.Fatalf("missing wrapper: %q", got)
		}
	})
}

// TestSystemEvents_RequeuedOnTerminalFailure locks the fix for the lost-event
// bug: the destructive drain happens before the first LLM call, so when the
// turn fails terminally BEFORE any successful LLM response (e.g. the network
// outage that also caused the delivery failure), the drained events must be
// re-enqueued — otherwise the only carrier of "reply FAILED / bot kicked" is
// gone forever and the model keeps replying into a dead channel.
func TestSystemEvents_RequeuedOnTerminalFailure(t *testing.T) {
	ts := time.Date(2026, 6, 5, 9, 0, 0, 0, time.UTC)

	// 400 → non-retryable APIError → Run fails terminally on the first attempt,
	// before any successful LLM response.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad request"}`))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	loop := NewAgentLoop(gw, reg, "medium", "", 25, 2000, 200, nil, nil, nil)

	drained := []SystemEvent{{Text: "reply FAILED: bot was kicked", Trusted: false, TS: ts}}
	loop.SetSystemEventDrainFn(func() []SystemEvent { return drained })
	var requeued []SystemEvent
	loop.SetSystemEventRequeueFn(func(evs []SystemEvent) { requeued = append(requeued, evs...) })

	if _, _, err := loop.Run(context.Background(), "hi", nil, nil); err == nil {
		t.Fatal("expected terminal error from 400 response")
	}

	if len(requeued) != 1 || requeued[0].Text != "reply FAILED: bot was kicked" {
		t.Fatalf("drained events not re-enqueued on terminal failure: got %+v", requeued)
	}
}

// TestSystemEvents_NotRequeuedOnSuccess is the other half: once the model has
// seen the scaffold (a successful LLM response), the events were delivered and
// must NOT be re-enqueued (they are intentionally stripped from history) — a
// re-enqueue would double-show the notice next turn.
func TestSystemEvents_NotRequeuedOnSuccess(t *testing.T) {
	ts := time.Date(2026, 6, 5, 9, 0, 0, 0, time.UTC)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(nativeResponse("ok", "end_turn", nil, 10, 5))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	loop := NewAgentLoop(gw, reg, "medium", "", 25, 2000, 200, nil, nil, nil)

	loop.SetSystemEventDrainFn(func() []SystemEvent {
		return []SystemEvent{{Text: "reply FAILED: bot was kicked", Trusted: false, TS: ts}}
	})
	var requeued []SystemEvent
	loop.SetSystemEventRequeueFn(func(evs []SystemEvent) { requeued = append(requeued, evs...) })

	if _, _, err := loop.Run(context.Background(), "hi", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(requeued) != 0 {
		t.Fatalf("events re-enqueued after the model saw them: %+v", requeued)
	}
}

// TestSystemEvents_NotRequeuedAfterPartialStream covers the partial-stream case:
// if the model streams a delta (so it HAS received the scaffold incl. the
// drained events) and the turn THEN fails terminally on a stream-idle timeout,
// the events must NOT be re-enqueued — re-enqueueing would double-show the
// notice next turn. The "saw scaffold" flag must flip on the first delta, not
// only on a fully successful response.
func TestSystemEvents_NotRequeuedAfterPartialStream(t *testing.T) {
	ts := time.Date(2026, 6, 5, 9, 0, 0, 0, time.UTC)

	// SSE server: emit one content delta, then stall until the client aborts —
	// the stream-idle watchdog fires AFTER the model has received the scaffold.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("response writer is not a flusher")
			return
		}
		fl.Flush()
		fmt.Fprintf(w, "data: %s\n\n", `{"type":"content_delta","text":"working on it"}`)
		fl.Flush()
		<-r.Context().Done() // stall → stream-idle watchdog aborts the turn
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	gw.SetStreamIdleTimeout(200 * time.Millisecond)
	reg := NewToolRegistry()
	loop := NewAgentLoop(gw, reg, "medium", "", 25, 2000, 200, nil, nil, nil)
	loop.SetEnableStreaming(true)
	loop.SetHandler(&mockHandler{})

	loop.SetSystemEventDrainFn(func() []SystemEvent {
		return []SystemEvent{{Text: "reply FAILED: bot was kicked", Trusted: false, TS: ts}}
	})
	var requeued []SystemEvent
	loop.SetSystemEventRequeueFn(func(evs []SystemEvent) { requeued = append(requeued, evs...) })

	if _, _, err := loop.Run(context.Background(), "hi", nil, nil); err == nil {
		t.Fatal("expected terminal stream-idle error")
	}
	if len(requeued) != 0 {
		t.Fatalf("events re-enqueued after the model saw the scaffold via a stream delta: %+v", requeued)
	}
}

func TestAppendDynamicUserBlocks_SystemEventsOrder(t *testing.T) {
	out := appendDynamicUserBlocks(
		"USER_PAYLOAD",
		"<system-reminder>\nSystem: [00:00:00] x\n</system-reminder>",
		"SKILL_LISTING",
		"LANG_DIRECTIVE",
	)
	idxUser := strings.Index(out, "USER_PAYLOAD")
	idxSys := strings.Index(out, "system-reminder")
	idxSkill := strings.Index(out, "SKILL_LISTING")
	idxLang := strings.Index(out, "LANG_DIRECTIVE")
	if !(idxUser < idxSys && idxSys < idxSkill && idxSkill < idxLang) {
		t.Fatalf("bad order: user=%d sys=%d skill=%d lang=%d in %q", idxUser, idxSys, idxSkill, idxLang, out)
	}
}

func TestAppendDynamicUserBlocks_EmptySystemEvents(t *testing.T) {
	out := appendDynamicUserBlocks("U", "", "S", "L")
	if strings.Contains(out, "system-reminder") {
		t.Fatalf("empty system-events should add nothing: %q", out)
	}
}

// TestSystemEvents_NotPersisted is the definition-of-done gate: an S0 system
// event drained onto the scaffolded first user turn must NOT survive into the
// persisted run messages. The existing first-turn scaffold strip
// (captureRunMessages restoring rawUserMessage, see loop.go ~L2265) already
// removes ALL scaffold framing on the first user message, so the injected
// `System:` line and its `<system-reminder>` wrapper disappear with no
// S0-specific strip code.
func TestSystemEvents_NotPersisted(t *testing.T) {
	ts := time.Date(2026, 6, 5, 9, 0, 0, 0, time.UTC)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(nativeResponse("Sunny.", "end_turn", nil, 10, 5))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	loop := NewAgentLoop(gw, reg, "medium", "", 25, 2000, 200, nil, nil, nil)
	loop.SetSystemEventDrainFn(func() []SystemEvent {
		return []SystemEvent{{Text: "reply FAILED: bot was kicked", Trusted: false, TS: ts}}
	})

	if _, _, err := loop.Run(context.Background(), "what is the weather?", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	msgs := loop.SanitizedRunMessages()
	for _, m := range msgs {
		txt := m.Content.Text()
		if strings.Contains(txt, "reply FAILED: bot was kicked") {
			t.Fatalf("S0 line leaked into persisted run messages: %q", txt)
		}
		if strings.Contains(txt, "system-reminder") {
			t.Fatalf("system-reminder wrapper leaked into persisted run messages: %q", txt)
		}
	}
	if len(msgs) == 0 {
		t.Fatalf("expected at least the user message to be persisted, got none")
	}
	if msgs[0].Content.Text() != "what is the weather?" {
		t.Fatalf("first user message should be raw user text, got %q", msgs[0].Content.Text())
	}
}
