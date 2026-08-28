package koe

import (
	"testing"
	"time"
)

// The 91% case: a missing billing principal is a SHARED prerequisite, not a
// provider outage. Retrying on a timer cannot fix it, and measured over three
// days it produced 27,290 requests that could never succeed.
func TestWarmRetryPausesOnSharedPrerequisite(t *testing.T) {
	for _, code := range []string{CallFailureAccountRequired, CallFailureQuotaExceeded} {
		var p WarmRetryPolicy
		delay, retry := p.OnFailure(code)
		if retry {
			t.Fatalf("%s: retry = true, want paused", code)
		}
		if delay != 0 {
			t.Fatalf("%s: delay = %v, want 0 when paused", code, delay)
		}
		paused, why := p.Paused()
		if !paused || why != code {
			t.Fatalf("%s: Paused() = (%v, %q), want (true, %q)", code, paused, why, code)
		}
	}
}

// Everything plausibly transient must keep retrying: pausing on a network blip
// would turn a hiccup into "voice is dead until you press the button".
func TestWarmRetryBacksOffOnTransientCauses(t *testing.T) {
	for _, code := range []string{
		CallFailureNetwork, CallFailureBackendUnavailable,
		CallFailureAudio, CallFailureUnknown,
	} {
		var p WarmRetryPolicy
		delay, retry := p.OnFailure(code)
		if !retry {
			t.Fatalf("%s: retry = false, want a retry", code)
		}
		if delay != 5*time.Second {
			t.Fatalf("%s: first delay = %v, want 5s (unchanged from before backoff)", code, delay)
		}
		if paused, _ := p.Paused(); paused {
			t.Fatalf("%s: must not pause", code)
		}
	}
}

// The exact curve, so a future edit cannot quietly make it aggressive again.
// The first step is still 5s on purpose: one transient failure recovers exactly
// as fast as it did before this policy existed.
func TestWarmRetryBackoffCurveAndCap(t *testing.T) {
	var p WarmRetryPolicy
	want := []time.Duration{
		5 * time.Second, 10 * time.Second, 20 * time.Second,
		40 * time.Second, 60 * time.Second,
		60 * time.Second, 60 * time.Second, // capped, not growing
	}
	for i, w := range want {
		delay, retry := p.OnFailure(CallFailureNetwork)
		if !retry {
			t.Fatalf("failure %d: retry = false", i+1)
		}
		if delay != w {
			t.Fatalf("failure %d: delay = %v, want %v", i+1, delay, w)
		}
	}
	if p.Streak() != len(want) {
		t.Fatalf("Streak() = %d, want %d", p.Streak(), len(want))
	}
}

func TestWarmRetrySuccessClearsStreakAndPause(t *testing.T) {
	var p WarmRetryPolicy
	p.OnFailure(CallFailureNetwork)
	p.OnFailure(CallFailureNetwork)
	p.OnFailure(CallFailureAccountRequired)
	if paused, _ := p.Paused(); !paused {
		t.Fatal("expected paused before success")
	}

	p.OnSuccess()
	if paused, why := p.Paused(); paused || why != "" {
		t.Fatalf("Paused() = (%v, %q) after success, want (false, \"\")", paused, why)
	}
	if p.Streak() != 0 {
		t.Fatalf("Streak() = %d after success, want 0", p.Streak())
	}
	if delay, _ := p.OnFailure(CallFailureNetwork); delay != 5*time.Second {
		t.Fatalf("delay after success = %v, want the curve to restart at 5s", delay)
	}
}

// The escape hatch that makes pausing safe: pressing the call button attempts
// immediately instead of inheriting a 60s backoff.
func TestWarmRetryUserRequestResetsTheSchedule(t *testing.T) {
	var p WarmRetryPolicy
	for i := 0; i < 6; i++ {
		p.OnFailure(CallFailureNetwork)
	}
	p.UserRequestedCall()
	if p.Streak() != 0 {
		t.Fatalf("Streak() = %d after a user request, want 0", p.Streak())
	}
	if delay, _ := p.OnFailure(CallFailureNetwork); delay != 5*time.Second {
		t.Fatalf("delay = %v, want the curve to restart at 5s", delay)
	}
}

// A user request must NOT clear the pause by itself. Only a real success does —
// otherwise a still-broken prerequisite restarts the storm on every button
// press, which is the failure mode this whole change removes.
func TestWarmRetryUserRequestDoesNotClearPause(t *testing.T) {
	var p WarmRetryPolicy
	p.OnFailure(CallFailureAccountRequired)
	p.UserRequestedCall()
	if paused, _ := p.Paused(); !paused {
		t.Fatal("a user request must not clear the pause; only success may")
	}
	// The attempt it triggers fails the same way → still paused, no schedule.
	if delay, retry := p.OnFailure(CallFailureAccountRequired); retry || delay != 0 {
		t.Fatalf("OnFailure after user request = (%v, %v), want (0, false)", delay, retry)
	}
}
