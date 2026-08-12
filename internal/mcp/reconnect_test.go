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
