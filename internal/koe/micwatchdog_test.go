package koe

import (
	"testing"
	"time"
)

func TestMicSilenceState(t *testing.T) {
	const floor = 0.004
	const window = 10 * time.Second
	base := time.Unix(0, 0)

	t.Run("sub-floor for the whole window warns exactly once", func(t *testing.T) {
		var m MicSilenceState
		// t=0: silence starts, no warning yet.
		if got := m.Observe(base, true, 0, floor, window); got != MicSilenceNone {
			t.Fatalf("t0: got %v, want None", got)
		}
		// t=9s: still within window.
		if got := m.Observe(base.Add(9*time.Second), true, 0.001, floor, window); got != MicSilenceNone {
			t.Fatalf("t9: got %v, want None", got)
		}
		// t=10s: window elapsed → warn once.
		if got := m.Observe(base.Add(10*time.Second), true, 0, floor, window); got != MicSilenceSilent {
			t.Fatalf("t10: got %v, want Silent", got)
		}
		// t=12s: still silent, must NOT warn again.
		if got := m.Observe(base.Add(12*time.Second), true, 0, floor, window); got != MicSilenceNone {
			t.Fatalf("t12: got %v, want None (already warned)", got)
		}
	})

	t.Run("above-floor input resets the timer", func(t *testing.T) {
		var m MicSilenceState
		m.Observe(base, true, 0, floor, window)                                   // silence starts
		m.Observe(base.Add(5*time.Second), true, 0.2, floor, window)              // user speaks → reset
		if got := m.Observe(base.Add(14*time.Second), true, 0, floor, window); got != MicSilenceNone {
			t.Fatalf("got %v, want None — timer should have reset at t5", got)
		}
	})

	t.Run("recovery after a warning emits Recovered once", func(t *testing.T) {
		var m MicSilenceState
		m.Observe(base, true, 0, floor, window)
		if got := m.Observe(base.Add(window), true, 0, floor, window); got != MicSilenceSilent {
			t.Fatalf("want Silent, got %v", got)
		}
		if got := m.Observe(base.Add(window+time.Second), true, 0.3, floor, window); got != MicSilenceRecovered {
			t.Fatalf("want Recovered, got %v", got)
		}
		// A second above-floor sample must not re-emit Recovered.
		if got := m.Observe(base.Add(window+2*time.Second), true, 0.3, floor, window); got != MicSilenceNone {
			t.Fatalf("want None, got %v", got)
		}
	})

	t.Run("not-capturing holds the timer reset (muted mic never warns)", func(t *testing.T) {
		var m MicSilenceState
		// Mic muted/gated the entire time → never warn even though level is 0.
		for s := 0; s <= 20; s++ {
			if got := m.Observe(base.Add(time.Duration(s)*time.Second), false, 0, floor, window); got != MicSilenceNone {
				t.Fatalf("t%d: got %v, want None while not capturing", s, got)
			}
		}
	})

	t.Run("suppression mid-silence restarts the window", func(t *testing.T) {
		var m MicSilenceState
		m.Observe(base, true, 0, floor, window)                        // silence starts at t0
		m.Observe(base.Add(8*time.Second), false, 0, floor, window)    // Kocoro speaks → reset
		// Capture resumes at t9; window must restart from t9, so t18 is still fine.
		m.Observe(base.Add(9*time.Second), true, 0, floor, window)
		if got := m.Observe(base.Add(18*time.Second), true, 0, floor, window); got != MicSilenceNone {
			t.Fatalf("got %v, want None — window should restart after suppression", got)
		}
		// t19 = 10s after t9 → warn.
		if got := m.Observe(base.Add(19*time.Second), true, 0, floor, window); got != MicSilenceSilent {
			t.Fatalf("got %v, want Silent at t19", got)
		}
	})
}
