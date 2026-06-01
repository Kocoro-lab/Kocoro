package daemon

import "sync"

// QGState describes a route's run-entry state. State transitions are
// synchronous and guarded by an internal mutex; the daemon uses these to
// decide whether incoming messages enqueue (someone owns the route) or kick
// off a fresh run (route is idle).
type QGState int

const (
	QGStateIdle        QGState = iota // No run owns this route.
	QGStateDispatching                // A caller has reserved the route but the loop hasn't started yet.
	QGStateRunning                    // Loop is live.
)

// String returns a stable identifier for logging.
func (s QGState) String() string {
	switch s {
	case QGStateIdle:
		return "idle"
	case QGStateDispatching:
		return "dispatching"
	case QGStateRunning:
		return "running"
	}
	return "unknown"
}

// QueryGuard is a per-route 3-state machine with a generation counter. It
// controls only *who may start a new run on this route* — it does NOT
// replace the session-mutation lock
// (routeEntry.mu) and does NOT serialize session writes.
//
// The generation counter exists so finalizers from a force-ended run can
// detect they are stale: if guard.End(myGen) returns false, the generation
// bumped under us via ForceEnd and we must not touch shared state.
//
// Concurrency model:
//   - Idle → Dispatching: Reserve() returns the new generation; only one
//     caller wins under contention.
//   - Dispatching → Running: TryStart(gen) succeeds iff gen matches the
//     reservation. Used right after the loop has its ctx + done channel ready.
//   - Dispatching → Idle: CancelReservation(gen) for setup failures that
//     happen before TryStart could be reached (e.g. session resolution error).
//   - Running → Idle (normal): End(gen). Returns false if the run was
//     force-ended (caller must bail out of any post-run cleanup).
//   - Running → Idle (forced): ForceEnd(). Bumps generation so any pending
//     End(gen) becomes a no-op.
//
// All transitions are O(1) under a single sync.Mutex; the state field is not
// touched outside this struct's methods.
type QueryGuard struct {
	mu    sync.Mutex
	state QGState
	gen   uint64
}

// NewQueryGuard returns a guard in the Idle state.
func NewQueryGuard() *QueryGuard {
	return &QueryGuard{state: QGStateIdle}
}

// Reserve atomically moves Idle → Dispatching and returns the new generation.
// Returns (0, false) when the guard is already busy.
func (g *QueryGuard) Reserve() (uint64, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state != QGStateIdle {
		return 0, false
	}
	g.state = QGStateDispatching
	g.gen++
	return g.gen, true
}

// TryStart moves Dispatching → Running if gen matches the active reservation.
func (g *QueryGuard) TryStart(gen uint64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state != QGStateDispatching || g.gen != gen {
		return false
	}
	g.state = QGStateRunning
	return true
}

// CancelReservation moves Dispatching → Idle without bumping generation. Used
// when setup failed before TryStart could be called (e.g. session resolution
// error or context cancelled). End() on the same gen afterwards is a no-op
// since the guard is back to Idle.
func (g *QueryGuard) CancelReservation(gen uint64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state != QGStateDispatching || g.gen != gen {
		return false
	}
	g.state = QGStateIdle
	return true
}

// End is the loop's normal finalizer. Returns false if the run was already
// force-ended (generation mismatch) — caller must NOT touch shared state in
// that case, since a newer reservation may already be progressing.
func (g *QueryGuard) End(gen uint64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.gen != gen {
		return false
	}
	g.state = QGStateIdle
	return true
}

// ForceEnd is invoked on user cancel paths. It transitions to Idle and bumps
// generation so any subsequent End(oldGen) from the cancelled run is a no-op.
// Safe to call from any state.
func (g *QueryGuard) ForceEnd() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.state = QGStateIdle
	g.gen++
}

// State returns the current state for diagnostics. Race-safe.
func (g *QueryGuard) State() QGState {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state
}

// IsActive returns true when the guard is Dispatching or Running (i.e. some
// caller owns the route). InjectMessage uses this as the gate for "enqueue
// instead of starting fresh".
func (g *QueryGuard) IsActive() bool {
	s := g.State()
	return s == QGStateDispatching || s == QGStateRunning
}
