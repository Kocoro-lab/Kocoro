package daemon

import (
	"encoding/json"
	"testing"
)

// setupCacheWithRoute returns a real SessionCache with one route entry
// registered under routeKey. It briefly locks+unlocks the route so the entry
// is materialized in sc.routes; AppendDrainedInflight no-ops on missing
// routes, so without this the test would silently observe zero appends.
func setupCacheWithRoute(t *testing.T, routeKey string) *SessionCache {
	t.Helper()
	cache := NewSessionCache(t.TempDir())
	cache.LockRouteWithManager(routeKey, t.TempDir())
	cache.UnlockRoute(routeKey)
	return cache
}

// snapshotDrainedInflight reads the route's drained-inflight slice under
// sc.mu. Test-only; exists so completion tests can assert the slice was
// cleared without poking unexported fields from outside the package.
func (sc *SessionCache) snapshotDrainedInflight(routeKey string) []DrainedInflightEntry {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	e, ok := sc.routes[routeKey]
	if !ok || e == nil {
		return nil
	}
	out := make([]DrainedInflightEntry, len(e.drainedInflight))
	copy(out, e.drainedInflight)
	return out
}

func TestRunCompletionEmitsDoneAndCleared(t *testing.T) {
	ws := &fakeLifecycleSender{}
	cache := setupCacheWithRoute(t, "route-X")
	cache.AppendDrainedInflight("route-X", DrainedInflightEntry{
		CloudMessageID:  "m1",
		IMStatusContext: json.RawMessage(`{"platform":"slack","ts":"1"}`),
	})
	cache.AppendDrainedInflight("route-X", DrainedInflightEntry{
		CloudMessageID:  "m2",
		IMStatusContext: json.RawMessage(`{"platform":"slack","ts":"2"}`),
	})
	cache.AppendDrainedInflight("route-X", DrainedInflightEntry{
		CloudMessageID:  "m3",
		IMStatusContext: json.RawMessage(`{"platform":"slack","ts":"3"}`),
	})

	EmitLifecycleOnRunCompletion(ws, cache, "route-X")

	if len(ws.events) != 3 {
		t.Fatalf("want 3 events, got %d", len(ws.events))
	}
	// m1, m2 cleared; m3 (tail) done
	if ws.events[0].MessageID != "m1" || ws.events[0].Data["state"] != LifecycleCleared {
		t.Fatalf("m1 should be cleared, got %+v", ws.events[0])
	}
	if ws.events[1].MessageID != "m2" || ws.events[1].Data["state"] != LifecycleCleared {
		t.Fatalf("m2 should be cleared, got %+v", ws.events[1])
	}
	if ws.events[2].MessageID != "m3" || ws.events[2].Data["state"] != LifecycleDone {
		t.Fatalf("m3 should be done, got %+v", ws.events[2])
	}
	for i, want := range []string{`{"platform":"slack","ts":"1"}`, `{"platform":"slack","ts":"2"}`, `{"platform":"slack","ts":"3"}`} {
		got, ok := ws.events[i].Data["im_status_context"].(json.RawMessage)
		if !ok {
			t.Fatalf("event %d im_status_context wrong type: %T", i, ws.events[i].Data["im_status_context"])
		}
		if string(got) != want {
			t.Fatalf("event %d context: got %s want %s", i, got, want)
		}
	}

	// Slice must be cleared after emit.
	if remaining := cache.snapshotDrainedInflight("route-X"); len(remaining) != 0 {
		t.Fatalf("drained slice should be cleared after emit, got %d entries", len(remaining))
	}
}

func TestRunCompletionWithEmptyDrainedSliceIsNoop(t *testing.T) {
	ws := &fakeLifecycleSender{}
	cache := setupCacheWithRoute(t, "route-X")
	EmitLifecycleOnRunCompletion(ws, cache, "route-X")
	if got := len(ws.events); got != 0 {
		t.Fatalf("expected 0 events, got %d", got)
	}
}

func TestRunCompletionNilArgsArentPanic(t *testing.T) {
	EmitLifecycleOnRunCompletion(nil, nil, "")
	EmitLifecycleOnRunCompletion(nil, NewSessionCache(t.TempDir()), "route-X")
	// no panic = pass
}

func TestRunCompletionSingleEntryEmitsDoneOnly(t *testing.T) {
	ws := &fakeLifecycleSender{}
	cache := setupCacheWithRoute(t, "route-X")
	cache.AppendDrainedInflight("route-X", DrainedInflightEntry{
		CloudMessageID:  "m1",
		IMStatusContext: json.RawMessage(`{"platform":"slack"}`),
	})
	EmitLifecycleOnRunCompletion(ws, cache, "route-X")
	if len(ws.events) != 1 || ws.events[0].Data["state"] != LifecycleDone {
		t.Fatalf("single entry should emit done only, got %+v", ws.events)
	}
}

// Second call after the slice is taken must be a silent no-op (idempotent).
func TestRunCompletionIdempotent(t *testing.T) {
	ws := &fakeLifecycleSender{}
	cache := setupCacheWithRoute(t, "route-X")
	cache.AppendDrainedInflight("route-X", DrainedInflightEntry{
		CloudMessageID:  "m1",
		IMStatusContext: json.RawMessage(`{"platform":"slack"}`),
	})
	EmitLifecycleOnRunCompletion(ws, cache, "route-X")
	if len(ws.events) != 1 {
		t.Fatalf("first call: want 1 event, got %d", len(ws.events))
	}
	EmitLifecycleOnRunCompletion(ws, cache, "route-X")
	if len(ws.events) != 1 {
		t.Fatalf("second call must be no-op, total events now %d", len(ws.events))
	}
}

func TestTakeDrainedInflight_ClearsSliceAtomically(t *testing.T) {
	cache := setupCacheWithRoute(t, "route-Y")
	cache.AppendDrainedInflight("route-Y", DrainedInflightEntry{CloudMessageID: "a"})
	cache.AppendDrainedInflight("route-Y", DrainedInflightEntry{CloudMessageID: "b"})

	got := cache.TakeDrainedInflight("route-Y")
	if len(got) != 2 || got[0].CloudMessageID != "a" || got[1].CloudMessageID != "b" {
		t.Fatalf("Take returned wrong slice: %+v", got)
	}
	if remaining := cache.snapshotDrainedInflight("route-Y"); len(remaining) != 0 {
		t.Fatalf("Take must clear, got %d remaining", len(remaining))
	}
	// Second call yields empty (no entry / drained already).
	if again := cache.TakeDrainedInflight("route-Y"); len(again) != 0 {
		t.Fatalf("second Take must be empty, got %d", len(again))
	}
}

func TestTakeDrainedInflight_NoopOnEmptyKeyOrMissingRoute(t *testing.T) {
	cache := NewSessionCache(t.TempDir())
	if got := cache.TakeDrainedInflight(""); got != nil {
		t.Fatalf("empty key must return nil, got %+v", got)
	}
	if got := cache.TakeDrainedInflight("never-existed"); got != nil {
		t.Fatalf("missing route must return nil, got %+v", got)
	}
}
