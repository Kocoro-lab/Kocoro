package daemon

import "testing"

// TestSessionCache_RetractInject_OneShot verifies the daemon-side steering
// retraction set: RetractInject marks a client_message_id; ConsumeInjectRetracted
// returns true exactly once (one-shot, removing the tombstone) then false.
func TestSessionCache_RetractInject_OneShot(t *testing.T) {
	sc := NewSessionCache(t.TempDir())
	const route = "session:r1"
	const id = "local-x"

	if sc.ConsumeInjectRetracted(route, id) {
		t.Fatal("not retracted yet: ConsumeInjectRetracted should be false")
	}
	sc.RetractInject(route, id)
	if !sc.ConsumeInjectRetracted(route, id) {
		t.Fatal("after RetractInject: first ConsumeInjectRetracted should be true")
	}
	if sc.ConsumeInjectRetracted(route, id) {
		t.Fatal("one-shot: second ConsumeInjectRetracted should be false")
	}
}

// TestSessionCache_RetractInject_SurvivesRunEnd verifies the durable-tombstone
// semantics: an unconsumed retraction (target cancelled but never drained
// because the run ended first) SURVIVES ClearRouteRunState, so a late copy of
// the cancelled follow-up — landing on the next run via inject or mailbox —
// is still dropped. Reaping is TTL + cap based (pruneInjectLedgerLocked), not
// wholesale at run end. This deliberately inverts the pre-2026-06 behavior,
// which reaped at run end and let a retract that lost the teardown race leak
// its target into the replacement run.
func TestSessionCache_RetractInject_SurvivesRunEnd(t *testing.T) {
	sc := NewSessionCache(t.TempDir())
	const route = "session:r2"
	sc.RetractInject(route, "local-y")
	sc.ClearRouteRunState(route)
	if !sc.ConsumeInjectRetracted(route, "local-y") {
		t.Fatal("tombstone must survive run end (TTL-reaped, not run-scoped)")
	}
}

// TestSessionCache_RetractInject_RouteIsolation verifies retractions are keyed
// per route — retracting on one route must not affect another.
func TestSessionCache_RetractInject_RouteIsolation(t *testing.T) {
	sc := NewSessionCache(t.TempDir())
	sc.RetractInject("session:a", "local-z")
	if sc.ConsumeInjectRetracted("session:b", "local-z") {
		t.Fatal("retraction must be per-route: route b should not see route a's tombstone")
	}
	if !sc.ConsumeInjectRetracted("session:a", "local-z") {
		t.Fatal("route a should still have its own tombstone")
	}
}
