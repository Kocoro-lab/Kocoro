package daemon

import (
	"sync"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// SystemEventStore holds per-route queues of agent.SystemEvent for next-turn
// injection (S0). Keyed by the route key resolved by ComputeRouteKey — NEVER a
// shared agent session — so an event enqueued for one route can never surface
// on another (OpenClaw #36614 cross-session leak class). Drain empties and
// removes the route's queue, so the steady-state flow self-cleans every turn;
// Forget is wired into route eviction for routes that enqueue but never run
// again. One store per ServerDeps; daemon restart wipes it.
type SystemEventStore struct {
	mu     sync.Mutex
	queues map[string][]agent.SystemEvent
	// cap bounds events per route. Justified at 20 (OpenClaw's MAX_EVENTS) — a
	// burst of membership/delivery signals between two turns of one route;
	// beyond that, oldest are dropped FIFO and the model sees the most recent
	// state. Symptom when it binds: an old signal silently evicted before the
	// route's next turn (acceptable — newest state wins). Override:
	// daemon constructs with viper key `agent.system_event_cap`.
	cap int
}

// NewSystemEventStore returns an empty store. capPerRoute <= 0 falls back to 20.
func NewSystemEventStore(capPerRoute int) *SystemEventStore {
	if capPerRoute <= 0 {
		capPerRoute = 20
	}
	return &SystemEventStore{queues: make(map[string][]agent.SystemEvent), cap: capPerRoute}
}

// Enqueue appends ev to routeKey's queue. If ev.ContextKey is non-empty and the
// immediately-preceding queued event carries the same key, ev REPLACES it
// (consecutive-key collapse — keeps the newest state). Over-cap queues drop the
// oldest. nil-safe and empty-routeKey-safe (both no-op).
func (s *SystemEventStore) Enqueue(routeKey string, ev agent.SystemEvent) {
	if s == nil || routeKey == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.queues[routeKey]
	if ev.ContextKey != "" && len(q) > 0 && q[len(q)-1].ContextKey == ev.ContextKey {
		q[len(q)-1] = ev
		s.queues[routeKey] = q
		return
	}
	q = append(q, ev)
	if len(q) > s.cap {
		q = q[len(q)-s.cap:]
	}
	s.queues[routeKey] = q
}

// Drain returns routeKey's queued events in FIFO order and removes the queue.
// nil store / unknown route returns nil.
func (s *SystemEventStore) Drain(routeKey string) []agent.SystemEvent {
	if s == nil || routeKey == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.queues[routeKey]
	if len(q) == 0 {
		return nil
	}
	delete(s.queues, routeKey)
	return q
}

// Forget drops a route's queue without draining. Called from route eviction so
// a route that enqueued but never ran again does not leak its (bounded) queue.
func (s *SystemEventStore) Forget(routeKey string) {
	if s == nil || routeKey == "" {
		return
	}
	s.mu.Lock()
	delete(s.queues, routeKey)
	s.mu.Unlock()
}
