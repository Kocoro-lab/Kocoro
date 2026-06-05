package daemon

import (
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

func ev(text, key string) agent.SystemEvent {
	return agent.SystemEvent{Text: text, ContextKey: key, TS: time.Now()}
}

func TestSystemEventStore_DrainReturnsAndClears(t *testing.T) {
	s := NewSystemEventStore(20)
	s.Enqueue("route-A", ev("one", ""))
	s.Enqueue("route-A", ev("two", ""))
	got := s.Drain("route-A")
	if len(got) != 2 || got[0].Text != "one" || got[1].Text != "two" {
		t.Fatalf("Drain = %+v", got)
	}
	if again := s.Drain("route-A"); len(again) != 0 {
		t.Fatalf("second Drain should be empty, got %+v", again)
	}
}

func TestSystemEventStore_RouteBinding(t *testing.T) {
	s := NewSystemEventStore(20)
	s.Enqueue("route-A", ev("a-only", ""))
	if got := s.Drain("route-B"); len(got) != 0 {
		t.Fatalf("route-B must not see route-A events, got %+v", got)
	}
	if got := s.Drain("route-A"); len(got) != 1 {
		t.Fatalf("route-A should still hold its event, got %+v", got)
	}
}

func TestSystemEventStore_ConsecutiveContextKeyDedup(t *testing.T) {
	s := NewSystemEventStore(20)
	s.Enqueue("r", ev("kicked from #ops (old)", "kick:#ops"))
	s.Enqueue("r", ev("kicked from #ops (new)", "kick:#ops")) // same key, consecutive -> replaces
	got := s.Drain("r")
	if len(got) != 1 || got[0].Text != "kicked from #ops (new)" {
		t.Fatalf("expected single newest event, got %+v", got)
	}
}

func TestSystemEventStore_NonConsecutiveSameKeyNotCollapsed(t *testing.T) {
	s := NewSystemEventStore(20)
	s.Enqueue("r", ev("k1", "kick:#ops"))
	s.Enqueue("r", ev("other", "other"))   // breaks the run
	s.Enqueue("r", ev("k2", "kick:#ops"))   // same key but not consecutive
	if got := s.Drain("r"); len(got) != 3 {
		t.Fatalf("non-consecutive same key must not collapse, got %d: %+v", len(got), got)
	}
}

func TestSystemEventStore_EmptyContextKeyNeverDedups(t *testing.T) {
	s := NewSystemEventStore(20)
	s.Enqueue("r", ev("x", ""))
	s.Enqueue("r", ev("x", "")) // identical text, empty key -> kept (no key dedup)
	if got := s.Drain("r"); len(got) != 2 {
		t.Fatalf("empty-key events must not collapse, got %d", len(got))
	}
}

func TestSystemEventStore_CapEvictsOldest(t *testing.T) {
	s := NewSystemEventStore(3)
	for _, txt := range []string{"e1", "e2", "e3", "e4"} {
		s.Enqueue("r", ev(txt, ""))
	}
	got := s.Drain("r")
	if len(got) != 3 || got[0].Text != "e2" || got[2].Text != "e4" {
		t.Fatalf("cap=3 should keep newest 3 [e2 e3 e4], got %+v", got)
	}
}

func TestSystemEventStore_Forget(t *testing.T) {
	s := NewSystemEventStore(20)
	s.Enqueue("r", ev("x", ""))
	s.Forget("r")
	if got := s.Drain("r"); len(got) != 0 {
		t.Fatalf("Forget should drop the route queue, got %+v", got)
	}
}

func TestSystemEventStore_NilSafe(t *testing.T) {
	var s *SystemEventStore
	s.Enqueue("r", ev("x", "")) // must not panic
	if got := s.Drain("r"); got != nil {
		t.Fatalf("nil store Drain should be nil, got %+v", got)
	}
	s.Forget("r") // must not panic
}
