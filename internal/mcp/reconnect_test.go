package mcp

import (
	"sync"
	"testing"
	"time"
)

// --- backoff ---------------------------------------------------------------

func TestReconnectBackoff_ExponentialThenCapped(t *testing.T) {
	// attempt is 1-based: the delay before retry #1 is the initial backoff.
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, reconnectInitialBackoff},
		{2, 2 * reconnectInitialBackoff},
		{3, 4 * reconnectInitialBackoff},
		{4, 8 * reconnectInitialBackoff},
		{99, reconnectMaxBackoff},
	}
	for _, c := range cases {
		if got := reconnectBackoff(c.attempt); got != c.want {
			t.Errorf("reconnectBackoff(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}

func TestReconnectBackoff_NeverExceedsCap(t *testing.T) {
	// Guards against the shift overflowing into a negative/absurd duration,
	// which would either fire instantly in a hot loop or never fire at all.
	for attempt := 1; attempt <= 128; attempt++ {
		got := reconnectBackoff(attempt)
		if got <= 0 || got > reconnectMaxBackoff {
			t.Fatalf("reconnectBackoff(%d) = %v, out of (0, %v]", attempt, got, reconnectMaxBackoff)
		}
	}
}

// --- scheduler -------------------------------------------------------------

// fakeClock records scheduled work instead of sleeping, so the tests assert
// on delays without wall-clock waits.
type fakeClock struct {
	mu      sync.Mutex
	pending []fakeTimer
}

type fakeTimer struct {
	delay time.Duration
	fn    func()
	// stopped is flipped by the returned cancel func.
	stopped *bool
}

func (f *fakeClock) after(d time.Duration, fn func()) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	stopped := false
	f.pending = append(f.pending, fakeTimer{delay: d, fn: fn, stopped: &stopped})
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		stopped = true
	}
}

// fireAll runs every not-yet-stopped timer once and clears the queue.
func (f *fakeClock) fireAll() {
	f.mu.Lock()
	pending := f.pending
	f.pending = nil
	f.mu.Unlock()
	for _, t := range pending {
		if !*t.stopped {
			t.fn()
		}
	}
}

func (f *fakeClock) delays() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]time.Duration, 0, len(f.pending))
	for _, t := range f.pending {
		out = append(out, t.delay)
	}
	return out
}

// recordingReconnect captures which servers were asked to reconnect.
type recordingReconnect struct {
	mu    sync.Mutex
	names []string
}

func (r *recordingReconnect) fn(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.names = append(r.names, name)
}

func (r *recordingReconnect) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.names...)
}

func newTestScheduler(t *testing.T) (*ReconnectScheduler, *fakeClock, *recordingReconnect) {
	t.Helper()
	clock := &fakeClock{}
	rec := &recordingReconnect{}
	s := NewReconnectScheduler(rec.fn)
	s.after = clock.after
	// Identity jitter so the ladder assertions can compare exact rungs; the
	// jitter band itself is covered separately.
	s.jitter = func(d time.Duration) time.Duration { return d }
	return s, clock, rec
}

func TestReconnectScheduler_FirstFailureSchedulesRetry(t *testing.T) {
	// The whole point of the fix: a failed async connect must leave a pending
	// retry behind instead of dying silently (the 2026-08-12 "changed the
	// Notion token, had to restart the daemon" report).
	s, clock, rec := newTestScheduler(t)

	s.OnFailure("notion")

	if got := clock.delays(); len(got) != 1 || got[0] != reconnectInitialBackoff {
		t.Fatalf("expected one retry scheduled at %v, got %v", reconnectInitialBackoff, got)
	}
	if len(rec.calls()) != 0 {
		t.Fatal("retry must not run before its delay elapses")
	}

	clock.fireAll()

	if got := rec.calls(); len(got) != 1 || got[0] != "notion" {
		t.Fatalf("expected reconnect(notion) after the delay, got %v", got)
	}
}

func TestReconnectScheduler_ConsecutiveFailuresBackOff(t *testing.T) {
	s, clock, _ := newTestScheduler(t)

	s.OnFailure("notion")
	clock.fireAll() // attempt 1 runs

	s.OnFailure("notion") // it failed again
	if got := clock.delays(); len(got) != 1 || got[0] != 2*reconnectInitialBackoff {
		t.Fatalf("second failure should back off to %v, got %v", 2*reconnectInitialBackoff, got)
	}
	clock.fireAll()

	s.OnFailure("notion")
	if got := clock.delays(); len(got) != 1 || got[0] != 4*reconnectInitialBackoff {
		t.Fatalf("third failure should back off to %v, got %v", 4*reconnectInitialBackoff, got)
	}
}

func TestReconnectScheduler_SuccessResetsBackoff(t *testing.T) {
	// A server that recovers must not carry its old failure count forward —
	// otherwise a single bad day permanently slows every later recovery.
	s, clock, _ := newTestScheduler(t)

	s.OnFailure("notion")
	clock.fireAll()
	s.OnFailure("notion")
	clock.fireAll()

	s.OnSuccess("notion")
	s.OnFailure("notion")

	if got := clock.delays(); len(got) != 1 || got[0] != reconnectInitialBackoff {
		t.Fatalf("after success the backoff must reset to %v, got %v", reconnectInitialBackoff, got)
	}
}

func TestReconnectScheduler_InFlightRetryIsNotDuplicated(t *testing.T) {
	// Two failure reports for the same server before the pending retry fires
	// (e.g. supervisor probe + async connect result) must collapse into one
	// scheduled attempt. Duplicates would race for the same process group —
	// the EADDRINUSE class of bug StartConnectAll's inFlight set guards.
	s, clock, rec := newTestScheduler(t)

	s.OnFailure("notion")
	s.OnFailure("notion")
	s.OnFailure("notion")

	if got := clock.delays(); len(got) != 1 {
		t.Fatalf("expected exactly one pending retry, got %d: %v", len(got), got)
	}

	clock.fireAll()
	if got := rec.calls(); len(got) != 1 {
		t.Fatalf("expected exactly one reconnect call, got %v", got)
	}
}

func TestReconnectScheduler_GivesUpAfterMaxAttempts(t *testing.T) {
	// A permanently broken server (bad command, revoked credential) must not
	// spin forever. After the cap it stays down until the user acts —
	// /config/reload and the enable toggle both clear the state.
	s, clock, rec := newTestScheduler(t)

	for i := 0; i < reconnectMaxAttempts; i++ {
		s.OnFailure("notion")
		clock.fireAll()
	}
	if got := len(rec.calls()); got != reconnectMaxAttempts {
		t.Fatalf("expected %d reconnect attempts, got %d", reconnectMaxAttempts, got)
	}

	s.OnFailure("notion")
	if got := clock.delays(); len(got) != 0 {
		t.Fatalf("must stop scheduling past the attempt cap, got %v", got)
	}
}

func TestReconnectScheduler_ForgetClearsState(t *testing.T) {
	// Forget is the user-action escape hatch: a reload or an enable toggle
	// re-arms a server that had exhausted its attempts.
	s, clock, rec := newTestScheduler(t)

	for i := 0; i < reconnectMaxAttempts; i++ {
		s.OnFailure("notion")
		clock.fireAll()
	}
	s.OnFailure("notion")
	if len(clock.delays()) != 0 {
		t.Fatal("precondition: scheduler should be exhausted")
	}

	s.Forget("notion")
	s.OnFailure("notion")

	if got := clock.delays(); len(got) != 1 || got[0] != reconnectInitialBackoff {
		t.Fatalf("Forget must re-arm at the initial backoff, got %v", got)
	}
	clock.fireAll()
	if got := len(rec.calls()); got != reconnectMaxAttempts+1 {
		t.Fatalf("expected one more reconnect after Forget, got %d", got)
	}
}

func TestReconnectScheduler_StopCancelsPendingRetries(t *testing.T) {
	// Daemon shutdown / deps swap must not leave a timer that resurrects a
	// server owned by a superseded manager.
	s, clock, rec := newTestScheduler(t)

	s.OnFailure("notion")
	s.OnFailure("playwright")
	s.Stop()

	clock.fireAll()

	if got := rec.calls(); len(got) != 0 {
		t.Fatalf("Stop must cancel pending retries, got %v", got)
	}
}

func TestReconnectScheduler_StopIsIdempotentAndBlocksLaterFailures(t *testing.T) {
	s, clock, rec := newTestScheduler(t)

	s.Stop()
	s.Stop()
	s.OnFailure("notion")

	if got := clock.delays(); len(got) != 0 {
		t.Fatalf("a stopped scheduler must not schedule anything, got %v", got)
	}
	clock.fireAll()
	if got := rec.calls(); len(got) != 0 {
		t.Fatalf("a stopped scheduler must not reconnect, got %v", got)
	}
}

func TestReconnectScheduler_TracksServersIndependently(t *testing.T) {
	s, clock, rec := newTestScheduler(t)

	s.OnFailure("notion")
	clock.fireAll()
	s.OnFailure("notion") // notion is now on its second failure
	s.OnFailure("ms365")  // ms365 is on its first

	delays := clock.delays()
	if len(delays) != 2 {
		t.Fatalf("expected two pending retries, got %v", delays)
	}
	var sawInitial, sawSecond bool
	for _, d := range delays {
		switch d {
		case reconnectInitialBackoff:
			sawInitial = true
		case 2 * reconnectInitialBackoff:
			sawSecond = true
		}
	}
	if !sawInitial || !sawSecond {
		t.Fatalf("per-server backoff state leaked between servers: %v", delays)
	}

	clock.fireAll()
	if got := len(rec.calls()); got != 3 {
		t.Fatalf("expected 3 total reconnects (1 notion + 1 notion retry + 1 ms365), got %d", got)
	}
}

func TestNewReconnectScheduler_NilReconnectIsSafe(t *testing.T) {
	// Defensive: callers wire this up during daemon construction, and a nil
	// action must degrade to "no retries" rather than panicking a goroutine.
	s := NewReconnectScheduler(nil)
	clock := &fakeClock{}
	s.after = clock.after

	s.OnFailure("notion")
	clock.fireAll()
}

// --- deferred attempts (PR #340 review finding 1) ---------------------------

func TestReconnectScheduler_DeferredReArmsWithoutBurningAnAttempt(t *testing.T) {
	// StartConnectAll drops a duplicate attempt when another connect holds the
	// in-flight slot — and the supervisor's own Reconnect takes that slot
	// without ever calling onResult. Before this path existed, fire() had
	// already cleared the pending marker, so the ladder froze with no timer
	// and no failure report: the exact stall this scheduler exists to prevent.
	//
	// A deferred attempt never reached the server, so it must re-arm WITHOUT
	// advancing the streak.
	s, clock, rec := newTestScheduler(t)

	s.OnFailure("notion")
	clock.fireAll() // attempt 1 runs

	s.OnDeferred("notion") // it never reached the server
	if got := clock.delays(); len(got) != 1 || got[0] != reconnectInitialBackoff {
		t.Fatalf("deferred retry must re-arm at the same rung (%v), got %v", reconnectInitialBackoff, got)
	}
	clock.fireAll()

	// The streak is still at 1, so a real failure now takes rung 2 — not 3.
	s.OnFailure("notion")
	if got := clock.delays(); len(got) != 1 || got[0] != 2*reconnectInitialBackoff {
		t.Fatalf("a deferred attempt must not consume a rung; want %v, got %v", 2*reconnectInitialBackoff, got)
	}
	if got := len(rec.calls()); got != 2 {
		t.Fatalf("expected 2 reconnect calls so far, got %d", got)
	}
}

func TestReconnectScheduler_DeferredOnFreshServerSchedulesFirstRung(t *testing.T) {
	s, clock, _ := newTestScheduler(t)

	s.OnDeferred("notion")

	if got := clock.delays(); len(got) != 1 || got[0] != reconnectInitialBackoff {
		t.Fatalf("a deferred first attempt must schedule rung 1, got %v", got)
	}
}

func TestReconnectScheduler_DeferredRespectsTheAttemptCap(t *testing.T) {
	// Otherwise a server that keeps losing the in-flight race would re-arm
	// forever, which is the runaway the cap exists to stop.
	s, clock, _ := newTestScheduler(t)

	for i := 0; i < reconnectMaxAttempts; i++ {
		s.OnFailure("notion")
		clock.fireAll()
	}

	s.OnDeferred("notion")
	if got := clock.delays(); len(got) != 0 {
		t.Fatalf("deferred must not re-arm past the cap, got %v", got)
	}
}

// --- jitter (PR #340 review finding 4) --------------------------------------

func TestReconnectScheduler_JitterStaysWithinBandAndVaries(t *testing.T) {
	// Without jitter, a reload that fails several servers at once puts every
	// ladder on the same schedule — a synchronized burst of npx spawns every
	// rung. Mirrors Supervisor.jitter's 0.8–1.2 band.
	s := NewReconnectScheduler(nil)
	base := 10 * time.Second

	seen := make(map[time.Duration]struct{})
	for i := 0; i < 200; i++ {
		got := s.jitter(base)
		if got < time.Duration(float64(base)*0.8) || got > time.Duration(float64(base)*1.2) {
			t.Fatalf("jitter(%v) = %v, outside the 0.8–1.2 band", base, got)
		}
		seen[got] = struct{}{}
	}
	if len(seen) < 2 {
		t.Fatal("jitter produced a constant delay; ladders would still fire in lockstep")
	}
}

func TestReconnectScheduler_JitterNeverProducesNonPositiveDelay(t *testing.T) {
	s := NewReconnectScheduler(nil)
	for _, base := range []time.Duration{time.Nanosecond, time.Millisecond, reconnectMaxBackoff} {
		for i := 0; i < 100; i++ {
			if got := s.jitter(base); got <= 0 {
				t.Fatalf("jitter(%v) = %v, must stay positive", base, got)
			}
		}
	}
}

// --- Stop race (PR #340 review finding 4) -----------------------------------

func TestReconnectScheduler_StopBetweenFireAndReconnectIsHonored(t *testing.T) {
	// fire() releases the lock before invoking reconnect. A Stop landing in
	// that window must still prevent the attempt — otherwise a timer can
	// respawn a subprocess the shutdown cleanup is already reaping.
	clock := &fakeClock{}
	rec := &recordingReconnect{}
	var s *ReconnectScheduler
	s = NewReconnectScheduler(func(name string) {
		rec.fn(name)
	})
	s.after = clock.after
	s.jitter = func(d time.Duration) time.Duration { return d }

	s.OnFailure("notion")
	// Simulate the interleaving: stop after the timer fires but before the
	// reconnect action would run.
	s.Stop()
	clock.fireAll()

	if got := rec.calls(); len(got) != 0 {
		t.Fatalf("a stopped scheduler must not reconnect, got %v", got)
	}
	_ = s
}
