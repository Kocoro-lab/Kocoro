package daemon

import (
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// TestSessionCache_ReEnqueueInjectSurvivors_RescuesNonEndTurnOrphan covers the
// gap left by the end_turn-only drain: on a NON-end_turn exit (LLM error,
// maxIter cap, empty-final) the loop never drains injectCh, so a follow-up that
// won InjectMessage (InjectOK) during teardown would be niled away by
// ClearRouteRunState and lost (busy-state injects have no mailbox replay).
// ReEnqueueInjectSurvivors must rescue it to the mailbox so the next run on the
// route replays it. (P5)
func TestSessionCache_ReEnqueueInjectSurvivors_RescuesNonEndTurnOrphan(t *testing.T) {
	dir := t.TempDir()
	sc := NewSessionCache(dir)

	const key = "default:slack:thread-orphan"
	injectCh := make(chan agent.InjectedMessage, 10)
	sc.mu.Lock()
	sc.routes[key] = &routeEntry{injectCh: injectCh, done: make(chan struct{})}
	sc.mu.Unlock()

	// A follow-up wins InjectMessage while the run is tearing down on a path
	// that does not drain injectCh.
	if got := sc.InjectMessage(key, agent.InjectedMessage{Text: "late follow-up", ClientMessageID: "local-late"}); got != InjectOK {
		t.Fatalf("InjectMessage = %v, want InjectOK", got)
	}

	// Runner cleanup rescues the survivor before ClearRouteRunState nils the
	// channel.
	if n := sc.ReEnqueueInjectSurvivors(key); n != 1 {
		t.Fatalf("ReEnqueueInjectSurvivors = %d, want 1 (survivor rescued, not orphaned)", n)
	}
	sc.ClearRouteRunState(key)

	// The inject window must be closed and the follow-up replayable from the
	// mailbox — not silently dropped.
	if sc.HasActiveRun(key) {
		t.Fatal("inject window must be closed after rescue + clear")
	}
	pending := sc.DrainMailbox(key, 10)
	if len(pending) != 1 || pending[0].Text != "late follow-up" {
		t.Fatalf("survivor must be replayable from the mailbox, got %+v", pending)
	}
}

// TestSessionCache_ReEnqueueInjectSurvivors_NoopOnClosedWindow: the end_turn
// path already drained + closed the window, so the cleanup rescue is an
// idempotent no-op that enqueues nothing.
func TestSessionCache_ReEnqueueInjectSurvivors_NoopOnClosedWindow(t *testing.T) {
	dir := t.TempDir()
	sc := NewSessionCache(dir)

	const key = "default:slack:thread-clean"
	sc.mu.Lock()
	sc.routes[key] = &routeEntry{injectCh: make(chan agent.InjectedMessage, 10), done: make(chan struct{})}
	sc.mu.Unlock()

	// Simulate the end_turn empty-drain closing the window.
	_ = sc.DrainSurvivorsOrCloseInject(key)

	if n := sc.ReEnqueueInjectSurvivors(key); n != 0 {
		t.Fatalf("ReEnqueueInjectSurvivors on a closed window = %d, want 0", n)
	}
	if got := sc.DrainMailbox(key, 10); len(got) != 0 {
		t.Fatalf("no message should be enqueued on a no-op rescue, got %+v", got)
	}
}
