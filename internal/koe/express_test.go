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

func TestExpressIntents_IsSmallEnum(t *testing.T) {
	// §19: the enum must stay small (realtime 16k tool budget); ~12 intents.
	if n := len(ExpressIntents()); n == 0 || n > 16 {
		t.Errorf("expected a small express enum (~12), got %d", n)
	}
}
