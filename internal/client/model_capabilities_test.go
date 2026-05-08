package client

import "testing"

func TestResolveModelCapabilities(t *testing.T) {
	cases := []struct {
		name          string
		specificModel string
		modelTier     string
		wantWindow    int
	}{
		// 1M-capable models (auto-1M as of 2026-03-13, no beta header).
		{"opus-4-7 specific", "claude-opus-4-7", "", 1_000_000},
		{"opus-4-7 dated", "claude-opus-4-7-20260416", "", 1_000_000},
		{"opus-4-6 specific", "claude-opus-4-6", "", 1_000_000},
		{"opus-4-6 dated", "claude-opus-4-6-20260205", "", 1_000_000},
		{"sonnet-4-6 specific", "claude-sonnet-4-6", "", 1_000_000},
		{"sonnet-4-6 dated", "claude-sonnet-4-6-20260217", "", 1_000_000},

		// 200K-capped models (beta header retired 2026-04-30).
		{"sonnet-4-5", "claude-sonnet-4-5", "", 200_000},
		{"sonnet-4-5 dated", "claude-sonnet-4-5-20250929", "", 200_000},
		{"sonnet-4 legacy", "claude-sonnet-4-20250514", "", 200_000},
		{"haiku-4-5", "claude-haiku-4-5", "", 200_000},
		{"haiku-4-5 dated", "claude-haiku-4-5-20251001", "", 200_000},

		// Tier-only resolution: every recognized tier returns the conservative
		// 200K floor. Cloud's priority/failover chains don't guarantee that
		// tier maps to a 1M-capable model (large priority 1 is gpt-5.1 at
		// 400K; medium failover lands on sonnet-4-5 at 200K). Operators who
		// need the 1M benefit must pin agent.model to a 1M-capable name.
		// "large" is the Shannon Cloud nomenclature; "big" is ShanClaw's —
		// both accepted to avoid surprises when conventions leak across.
		{"big tier (conservative 200K)", "", "big", 200_000},
		{"large tier (Cloud nomenclature)", "", "large", 200_000},
		{"medium tier (conservative 200K)", "", "medium", 200_000},
		{"small tier", "", "small", 200_000},

		// Specific takes precedence over tier (specific path is the only one
		// that can return 1M, so a 200K model + big tier still resolves to 200K).
		{"specific wins over tier", "claude-haiku-4-5", "big", 200_000},

		// Unknown / empty → conservative default.
		{"empty both", "", "", 200_000},
		{"unknown tier", "", "huge", 200_000},
		{"future model", "claude-opus-5-0", "", 200_000},

		// Regression: specific model that lookupSpecificModel does not
		// recognize must NOT fall back to tier. Operator pinned a model
		// we don't know; conservative 200K is the safe default. (See
		// 2026-05-08 review Finding 2 — earlier impl returned 1M here
		// via the tier fallback, defeating the resolver on operator
		// pins of newly-released models that hadn't been added to the
		// prefix table.)
		{"unknown specific + medium tier", "claude-flux-9000", "medium", 200_000},
		{"unknown specific + big tier", "claude-future-1-0", "big", 200_000},
		{"unknown specific + small tier", "claude-future-1-0", "small", 200_000},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveModelCapabilities(tc.specificModel, tc.modelTier)
			if got.ContextWindow != tc.wantWindow {
				t.Errorf("ContextWindow = %d, want %d", got.ContextWindow, tc.wantWindow)
			}
		})
	}
}
