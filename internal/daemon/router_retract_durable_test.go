package daemon

import (
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// withActiveRun installs live run state (done + injectCh) on a route so
// InjectMessage treats it as an active run. Returns the inject channel.
func withActiveRun(t *testing.T, sc *SessionCache, key string) chan agent.InjectedMessage {
	t.Helper()
	injectCh := make(chan agent.InjectedMessage, 10)
	sc.mu.Lock()
	sc.routes[key] = &routeEntry{
		injectCh:   injectCh,
		done:       make(chan struct{}),
		lastAccess: time.Now(),
	}
	sc.mu.Unlock()
	return injectCh
}

// TestSessionCache_RetractStatus_AlreadyCommitted is the contract behind the
// force-send duplicate fix: once the drain-time checker has resolved an id as
// "kept" (ConsumeInjectRetractedOrMarkCommitted returned false), a retract for
// that id must answer "already_committed" so the client knows the text already
// lives in the session and must not be re-sent as a fresh message.
func TestSessionCache_RetractStatus_AlreadyCommitted(t *testing.T) {
	sc := NewSessionCache(t.TempDir())
	const route = "session:commit-race"
	const id = "local-committed"

	if sc.ConsumeInjectRetractedOrMarkCommitted(route, id) {
		t.Fatal("no tombstone yet: checker must keep the follow-up")
	}
	if got := sc.RetractInjectWithStatus(route, id); got != "already_committed" {
		t.Fatalf("RetractInjectWithStatus after commit = %q, want already_committed", got)
	}
	// "already_committed" must NOT plant a tombstone: a later inject reusing
	// nothing (and the committed copy itself) stays unaffected.
	withActiveRun(t, sc, route)
	if got := sc.InjectMessage(route, agent.InjectedMessage{Text: "x", ClientMessageID: id}); got != InjectOK {
		t.Fatalf("InjectMessage after already_committed retract = %v, want InjectOK (no tombstone)", got)
	}
}

// TestSessionCache_RetractStatus_RetractedBeforeDrain covers the winning-side
// race: retract lands first, the drain-time checker then drops the follow-up,
// and the id never enters the committed ledger.
func TestSessionCache_RetractStatus_RetractedBeforeDrain(t *testing.T) {
	sc := NewSessionCache(t.TempDir())
	const route = "session:retract-first"
	const id = "local-retracted"

	if got := sc.RetractInjectWithStatus(route, id); got != "retracted" {
		t.Fatalf("RetractInjectWithStatus = %q, want retracted", got)
	}
	if !sc.ConsumeInjectRetractedOrMarkCommitted(route, id) {
		t.Fatal("checker must drop the retracted follow-up")
	}
	if sc.WasInjectCommitted(route, id) {
		t.Fatal("a dropped follow-up must not enter the committed ledger")
	}
}

// TestSessionCache_TombstoneDropsLateInjectOnNextRun is the Bug-D guard: a
// retract whose target inject is delayed past the entire retract+cancel+resend
// sequence must still drop the late inject when it lands on the REPLACEMENT
// run — tombstones are keyed per route and survive run transitions.
func TestSessionCache_TombstoneDropsLateInjectOnNextRun(t *testing.T) {
	sc := NewSessionCache(t.TempDir())
	const route = "session:late-inject"
	const id = "local-late"

	// Run 1 owns the route; the user retracts; run 1 ends.
	withActiveRun(t, sc, route)
	sc.RetractInject(route, id)
	sc.ClearRouteRunState(route)

	// Run 2 starts on the same route; the stale inject finally arrives.
	sc.SetRouteRunState(route, make(chan struct{}), make(chan agent.InjectedMessage, 10), "")
	if got := sc.InjectMessage(route, agent.InjectedMessage{Text: "stale", ClientMessageID: id}); got != InjectRetracted {
		t.Fatalf("late inject after retract = %v, want InjectRetracted", got)
	}
	// One-shot: the tombstone was consumed by the drop.
	if got := sc.InjectMessage(route, agent.InjectedMessage{Text: "fresh", ClientMessageID: id}); got != InjectOK {
		t.Fatalf("second inject (tombstone consumed) = %v, want InjectOK", got)
	}
}

// TestSessionCache_ReEnqueueSurvivors_CarriesClientMessageID verifies the
// mailbox row keeps the inject's client id — the hook both the retract cascade
// and the DrainMailbox tombstone filter depend on.
func TestSessionCache_ReEnqueueSurvivors_CarriesClientMessageID(t *testing.T) {
	sc := NewSessionCache(t.TempDir())
	const route = "session:carry-id"
	const id = "local-survivor"

	injectCh := withActiveRun(t, sc, route)
	injectCh <- agent.InjectedMessage{Text: "park me", ClientMessageID: id}

	if n := sc.ReEnqueueInjectSurvivors(route); n != 1 {
		t.Fatalf("ReEnqueueInjectSurvivors = %d, want 1", n)
	}
	rows := sc.DrainMailbox(route, 10)
	if len(rows) != 1 || rows[0].ClientMessageID != id {
		t.Fatalf("mailbox row must carry the inject's client id, got %+v", rows)
	}
	// Parking in the mailbox is NOT a commit: a retract for this id must not
	// claim already_committed (that answer would make the client drop the text
	// while the cascade deletes the row — losing the message entirely).
	if got := sc.RetractInjectWithStatus(route, id); got != "retracted" {
		t.Fatalf("retract of a mailbox-parked survivor = %q, want retracted", got)
	}
}

// TestSessionCache_RetractCascadesToMailbox is the Bug-C guard: a retract that
// arrives AFTER ReEnqueueInjectSurvivors parked the inject in the durable
// mailbox must delete the row, so the next run's startup drain cannot prepend
// the cancelled text to the new prompt (the "text\ntext" duplicate).
func TestSessionCache_RetractCascadesToMailbox(t *testing.T) {
	sc := NewSessionCache(t.TempDir())
	const route = "session:cascade"
	const id = "local-cascade"

	injectCh := withActiveRun(t, sc, route)
	injectCh <- agent.InjectedMessage{Text: "duplicate me", ClientMessageID: id}
	if n := sc.ReEnqueueInjectSurvivors(route); n != 1 {
		t.Fatalf("ReEnqueueInjectSurvivors = %d, want 1", n)
	}

	if removed := sc.RetractMailboxByClientMessageID(route, id); removed != 1 {
		t.Fatalf("RetractMailboxByClientMessageID = %d, want 1", removed)
	}
	if rows := sc.DrainMailbox(route, 10); len(rows) != 0 {
		t.Fatalf("cascaded retract must empty the mailbox, got %+v", rows)
	}
}

// TestSessionCache_DrainMailbox_DropsTombstonedRows covers the residual race
// the cascade cannot reach: the retract lands between the survivor drain and
// the mailbox insert (nothing to cascade-delete yet), so the row materializes
// with a live tombstone. The next run's DrainMailbox must drop it instead of
// handing it to the startup prepend.
func TestSessionCache_DrainMailbox_DropsTombstonedRows(t *testing.T) {
	sc := NewSessionCache(t.TempDir())
	const route = "session:tombstoned-row"
	const id = "local-row"

	injectCh := withActiveRun(t, sc, route)
	injectCh <- agent.InjectedMessage{Text: "cancelled text", ClientMessageID: id}
	if n := sc.ReEnqueueInjectSurvivors(route); n != 1 {
		t.Fatalf("ReEnqueueInjectSurvivors = %d, want 1", n)
	}
	// Tombstone planted after the row already exists (cascade not used).
	sc.RetractInject(route, id)

	if rows := sc.DrainMailbox(route, 10); len(rows) != 0 {
		t.Fatalf("DrainMailbox must drop tombstoned rows, got %+v", rows)
	}
}

// TestSessionCache_EndTurnDrain_MarksSurvivorsCommitted pins the
// markSurvivorsCommitted contract: the end_turn guard (true) registers
// survivors in the committed ledger; the ReEnqueue path (false) must not.
func TestSessionCache_EndTurnDrain_MarksSurvivorsCommitted(t *testing.T) {
	sc := NewSessionCache(t.TempDir())
	const route = "session:endturn-mark"
	const id = "local-endturn"

	injectCh := withActiveRun(t, sc, route)
	injectCh <- agent.InjectedMessage{Text: "follow-up", ClientMessageID: id}

	survivors := sc.DrainSurvivorsOrCloseInject(route, true)
	if len(survivors) != 1 {
		t.Fatalf("survivors = %d, want 1", len(survivors))
	}
	if !sc.WasInjectCommitted(route, id) {
		t.Fatal("end_turn survivors must enter the committed ledger")
	}
	if got := sc.RetractInjectWithStatus(route, id); got != "already_committed" {
		t.Fatalf("retract after end_turn commit = %q, want already_committed", got)
	}
}

// TestPruneInjectLedger_TTLAndCap unit-tests the reaping helper directly:
// expired entries vanish, and over-cap sets evict oldest-first down to cap.
func TestPruneInjectLedger_TTLAndCap(t *testing.T) {
	now := time.Now()
	ledger := map[string]map[string]time.Time{
		"r": {
			"fresh":   now,
			"expired": now.Add(-injectLedgerTTL - time.Minute),
		},
	}
	pruneInjectLedgerLocked(ledger, "r", now)
	if _, ok := ledger["r"]["expired"]; ok {
		t.Fatal("expired entry must be reaped")
	}
	if _, ok := ledger["r"]["fresh"]; !ok {
		t.Fatal("fresh entry must survive")
	}

	over := map[string]time.Time{}
	for i := 0; i < injectLedgerCap+10; i++ {
		over[string(rune('a'+i%26))+string(rune('0'+i/26))] = now.Add(time.Duration(i) * time.Second)
	}
	ledger["r"] = over
	pruneInjectLedgerLocked(ledger, "r", now.Add(time.Duration(injectLedgerCap+10)*time.Second))
	if len(ledger["r"]) > injectLedgerCap {
		t.Fatalf("cap eviction failed: %d entries > cap %d", len(ledger["r"]), injectLedgerCap)
	}

	empty := map[string]map[string]time.Time{"r": {"only": now.Add(-injectLedgerTTL - time.Hour)}}
	pruneInjectLedgerLocked(empty, "r", now)
	if _, ok := empty["r"]; ok {
		t.Fatal("emptied set must be deleted from the ledger")
	}
}

// TestSessionCache_LedgerSweep_ReapsIdleRoutes pins the leak fix flagged in
// review: the inject ledgers are keyed like sc.routes (which never shrinks), so
// a route that committed/retracted an inject and then went idle would retain
// its entry for the daemon's lifetime. sweepInjectLedgersLocked reclaims those
// expired keys cross-route on the throttled write path.
func TestSessionCache_LedgerSweep_ReapsIdleRoutes(t *testing.T) {
	sc := NewSessionCache(t.TempDir())
	now := time.Now()
	expired := now.Add(-injectLedgerTTL - time.Minute)

	sc.mu.Lock()
	// 50 idle routes, each holding one expired entry in each ledger — the
	// "every session route that ever drained an inject" shape.
	for i := 0; i < 50; i++ {
		key := "session:idle-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		sc.retractedInjects[key] = map[string]time.Time{"r": expired}
		sc.committedInjects[key] = map[string]time.Time{"c": expired}
	}
	// One fresh route that must survive the sweep.
	sc.committedInjects["session:live"] = map[string]time.Time{"c": now}
	// Force the throttle open.
	sc.lastInjectLedgerSweep = time.Time{}
	sc.sweepInjectLedgersLocked(now)
	retractKeys := len(sc.retractedInjects)
	committedKeys := len(sc.committedInjects)
	_, liveSurvived := sc.committedInjects["session:live"]
	sc.mu.Unlock()

	if retractKeys != 0 {
		t.Errorf("expired retract ledger keys not reaped: %d remain", retractKeys)
	}
	if committedKeys != 1 || !liveSurvived {
		t.Errorf("sweep should leave only the live route: %d keys, liveSurvived=%v", committedKeys, liveSurvived)
	}
}

// TestSessionCache_LedgerSweep_Throttled verifies the sweep does not run more
// than once per interval, so the write path stays cheap.
func TestSessionCache_LedgerSweep_Throttled(t *testing.T) {
	sc := NewSessionCache(t.TempDir())
	now := time.Now()
	expired := now.Add(-injectLedgerTTL - time.Minute)

	sc.mu.Lock()
	sc.lastInjectLedgerSweep = now // a sweep just ran
	sc.committedInjects["session:stale"] = map[string]time.Time{"c": expired}
	// Within the interval: must be a no-op, the stale entry survives.
	sc.sweepInjectLedgersLocked(now.Add(injectLedgerSweepInterval / 2))
	_, withinSurvives := sc.committedInjects["session:stale"]
	// Past the interval: the stale entry is reclaimed.
	sc.sweepInjectLedgersLocked(now.Add(injectLedgerSweepInterval + time.Second))
	_, afterReaped := sc.committedInjects["session:stale"]
	sc.mu.Unlock()

	if !withinSurvives {
		t.Error("sweep ran inside the throttle interval (should be a no-op)")
	}
	if afterReaped {
		t.Error("sweep did not run after the throttle interval elapsed")
	}
}

// TestSessionCache_EvictRoute_ReapsLedgers verifies the immediate-reclaim path:
// when an agent is evicted, its routes' inject ledgers are dropped at once
// rather than waiting for the periodic sweep.
func TestSessionCache_EvictRoute_ReapsLedgers(t *testing.T) {
	sc := NewSessionCache(t.TempDir())
	const agentName = "researcher"
	key := sc.agentRouteKey(agentName) // "agent:researcher" — matches Evict's prefix

	sc.mu.Lock()
	sc.routes[key] = &routeEntry{lastAccess: time.Now()}
	sc.retractedInjects[key] = map[string]time.Time{"r": time.Now()}
	sc.committedInjects[key] = map[string]time.Time{"c": time.Now()}
	sc.mu.Unlock()

	sc.Evict(agentName)

	sc.mu.Lock()
	_, retractLeft := sc.retractedInjects[key]
	_, committedLeft := sc.committedInjects[key]
	sc.mu.Unlock()
	if retractLeft || committedLeft {
		t.Errorf("evict must reap inject ledgers: retract=%v committed=%v", retractLeft, committedLeft)
	}
}
