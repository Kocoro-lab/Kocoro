package daemon

import (
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// These tests cover the daemon-side fix for the IM-burst bug "the last follow-up
// never enters the loop" (and its sibling "the IM card spins forever").
//
// Root cause (reproduced before the fix): InjectMessage returned InjectOK after
// the agent loop had already peeked an empty injectCh at its end_turn branch and
// was returning. ClearRouteRunState then niled injectCh WITHOUT draining the
// buffered survivor, and — unlike the /queue mailbox path — a busy-state inject
// has no SQLite backing to replay. The message was orphaned while the caller had
// already told Cloud it was accepted. Desktop survives via its client-side .done
// re-queue; Slack's "client" is stateless Cloud, so the recovery lives here.
//
// Fix (方案 A): the end_turn guard's bare `len(injectCh) > 0` peek is replaced by
// DrainSurvivorsOrCloseInject — an atomic, retraction-filtering drain that either
// hands survivors back to the loop (window stays open, loop continues) or, when
// nothing survives, closes the inject window in the same sc.mu hold. Combined
// with InjectMessage now sending under sc.mu, a racing follow-up can no longer be
// orphaned: it is drained as a survivor or it observes the closed window and
// falls through to a fresh run.

// TestSessionCache_DrainSurvivorsOrCloseInject_ReturnsSurvivorsKeepsWindowOpen:
// an undelivered follow-up is reclaimed for the loop to continue processing, and
// the inject window stays OPEN so the loop can re-check after handling it.
func TestSessionCache_DrainSurvivorsOrCloseInject_ReturnsSurvivorsKeepsWindowOpen(t *testing.T) {
	dir := t.TempDir()
	sc := NewSessionCache(dir)

	const key = "default:slack:thread-burst"
	injectCh := make(chan agent.InjectedMessage, 10)
	sc.mu.Lock()
	sc.routes[key] = &routeEntry{injectCh: injectCh, done: make(chan struct{})}
	sc.mu.Unlock()

	last := agent.InjectedMessage{Text: "图片也看看", ClientMessageID: "local-last"}
	if got := sc.InjectMessage(key, last); got != InjectOK {
		t.Fatalf("InjectMessage = %v, want InjectOK", got)
	}

	survivors := sc.DrainSurvivorsOrCloseInject(key)
	if len(survivors) != 1 || survivors[0].ClientMessageID != "local-last" {
		t.Fatalf("survivor must be reclaimed for the loop to continue, got %+v", survivors)
	}
	if !sc.HasActiveRun(key) {
		t.Fatal("window must stay OPEN while survivors remain (loop will continue + re-check)")
	}
}

// TestSessionCache_DrainSurvivorsOrCloseInject_EmptyClosesWindow: the loop's
// final end_turn finds nothing buffered, so the window closes atomically and a
// follow-up arriving after teardown falls through to a fresh run.
func TestSessionCache_DrainSurvivorsOrCloseInject_EmptyClosesWindow(t *testing.T) {
	dir := t.TempDir()
	sc := NewSessionCache(dir)

	const key = "session:sess-empty"
	sc.mu.Lock()
	sc.routes[key] = &routeEntry{injectCh: make(chan agent.InjectedMessage, 10), done: make(chan struct{})}
	sc.mu.Unlock()

	survivors := sc.DrainSurvivorsOrCloseInject(key)
	if len(survivors) != 0 {
		t.Fatalf("expected no survivors on the clean path, got %+v", survivors)
	}
	if sc.HasActiveRun(key) {
		t.Fatal("window must be CLOSED when nothing survives")
	}
	if got := sc.InjectMessage(key, agent.InjectedMessage{Text: "late"}); got != InjectNoActiveRun {
		t.Fatalf("post-close InjectMessage = %v, want InjectNoActiveRun (caller starts a fresh run)", got)
	}
	// Idempotent: re-calling on a closed route is a safe no-op.
	if survivors := sc.DrainSurvivorsOrCloseInject(key); len(survivors) != 0 {
		t.Fatalf("second call must be a no-op, got %+v", survivors)
	}
}

// TestSessionCache_DrainSurvivorsOrCloseInject_AllRetractedClosesWindow: every
// buffered follow-up was retracted, so after filtering nothing survives and the
// window closes — the loop returns without re-issuing an LLM call for cancelled
// input (also fixes the duplicate-final-answer retract race).
func TestSessionCache_DrainSurvivorsOrCloseInject_AllRetractedClosesWindow(t *testing.T) {
	dir := t.TempDir()
	sc := NewSessionCache(dir)

	const key = "session:sess-allretract"
	sc.mu.Lock()
	sc.routes[key] = &routeEntry{injectCh: make(chan agent.InjectedMessage, 10), done: make(chan struct{})}
	sc.mu.Unlock()

	sc.InjectMessage(key, agent.InjectedMessage{Text: "cancel1", ClientMessageID: "local-c1"})
	sc.InjectMessage(key, agent.InjectedMessage{Text: "cancel2", ClientMessageID: "local-c2"})
	sc.RetractInject(key, "local-c1")
	sc.RetractInject(key, "local-c2")

	survivors := sc.DrainSurvivorsOrCloseInject(key)
	if len(survivors) != 0 {
		t.Fatalf("all-retracted batch must yield no survivors, got %+v", survivors)
	}
	if sc.HasActiveRun(key) {
		t.Fatal("window must close when only retracted follow-ups were buffered")
	}
}

// TestSessionCache_DrainSurvivorsOrCloseInject_MixedKeepsWindowOpen: a retracted
// follow-up is reaped while a live one is returned, and the window stays open.
func TestSessionCache_DrainSurvivorsOrCloseInject_MixedKeepsWindowOpen(t *testing.T) {
	dir := t.TempDir()
	sc := NewSessionCache(dir)

	const key = "session:sess-mixed"
	sc.mu.Lock()
	sc.routes[key] = &routeEntry{injectCh: make(chan agent.InjectedMessage, 10), done: make(chan struct{})}
	sc.mu.Unlock()

	sc.InjectMessage(key, agent.InjectedMessage{Text: "keep me", ClientMessageID: "local-keep"})
	sc.InjectMessage(key, agent.InjectedMessage{Text: "cancelled", ClientMessageID: "local-drop"})
	sc.RetractInject(key, "local-drop")

	survivors := sc.DrainSurvivorsOrCloseInject(key)
	if len(survivors) != 1 || survivors[0].ClientMessageID != "local-keep" {
		t.Fatalf("retracted survivor must be skipped, live one returned, got %+v", survivors)
	}
	if !sc.HasActiveRun(key) {
		t.Fatal("window must stay open while a live survivor remains")
	}
}
