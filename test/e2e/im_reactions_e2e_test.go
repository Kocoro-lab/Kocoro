//go:build e2e
// +build e2e

// Package e2e: IM message-reaction lifecycle end-to-end test.
//
// What this test exercises (and what it doesn't)
// ----------------------------------------------
// The real production path is:
//
//	Cloud webhook → MessagePayload{IMStatusContext} → daemon WS client
//	  → cmd/daemon.go (emit "received")
//	  → InjectMessage OR RunAgent → SessionCache routes
//	    → AgentLoop drains injectCh → emitDrainedLifecycle (per-message)
//	      → RunLifecycleEmitter.OnUserMessageProcessing → SendEvent("processing")
//	         + SessionCache.AppendDrainedInflight (bookkeeping)
//	    → per-route completion defer → EmitLifecycleOnRunCompletion
//	      → SendEvent("cleared") × (n-1) + SendEvent("done") × 1
//
// Standing up a real WS server + a fake Shannon Cloud + a live LLM tier from
// this repo is heavy (postgres, redis, gateway, image of cloud). Per the
// task spec ("lightweight composition test pattern" fallback) this test
// drives the lifecycle pipeline in its REAL shape — same types, same
// interfaces, same ordering — and only mocks the OUTER edges:
//
//   - The WS sink (LifecycleEventSender) is a recording fake so we can
//     assert the wire-level events without running an actual WS server.
//   - The agent loop's LLM call is bypassed — we drive
//     RunLifecycleEmitter.OnUserMessageProcessing directly with the same
//     arguments the loop's emitDrainedLifecycle / emitFirstTurnLifecycle
//     would pass. That call boundary IS where the agent package hands off
//     to the daemon, so exercising it directly here is byte-equivalent to
//     a loop-driven invocation.
//   - The "received" emit lives in cmd/daemon.go::emitLifecycleReceived
//     (unexported helper inside the cmd package). Its full source is one
//     SendEvent call; we replicate that call inline below and assert the
//     same wire shape. The cmd helper has dedicated unit tests in
//     cmd/daemon_lifecycle_test.go.
//
// Everything else — SessionCache, RunLifecycleEmitter, drained-inflight
// slice, EmitLifecycleOnRunCompletion, event ordering — is the real
// production code path.
//
// Build tag: this file is behind `e2e` so it never runs under
// `go test ./...`. Invoke explicitly:
//
//	go test -tags=e2e ./test/e2e/ -run TestIMReactionLifecycleE2E -v
package e2e

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/daemon"
)

// recordedWSEvent captures one Client.SendEvent call so the test can
// assert on the wire-level shape the daemon would push to Cloud.
type recordedWSEvent struct {
	MessageID string
	EventType string
	Message   string
	Data      map[string]interface{}
}

// lifecycleSink is a recording LifecycleEventSender — the same interface
// the real *daemon.Client satisfies. Plugged into RunLifecycleEmitter and
// EmitLifecycleOnRunCompletion in place of a real WS connection.
type lifecycleSink struct {
	mu     sync.Mutex
	events []recordedWSEvent
}

func newLifecycleSink() *lifecycleSink { return &lifecycleSink{} }

func (s *lifecycleSink) SendEvent(messageID, eventType, message string, data map[string]interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Copy the data map so later mutation by the caller (none expected, but
	// be defensive) cannot retroactively change recorded events.
	cp := make(map[string]interface{}, len(data))
	for k, v := range data {
		cp[k] = v
	}
	s.events = append(s.events, recordedWSEvent{
		MessageID: messageID,
		EventType: eventType,
		Message:   message,
		Data:      cp,
	})
	return nil
}

func (s *lifecycleSink) snapshot() []recordedWSEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]recordedWSEvent, len(s.events))
	copy(out, s.events)
	return out
}

func formatEvents(events []recordedWSEvent) string {
	b, _ := json.MarshalIndent(events, "", "  ")
	return string(b)
}

// stateOf reads the "state" field from a recorded event's data map. Returns
// "" if the field is missing or wrong-typed (those would be assertion
// failures elsewhere; here we just want a debuggable string).
func stateOf(e recordedWSEvent) string {
	s, _ := e.Data["state"].(string)
	return s
}

// rawContextOf reads "im_status_context" from a recorded event's data map.
// The daemon stores it as json.RawMessage; tolerate []byte for defensive
// comparison.
func rawContextOf(t *testing.T, e recordedWSEvent) []byte {
	t.Helper()
	switch v := e.Data["im_status_context"].(type) {
	case json.RawMessage:
		return []byte(v)
	case []byte:
		return v
	case nil:
		return nil
	default:
		t.Fatalf("im_status_context wrong type %T", v)
		return nil
	}
}

// emitLifecycleReceivedShim replicates the body of
// cmd/daemon.go::emitLifecycleReceived. That helper is unexported and lives
// in package cmd (binary entry point); reproducing the one-line call here
// keeps the e2e test in package e2e while still asserting on the same wire
// shape Cloud would observe. cmd/daemon_lifecycle_test.go covers the helper
// itself in isolation.
func emitLifecycleReceivedShim(sink *lifecycleSink, messageID string, ctx json.RawMessage) {
	if sink == nil || messageID == "" || len(ctx) == 0 {
		return
	}
	_ = sink.SendEvent(messageID, daemon.EventTypeMessageLifecycle, "", map[string]interface{}{
		"state":             daemon.LifecycleReceived,
		"im_status_context": ctx,
	})
}

// TestIMReactionLifecycleE2E exercises the full IM-message-reaction
// lifecycle pipeline for a representative Slack thread that receives a
// primary user message followed by two mid-run follow-ups. It asserts the
// wire-level event sequence the daemon would push to Cloud:
//
//	1. received   (primary,     on inbound at cmd/daemon.go)
//	2. processing (primary,     on first-turn entry in agent loop)
//	3. received   (follow-up 1, on inbound at cmd/daemon.go InjectOK path)
//	4. processing (follow-up 1, on drain in agent loop)
//	5. received   (follow-up 2, on inbound at cmd/daemon.go InjectOK path)
//	6. processing (follow-up 2, on drain in agent loop)
//	7. cleared    (primary,     on run completion — non-tail)
//	8. cleared    (follow-up 1, on run completion — non-tail)
//	9. done       (follow-up 2, on run completion — tail of drained slice)
//
// Note: every OnUserMessageProcessing call (primary first-turn AND each
// drained follow-up) goes through RunLifecycleEmitter, which both emits
// the "processing" wire event AND appends to the route's
// drained-inflight slice. At run completion EmitLifecycleOnRunCompletion
// drains the slice and emits "done" for the tail (the last message the
// agent worked on) and "cleared" for everything earlier. Cloud then
// renders that as: tail message gets the success reaction, earlier
// messages have their pending reactions removed.
//
// See `docs/superpowers/specs/2026-05-19-im-message-reaction-lifecycle-design.md`
// for the spec; see `internal/daemon/lifecycle.go` for the production code
// being exercised.
func TestIMReactionLifecycleE2E(t *testing.T) {
	sink := newLifecycleSink()

	// Real SessionCache — same type used by the production daemon.
	cache := daemon.NewSessionCache(t.TempDir())

	routeKey := "default:slack:T1:U1"
	// Register the route so AppendDrainedInflight isn't a no-op
	// (SessionCache silently drops appends to unknown routes — see
	// internal/daemon/lifecycle_test.go::TestSessionCache_AppendDrainedInflight_NoOpOnMissingRoute).
	cache.LockRouteWithManager(routeKey, t.TempDir())
	cache.UnlockRoute(routeKey)

	// Per-message IMStatusContext blobs. Cloud builds these per-platform in
	// internal/channels/handler.go and stuffs them into MessagePayload; the
	// daemon never decodes them — it just echoes them back inside lifecycle
	// events. Using realistic Slack shape here for fidelity.
	primaryCtx := json.RawMessage(`{"platform":"slack","workspace_id":"Txxx","channel_id":"Cxxx","message_ts":"1.1"}`)
	followup1Ctx := json.RawMessage(`{"platform":"slack","workspace_id":"Txxx","channel_id":"Cxxx","message_ts":"1.2"}`)
	followup2Ctx := json.RawMessage(`{"platform":"slack","workspace_id":"Txxx","channel_id":"Cxxx","message_ts":"1.3"}`)

	// ── Step 1: Primary inbound → "received" emit.
	// In production this fires at cmd/daemon.go:419 just before RunAgent.
	emitLifecycleReceivedShim(sink, "envelope-primary", primaryCtx)

	// ── Step 2: Construct the per-run RunLifecycleEmitter (real production
	// type) and simulate the agent loop's first-turn emit. The production
	// driver is AgentLoop.emitFirstTurnLifecycle, which calls
	// emitter.OnUserMessageProcessing exactly once with the run's primary
	// CloudMessageID + IMStatusContext.
	emitter := daemon.NewRunLifecycleEmitter(sink, cache, routeKey)
	emitter.OnUserMessageProcessing("envelope-primary", primaryCtx)

	// ── Step 3: First follow-up inbound → "received" emit.
	// Production path: cmd/daemon.go:363 on InjectOK.
	emitLifecycleReceivedShim(sink, "envelope-followup-1", followup1Ctx)

	// ── Step 4: Agent loop drains follow-up 1 into an LLM turn → "processing"
	// emit + drained-inflight bookkeeping. Production path:
	// AgentLoop.emitDrainedLifecycle in internal/agent/loop.go.
	emitter.OnUserMessageProcessing("envelope-followup-1", followup1Ctx)

	// ── Step 5: Second follow-up inbound → "received" emit.
	emitLifecycleReceivedShim(sink, "envelope-followup-2", followup2Ctx)

	// ── Step 6: Agent loop drains follow-up 2 → "processing" emit + bookkeeping.
	emitter.OnUserMessageProcessing("envelope-followup-2", followup2Ctx)

	// ── Step 7: Run completes. Production path: per-route completion defer
	// at internal/daemon/runner.go:1072 calls EmitLifecycleOnRunCompletion
	// which TakeDrainedInflight, then emits "done" for the tail and
	// "cleared" for earlier entries.
	daemon.EmitLifecycleOnRunCompletion(sink, cache, routeKey)

	// ── Assertions: total event count + per-state ordering.

	all := sink.snapshot()

	// All events must carry EventTypeMessageLifecycle.
	for i, e := range all {
		if e.EventType != daemon.EventTypeMessageLifecycle {
			t.Errorf("event %d: type %q, want %q", i, e.EventType, daemon.EventTypeMessageLifecycle)
		}
	}

	// Expected sequence in wire order.
	//
	// All three OnUserMessageProcessing calls hit the production emitter
	// (lifecycle.go::RunLifecycleEmitter.OnUserMessageProcessing), which
	// both fires the "processing" wire event AND appends a
	// DrainedInflightEntry to the route's slice. So at completion the
	// route's drained-inflight slice is [primary, f1, f2]: the tail (f2)
	// emits "done" and the earlier two emit "cleared".
	expected := []struct {
		messageID string
		state     string
		ctx       json.RawMessage
	}{
		{"envelope-primary", daemon.LifecycleReceived, primaryCtx},
		{"envelope-primary", daemon.LifecycleProcessing, primaryCtx},
		{"envelope-followup-1", daemon.LifecycleReceived, followup1Ctx},
		{"envelope-followup-1", daemon.LifecycleProcessing, followup1Ctx},
		{"envelope-followup-2", daemon.LifecycleReceived, followup2Ctx},
		{"envelope-followup-2", daemon.LifecycleProcessing, followup2Ctx},
		{"envelope-primary", daemon.LifecycleCleared, primaryCtx},
		{"envelope-followup-1", daemon.LifecycleCleared, followup1Ctx},
		{"envelope-followup-2", daemon.LifecycleDone, followup2Ctx},
	}

	if len(all) != len(expected) {
		t.Fatalf("event count mismatch: want %d got %d:\n%s",
			len(expected), len(all), formatEvents(all))
	}

	for i, want := range expected {
		got := all[i]
		if got.MessageID != want.messageID {
			t.Errorf("event %d message_id: got %q want %q", i, got.MessageID, want.messageID)
		}
		if state := stateOf(got); state != want.state {
			t.Errorf("event %d state: got %q want %q", i, state, want.state)
		}
		if rawCtx := rawContextOf(t, got); string(rawCtx) != string(want.ctx) {
			t.Errorf("event %d im_status_context: got %s want %s", i, rawCtx, want.ctx)
		}
	}
}

// TestIMReactionLifecycleE2E_NonIMRunIsSilent verifies the lifecycle
// pipeline is a strict no-op when IMStatusContext is empty — i.e. a TUI /
// CLI / webhook / scheduled run produces zero MESSAGE_LIFECYCLE events
// regardless of whether the pipeline is wired up. This is the
// downgrade-safety guarantee: Cloud only attaches IMStatusContext when the
// daemon advertises im_message_lifecycle_v1, and old daemons that don't
// advertise it must continue working unchanged.
func TestIMReactionLifecycleE2E_NonIMRunIsSilent(t *testing.T) {
	sink := newLifecycleSink()
	cache := daemon.NewSessionCache(t.TempDir())

	routeKey := "agent:explorer"
	cache.LockRouteWithManager(routeKey, t.TempDir())
	cache.UnlockRoute(routeKey)

	// Non-IM inbound: empty IMStatusContext. The cmd helper short-circuits
	// — emitLifecycleReceivedShim mirrors that guard.
	emitLifecycleReceivedShim(sink, "tui-run-1", nil)
	emitLifecycleReceivedShim(sink, "", json.RawMessage(`{"platform":"slack"}`))

	// Real per-run emitter. Both args empty: should short-circuit silently
	// and not push anything to the route's drained-inflight slice either.
	emitter := daemon.NewRunLifecycleEmitter(sink, cache, routeKey)
	emitter.OnUserMessageProcessing("", nil)
	emitter.OnUserMessageProcessing("tui-run-1", nil)
	emitter.OnUserMessageProcessing("", json.RawMessage(`{"platform":"slack"}`))

	// Completion sweep on an empty route: no events.
	daemon.EmitLifecycleOnRunCompletion(sink, cache, routeKey)

	if got := sink.snapshot(); len(got) != 0 {
		t.Fatalf("non-IM run should emit zero lifecycle events, got %d:\n%s",
			len(got), formatEvents(got))
	}
}

// TestIMReactionLifecycleE2E_SingleMessageRunEmitsDone covers the common
// case of a single user message with no follow-ups — the run-completion
// sweep should emit exactly one "done" (for the tail) with no preceding
// "cleared".
func TestIMReactionLifecycleE2E_SingleMessageRunEmitsDone(t *testing.T) {
	sink := newLifecycleSink()
	cache := daemon.NewSessionCache(t.TempDir())

	routeKey := "default:slack:T1:U1"
	cache.LockRouteWithManager(routeKey, t.TempDir())
	cache.UnlockRoute(routeKey)

	ctx := json.RawMessage(`{"platform":"slack","message_ts":"1.0"}`)

	emitLifecycleReceivedShim(sink, "envelope-1", ctx)

	emitter := daemon.NewRunLifecycleEmitter(sink, cache, routeKey)
	emitter.OnUserMessageProcessing("envelope-1", ctx)

	daemon.EmitLifecycleOnRunCompletion(sink, cache, routeKey)

	all := sink.snapshot()
	if len(all) != 3 {
		t.Fatalf("single-message run: want 3 events, got %d:\n%s",
			len(all), formatEvents(all))
	}
	if stateOf(all[0]) != daemon.LifecycleReceived {
		t.Errorf("event 0: want received, got %q", stateOf(all[0]))
	}
	if stateOf(all[1]) != daemon.LifecycleProcessing {
		t.Errorf("event 1: want processing, got %q", stateOf(all[1]))
	}
	if stateOf(all[2]) != daemon.LifecycleDone {
		t.Errorf("event 2: want done, got %q", stateOf(all[2]))
	}
	for i, e := range all {
		if e.MessageID != "envelope-1" {
			t.Errorf("event %d message_id: got %q want envelope-1", i, e.MessageID)
		}
	}
}
