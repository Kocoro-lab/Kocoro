package koe

import (
	"testing"
	"time"
)

func fixedClock(t *time.Time) func() time.Time { return func() time.Time { return *t } }

func TestExpressGate_InvalidIntentSkipped(t *testing.T) {
	now := time.Unix(1000, 0)
	g := NewExpressGate(ActivityStandard, WithClock(fixedClock(&now)))
	_, ok, reason := g.Allow("banana")
	if ok || reason != "invalid_intent" {
		t.Errorf("invalid intent: ok=%v reason=%q", ok, reason)
	}
}

func TestExplicitDanceRequest(t *testing.T) {
	tests := []struct {
		transcript string
		want       bool
	}{
		{"请跳个舞。", true},
		{"Kocoro，给我跳舞吧！", true},
		{"Please dance for me.", true},
		{"Can you dance for me?", true},
		{"Could you dance for us, please?", true},
		{"踊ってみて。", true},
		{"ダンスして！", true},
		{"你会跳舞吗？", false},
		{"什么是跳舞？", false},
		{"Can you dance?", false},
		{"Please explain dance for me.", false},
		{"不要给我跳舞。", false},
		{"Please don't dance.", false},
		{"来首适合跳舞的音乐。", false},
	}
	for _, tc := range tests {
		if got := explicitDanceRequest(tc.transcript); got != tc.want {
			t.Errorf("explicitDanceRequest(%q) = %t, want %t", tc.transcript, got, tc.want)
		}
	}
}

func TestExpressGate_QuietTierAlwaysSkips(t *testing.T) {
	now := time.Unix(1000, 0)
	g := NewExpressGate(ActivityQuiet, WithClock(fixedClock(&now)))
	if _, ok, reason := g.Allow("happy"); ok || reason != "quiet" {
		t.Errorf("quiet: ok=%v reason=%q (model lane must be off)", ok, reason)
	}
}

func TestExpressGate_FiresAtMostOncePerResponse(t *testing.T) {
	now := time.Unix(1000, 0)
	g := NewExpressGate(ActivityStandard, WithClock(fixedClock(&now)))
	clip, ok, _ := g.Allow("happy")
	if !ok || clip == "" {
		t.Fatalf("first express should fire, got ok=%v clip=%q", ok, clip)
	}
	if _, ok, reason := g.Allow("surprised"); ok || reason != "over_budget" {
		t.Errorf("second express in one response must skip: ok=%v reason=%q", ok, reason)
	}
	// A new response + enough time later fires again.
	g.NewResponse()
	now = now.Add(30 * time.Second)
	if _, ok, _ := g.Allow("surprised"); !ok {
		t.Error("after NewResponse + cooldown, express should fire again")
	}
}

func TestExpressGate_Cooldown(t *testing.T) {
	now := time.Unix(1000, 0)
	g := NewExpressGate(ActivityStandard, WithClock(fixedClock(&now)))
	if _, ok, _ := g.Allow("happy"); !ok {
		t.Fatal("first fire")
	}
	g.NewResponse()
	now = now.Add(5 * time.Second) // < standard cooldown
	if _, ok, reason := g.Allow("happy"); ok || reason != "cooldown" {
		t.Errorf("within cooldown must skip: ok=%v reason=%q", ok, reason)
	}
	g.NewResponse()
	now = now.Add(20 * time.Second) // now well past cooldown
	if _, ok, _ := g.Allow("happy"); !ok {
		t.Error("past cooldown should fire")
	}
}

func TestExpressGate_LivelyHalvesCooldown(t *testing.T) {
	now := time.Unix(1000, 0)
	g := NewExpressGate(ActivityLively, WithClock(fixedClock(&now)))
	if _, ok, _ := g.Allow("happy"); !ok {
		t.Fatal("first fire")
	}
	g.NewResponse()
	now = now.Add(10 * time.Second) // > lively cooldown (~9s), < standard (~18s)
	if _, ok, _ := g.Allow("happy"); !ok {
		t.Error("lively cooldown should let this fire by 10s")
	}
}

func TestExpressGate_VariantAvoidsRepeat(t *testing.T) {
	now := time.Unix(1000, 0)
	// "sad" has >1 clip. A picker that always returns 0 would repeat without the
	// avoid-last logic; assert consecutive fires differ.
	g := NewExpressGate(ActivityStandard, WithClock(fixedClock(&now)), WithPicker(func(n int) int { return 0 }))
	first, ok, _ := g.Allow("sad")
	if !ok {
		t.Fatal("first sad")
	}
	g.NewResponse()
	now = now.Add(30 * time.Second)
	second, ok, _ := g.Allow("sad")
	if !ok {
		t.Fatal("second sad")
	}
	if first == second {
		t.Errorf("consecutive same-intent clips should differ (got %q twice)", first)
	}
}

func TestExpressGate_AllKnownIntentsResolve(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, intent := range ExpressIntents() {
		g := NewExpressGate(ActivityStandard, WithClock(fixedClock(&now)))
		clip, ok, reason := g.Allow(intent)
		if !ok || clip == "" {
			t.Errorf("intent %q should resolve to a clip: ok=%v clip=%q reason=%q", intent, ok, clip, reason)
		}
	}
}

func TestExpressGate_SetAvailableMovesFiltersToExposedClips(t *testing.T) {
	now := time.Unix(1000, 0)
	g := NewExpressGate(ActivityStandard, WithClock(fixedClock(&now)))
	// The bridge exposes only a subset of the "happy" pool
	// (cheerful1/laughing1/laughing2/loving1/relief1).
	g.SetAvailableMoves([]string{"laughing1", "loving1", "curious1"})
	allowed := map[string]bool{"laughing1": true, "loving1": true}
	for i := 0; i < 8; i++ {
		g.NewResponse()
		now = now.Add(30 * time.Second)
		clip, ok, _ := g.Allow("happy")
		if !ok {
			t.Fatalf("happy should fire (iter %d)", i)
		}
		if !allowed[clip] {
			t.Errorf("clip %q is not in the exposed subset — a missing clip would make the bridge reject it (unknown_move)", clip)
		}
	}
}

func TestExpressGate_SetAvailableMovesDropsFullyMissingIntent(t *testing.T) {
	now := time.Unix(1000, 0)
	g := NewExpressGate(ActivityStandard, WithClock(fixedClock(&now)))
	// Expose NONE of the "sad" pool (sad1/sad2/downcast1/lonely1) but a "happy" clip.
	g.SetAvailableMoves([]string{"laughing1", "curious1"})
	if _, ok, reason := g.Allow("sad"); ok || reason != "invalid_intent" {
		t.Errorf("an intent whose entire pool is missing must skip as invalid_intent: ok=%v reason=%q", ok, reason)
	}
	g.NewResponse()
	now = now.Add(30 * time.Second)
	if _, ok, _ := g.Allow("happy"); !ok {
		t.Error("happy should still fire (laughing1 is exposed)")
	}
}

func TestExpressGate_SetAvailableMovesEmptyKeepsFullMapping(t *testing.T) {
	// A disconnected/never-filtered gate (no bridge moves yet) must behave as the
	// unfiltered gate — an empty move set means "unknown", not "nothing allowed".
	now := time.Unix(1000, 0)
	g := NewExpressGate(ActivityStandard, WithClock(fixedClock(&now)))
	g.SetAvailableMoves(nil)
	for _, intent := range ExpressIntents() {
		g.NewResponse()
		now = now.Add(30 * time.Second)
		if _, ok, _ := g.Allow(intent); !ok {
			t.Errorf("intent %q should still resolve when the move set is empty (unfiltered)", intent)
		}
	}
}

func TestExpressIntents_IsSmallEnum(t *testing.T) {
	// §19: the enum must stay small (realtime 16k tool budget); ~12 intents.
	if n := len(ExpressIntents()); n == 0 || n > 16 {
		t.Errorf("expected a small express enum (~12), got %d", n)
	}
}
