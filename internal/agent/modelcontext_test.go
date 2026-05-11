package agent

import "testing"

func TestLookupModelContextWindow(t *testing.T) {
	cases := []struct {
		name     string
		model    string
		wantCW   int
		wantOK   bool
	}{
		{"empty", "", 0, false},
		{"unknown", "claude-future-99", 0, false},
		{"sonnet 4.6 dateless 1M", "claude-sonnet-4-6", 1_000_000, true},
		{"opus 4.6 dateless 1M", "claude-opus-4-6", 1_000_000, true},
		{"opus 4.7 dateless 1M", "claude-opus-4-7", 1_000_000, true},
		{"sonnet 4.5 dated 200K", "claude-sonnet-4-5-20250929", 200_000, true},
		{"haiku 4.5 dated 200K", "claude-haiku-4-5-20251001", 200_000, true},
		{"sonnet 4 deprecated 200K", "claude-sonnet-4-20250514", 200_000, true},
		{"future dated sonnet 4.6 via prefix", "claude-sonnet-4-6-20260301", 1_000_000, true},
		{"future dated opus 4.7 via prefix", "claude-opus-4-7-20260601", 1_000_000, true},
		// 200K prefix coverage — guards against the "cloud failover from
		// sonnet-4-6 (1M) to a dateless/future-dated 200K model leaves
		// the loop at 1M and 400s on next prompt" failure mode that
		// PR review item #3 caught.
		{"dateless sonnet 4.5 via prefix", "claude-sonnet-4-5", 200_000, true},
		{"dateless haiku 4.5 via prefix", "claude-haiku-4-5", 200_000, true},
		{"dateless opus 4.5 via prefix", "claude-opus-4-5", 200_000, true},
		{"dateless opus 4.1 via prefix", "claude-opus-4-1", 200_000, true},
		{"future dated haiku 4.5 via prefix", "claude-haiku-4-5-20260601", 200_000, true},
		{"future dated sonnet 4.5 via prefix", "claude-sonnet-4-5-20260101", 200_000, true},
		// Hypothetical future "claude-sonnet-4-NN" models with no specific
		// prefix entry must NOT silently down-cap to 200K — we don't know
		// their actual cap, so (0,false) → caller keeps existing window
		// is the safer default. (Mirror of the "numeric suffix is NOT a
		// dated variant" guard for the 200K side.)
		{"unknown sonnet 4 sibling", "claude-sonnet-4-anyfuture", 0, false},
		// Longest-prefix-first invariant: with multiple matching prefixes
		// (e.g. if we later added a broader fallback), the longest one
		// must win. claude-sonnet-4-6-20270101 must match the 1M family
		// prefix, never a hypothetical broader 200K prefix. (PR review
		// item #3 — current table has no such overlap, but the assertion
		// guards future additions.)
		{"longest-prefix-first guards future overlap", "claude-sonnet-4-6-20270101", 1_000_000, true},
		{"gpt-5.1 400K", "gpt-5.1", 400_000, true},
		{"gpt-4.1 dated 128K", "gpt-4.1-2025-04-14", 128_000, true},
		{"gemini 2.5 pro 1M+", "gemini-2.5-pro", 1_048_576, true},
		{"grok 2M", "grok-4-1-fast-reasoning", 2_000_000, true},
		// Guard against accidental prefix collisions.
		{"sonnet 4.6 numeric suffix is NOT a dated variant", "claude-sonnet-4-60", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cw, ok := LookupModelContextWindow(tc.model)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want=%v", ok, tc.wantOK)
			}
			if cw != tc.wantCW {
				t.Fatalf("cw=%d want=%d", cw, tc.wantCW)
			}
		})
	}
}

func TestMaybeAutoAdjustContextWindow(t *testing.T) {
	t.Run("unlocked: known model bumps window", func(t *testing.T) {
		a := &AgentLoop{contextWindow: 200_000}
		a.maybeAutoAdjustContextWindow("claude-sonnet-4-6")
		if a.contextWindow != 1_000_000 {
			t.Fatalf("expected auto-bump to 1M, got %d", a.contextWindow)
		}
	})

	t.Run("locked: known model is ignored", func(t *testing.T) {
		a := &AgentLoop{contextWindow: 200_000}
		a.SetContextWindowExplicit(200_000)
		a.maybeAutoAdjustContextWindow("claude-sonnet-4-6")
		if a.contextWindow != 200_000 {
			t.Fatalf("explicit lock violated: got %d", a.contextWindow)
		}
	})

	t.Run("unlocked: unknown model leaves window untouched", func(t *testing.T) {
		a := &AgentLoop{contextWindow: 200_000}
		a.maybeAutoAdjustContextWindow("totally-made-up-model")
		if a.contextWindow != 200_000 {
			t.Fatalf("unknown should not change window: got %d", a.contextWindow)
		}
	})

	t.Run("unlocked: empty model name is no-op", func(t *testing.T) {
		a := &AgentLoop{contextWindow: 200_000}
		a.maybeAutoAdjustContextWindow("")
		if a.contextWindow != 200_000 {
			t.Fatalf("empty model should be no-op: got %d", a.contextWindow)
		}
	})

	t.Run("unlocked: same value is no-op", func(t *testing.T) {
		a := &AgentLoop{contextWindow: 1_000_000}
		a.maybeAutoAdjustContextWindow("claude-sonnet-4-6")
		if a.contextWindow != 1_000_000 {
			t.Fatalf("same value churn: got %d", a.contextWindow)
		}
	})

	t.Run("unlocked: switch from large to small model shrinks window", func(t *testing.T) {
		a := &AgentLoop{contextWindow: 1_000_000}
		a.maybeAutoAdjustContextWindow("claude-haiku-4-5-20251001")
		if a.contextWindow != 200_000 {
			t.Fatalf("expected shrink to 200K, got %d", a.contextWindow)
		}
	})

	// SetContextWindow must clear the explicit-lock flag so it can serve
	// as a "reset to seed" path in future refactors. (PR review #4.)
	t.Run("SetContextWindow clears explicit-lock flag", func(t *testing.T) {
		a := &AgentLoop{}
		a.SetContextWindowExplicit(50_000) // user pin
		if !a.contextWindowExplicit {
			t.Fatalf("test setup invariant: explicit lock should be set")
		}
		a.SetContextWindow(200_000) // soft reseed
		if a.contextWindowExplicit {
			t.Fatalf("SetContextWindow should clear explicit lock; still locked")
		}
		// Auto-detect must now flow through to the loop.
		a.maybeAutoAdjustContextWindow("claude-sonnet-4-6")
		if a.contextWindow != 1_000_000 {
			t.Fatalf("expected auto-detect to flow after SetContextWindow reset, got %d", a.contextWindow)
		}
	})
}

// TestSeedContextWindowFromModels guards the precedence used by daemon
// runner / one-shot CLI / TUI when seeding a fresh AgentLoop. Critical
// because daemon RunAgent builds a new loop on every routed turn — if
// the seed is wrong, preflight compaction uses the wrong cap until the
// first response triggers maybeAutoAdjustContextWindow.
func TestSeedContextWindowFromModels(t *testing.T) {
	cases := []struct {
		name             string
		configuredModel  string
		lastSeenModel    string
		fallback         int
		want             int
	}{
		{
			name:            "configured 1M wins",
			configuredModel: "claude-sonnet-4-6",
			lastSeenModel:   "claude-haiku-4-5",
			fallback:        128_000,
			want:            1_000_000,
		},
		{
			name:            "configured 200K wins over fallback default",
			configuredModel: "claude-opus-4-5",
			lastSeenModel:   "",
			fallback:        128_000,
			want:            200_000,
		},
		{
			name:            "last-seen used when no configured",
			configuredModel: "",
			lastSeenModel:   "claude-sonnet-4-6",
			fallback:        200_000,
			want:            1_000_000,
		},
		{
			name:            "last-seen dated variant via prefix",
			configuredModel: "",
			lastSeenModel:   "claude-sonnet-4-6-20260301",
			fallback:        200_000,
			want:            1_000_000,
		},
		{
			name:            "unknown configured falls through to last-seen",
			configuredModel: "totally-made-up",
			lastSeenModel:   "claude-sonnet-4-6",
			fallback:        200_000,
			want:            1_000_000,
		},
		{
			name:            "unknown configured and unknown last-seen use fallback",
			configuredModel: "totally-made-up",
			lastSeenModel:   "another-fake-id",
			fallback:        128_000,
			want:            128_000,
		},
		{
			name:            "both empty uses fallback",
			configuredModel: "",
			lastSeenModel:   "",
			fallback:        200_000,
			want:            200_000,
		},
		{
			name:            "configured beats last-seen even when last-seen is larger",
			configuredModel: "claude-haiku-4-5",
			lastSeenModel:   "claude-sonnet-4-6",
			fallback:        128_000,
			want:            200_000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SeedContextWindowFromModels(tc.configuredModel, tc.lastSeenModel, tc.fallback)
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}
