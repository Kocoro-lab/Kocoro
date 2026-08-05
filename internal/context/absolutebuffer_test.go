package context

import "testing"

// Gap #5: percentage-only thresholds forfeit ~100K usable tokens on the 1M
// families the default tiers route to. The absolute buffer complements the
// fraction — the trigger is whichever keeps MORE window usable — while small
// windows keep the fractional floor untouched.

func TestCompactTargetTokens_AbsoluteBufferWinsOnLargeWindow(t *testing.T) {
	got := compactTargetTokens(1_000_000)
	want := 1_000_000 - compactAbsoluteBufferTokens
	if got != want {
		t.Errorf("1M trigger = %d, want window-buffer %d (fraction-only would waste %d tokens)",
			got, want, want-900_000)
	}
}

func TestCompactTargetTokens_FractionFloorOnSmallWindow(t *testing.T) {
	got := compactTargetTokens(200_000)
	if want := 180_000; got != want {
		t.Errorf("200K trigger = %d, want fractional %d (absolute buffer must not tighten small windows)", got, want)
	}
}

func TestCompactLandingTokens_TracksTriggerWithHysteresis(t *testing.T) {
	for _, w := range []int{120_000, 200_000, 1_000_000} {
		trigger := compactTargetTokens(w)
		landing := compactLandingTokens(w)
		if landing >= trigger {
			t.Errorf("window %d: landing %d must stay below trigger %d (hysteresis band)", w, landing, trigger)
		}
	}
	// On the 1M family the band is two buffers: landing = window - 3*buffer.
	// One buffer proved too tight — the ~50K restoration payload budgets
	// against the trigger and left ~10K of re-trigger slack.
	if got, want := compactLandingTokens(1_000_000), 1_000_000-3*compactAbsoluteBufferTokens; got != want {
		t.Errorf("1M landing = %d, want %d", got, want)
	}
	if got, want := compactLandingTokens(200_000), 160_000; got != want {
		t.Errorf("200K landing = %d, want fractional %d", got, want)
	}
}

func TestShouldCompact_AbsoluteBufferReclaimsWindow(t *testing.T) {
	// 920K on a 1M window crossed the old 90% line but sits under the
	// absolute-buffer trigger — the whole point of gap #5.
	if ShouldCompact(920_000, 0, 1_000_000) {
		t.Error("920K/1M must NOT compact under the absolute-buffer trigger")
	}
	if !ShouldCompact(950_000, 0, 1_000_000) {
		t.Error("950K/1M must compact (over window-buffer)")
	}
	if !ShouldCompact(185_000, 0, 200_000) {
		t.Error("185K/200K must still compact at the fractional floor")
	}
}
