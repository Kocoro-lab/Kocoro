package agent

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TurnPhase is the discrete blocking stage a single AgentLoop.Run is in.
// Every blocking boundary in Run() either (a) calls tracker.Enter(p) to
// transition the top-level phase, or (b) wraps itself in
// tracker.EnterTransient(p)...restore() when the call is nested inside an
// outer phase.
type TurnPhase int

const (
	PhaseInit TurnPhase = iota
	PhaseSetup
	PhaseAwaitingLLM
	PhaseRetryingLLM
	PhaseCompacting
	PhaseAwaitingApproval
	PhaseExecutingTools
	PhaseInjectingMessage
	PhaseForceStop
	PhaseDone
)

func (p TurnPhase) String() string {
	switch p {
	case PhaseInit:
		return "init"
	case PhaseSetup:
		return "setup"
	case PhaseAwaitingLLM:
		return "awaiting_llm"
	case PhaseRetryingLLM:
		return "retrying_llm"
	case PhaseCompacting:
		return "compacting"
	case PhaseAwaitingApproval:
		return "awaiting_approval"
	case PhaseExecutingTools:
		return "executing_tools"
	case PhaseInjectingMessage:
		return "injecting_message"
	case PhaseForceStop:
		return "force_stop"
	case PhaseDone:
		return "done"
	}
	return "unknown"
}

// CountsAsIdle reports whether the watchdog should measure duration in this
// phase. Only phases that are strictly waiting on a remote LLM response are
// idle-counted. Tool execution, approval waits, retries, and compaction
// wrappers have their own bounded owners and are structurally excluded.
//
// INVARIANT: every LLM call inside PhaseCompacting MUST wrap itself in
// EnterTransient(PhaseAwaitingLLM) and restore when done, otherwise the
// watchdog silently loses coverage of those nested calls. This is enforced
// at the API level by EnterTransient always returning a restore closure;
// callers use `defer restore()` or synchronous `restore()` after the call.
func (p TurnPhase) CountsAsIdle() bool {
	return p == PhaseAwaitingLLM || p == PhaseForceStop
}

// phaseTracker holds the current phase and a timestamp of the last
// transition. Safe for concurrent read (the watchdog goroutine observes
// while the loop goroutine mutates). Writes take a write lock to keep the
// (phase, since) pair coherent.
//
// FAIL-CLOSED DESIGN:
//
//   - EnterTransient returns a restore closure that the caller must invoke.
//     If it is forgotten, transientDepth stays non-zero.
//   - AssertClean checks transientDepth at Run() exit. If any transient was
//     forgotten, the tracker panics under `go test` (via testing.Testing())
//     or when SHANNON_PHASE_STRICT=1 is set; otherwise it logs to stderr.
//     This catches the "forgot to restore, silently left in wrong phase"
//     bug at development time without crashing production.
//   - Enter() (top-level) panics if called while a transient is open,
//     because that would silently orphan the transient's restore. Again,
//     test-mode panic / production log.
//   - Any structural violation also sets `invalid`. Observers (e.g. the
//     watchdog) check Invalid() and silently disable themselves for the
//     rest of the run, rather than acting on untrustworthy phase data.
type phaseTracker struct {
	mu             sync.RWMutex
	phase          TurnPhase
	since          time.Time
	dirty          bool
	transientDepth int
	// seq increments on every successful Enter/EnterTransient (and on
	// restore). Observers use it to dedup soft-status firings by transition
	// rather than by phase type, so `AwaitingLLM → RetryingLLM → AwaitingLLM`
	// re-arms cleanly instead of silently suppressing the second soft event.
	seq int64
	// invalid is set by any structural violation (forgotten transient,
	// Enter-inside-transient). Observers must check Invalid() and disable
	// themselves for the rest of the run when set.
	invalid atomic.Bool
}

func newPhaseTracker() *phaseTracker {
	return &phaseTracker{phase: PhaseInit, since: time.Now()}
}

// Enter sets the current top-level phase. Not safe inside an active
// transient — a transient must be restored first. Typical use: sequential
// phase transitions driven from AgentLoop.Run on the loop goroutine.
//
// If called while a transient is still active, this is a structural
// violation: the tracker is marked invalid (observers self-disable),
// a diagnostic is logged (or panic under test/strict mode), and the
// transition is applied anyway. The lock is held across the whole
// sequence so a racing restore closure cannot clobber the write or
// leave transientDepth negative.
func (t *phaseTracker) Enter(p TurnPhase) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.transientDepth != 0 {
		// reportViolation writes to stderr / sets an atomic / may panic
		// under testing.Testing() — all safe to do under the mutex.
		t.reportViolation(fmt.Sprintf(
			"Enter(%s) called while transient is active (depth=%d, current=%s). "+
				"Use EnterTransient or restore first.", p, t.transientDepth, t.phase))
	}
	t.phase = p
	t.since = time.Now()
	t.seq++
}

// EnterTransient enters phase p and returns a restore closure that restores
// the previous phase. The closure is idempotent (safe to call twice — second
// call is a no-op). Typical use:
//
//	restore := tracker.EnterTransient(PhaseAwaitingLLM)
//	resp, err := client.Complete(ctx, req)
//	restore()
//
// Or with defer for panic safety:
//
//	defer tracker.EnterTransient(PhaseAwaitingLLM)()
func (t *phaseTracker) EnterTransient(p TurnPhase) func() {
	t.mu.Lock()
	prev := t.phase
	prevSince := t.since
	t.phase = p
	t.since = time.Now()
	t.seq++
	t.transientDepth++
	t.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			t.phase = prev
			t.since = prevSince
			t.seq++
			t.transientDepth--
			t.mu.Unlock()
		})
	}
}

// Current returns the current phase, the duration since it was last
// entered, and a monotonically increasing transition sequence number.
// Observers use (phase, seq) as the identity of an idle phase instance:
// the same TurnPhase reentered twice produces two different seq values,
// so a watchdog can re-arm its soft-status fire on re-entry.
// Safe from any goroutine (uses the read lock).
func (t *phaseTracker) Current() (TurnPhase, time.Duration, int64) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.phase, time.Since(t.since), t.seq
}

// Invalid reports whether the tracker has recorded a structural violation
// during this run. Observers should disable themselves when this is true.
func (t *phaseTracker) Invalid() bool { return t.invalid.Load() }

// MarkDirty signals that the current phase produced durable state the
// checkpoint hook should persist. Cleared by TakeDirty().
func (t *phaseTracker) MarkDirty() {
	t.mu.Lock()
	t.dirty = true
	t.mu.Unlock()
}

// TakeDirty atomically reads and clears the dirty flag.
func (t *phaseTracker) TakeDirty() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	d := t.dirty
	t.dirty = false
	return d
}

// IsDirty reads the dirty flag without clearing. Observers use it to
// peek before deciding to fire a callback; TakeDirty is called only
// after the callback succeeds so storage errors never silently drop
// the pending durable-state signal.
func (t *phaseTracker) IsDirty() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.dirty
}

// AssertClean reports a violation if any transient restore was forgotten.
// Call via defer at AgentLoop.Run exit.
func (t *phaseTracker) AssertClean() {
	t.mu.RLock()
	depth := t.transientDepth
	phase := t.phase
	t.mu.RUnlock()
	if depth != 0 {
		t.reportViolation(fmt.Sprintf(
			"pending transient at Run() exit: depth=%d, stuck_in=%s", depth, phase))
	}
}

// phaseStrictMode forces panics on violations in production builds.
// Enable with SHANNON_PHASE_STRICT=1 for diagnostic runs.
var phaseStrictMode = os.Getenv("SHANNON_PHASE_STRICT") == "1"

// reportViolation is the single choke point for structural phase
// violations. It always marks the tracker invalid (so observers disable
// themselves for the rest of the run). Panics under `go test` (via
// testing.Testing()) or when SHANNON_PHASE_STRICT=1; logs otherwise.
func (t *phaseTracker) reportViolation(msg string) {
	t.invalid.Store(true)
	if testing.Testing() || phaseStrictMode {
		panic("phaseTracker: " + msg)
	}
	fmt.Fprintf(os.Stderr, "[phase] WARN %s (tracker disabled for rest of run)\n", msg)
}
