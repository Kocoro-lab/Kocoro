package agent

import "testing"

func TestMaxTokensForModel(t *testing.T) {
	cases := []struct {
		name  string
		model string
		want  int
	}{
		// Known 1M-context family — expect 64K
		{"sonnet 4.6 dateless", "claude-sonnet-4-6", 64_000},
		{"opus 4.6 dateless", "claude-opus-4-6", 64_000},
		{"opus 4.7 dateless", "claude-opus-4-7", 64_000},

		// Known 200K dated forms
		{"sonnet 4.5 dated", "claude-sonnet-4-5-20250929", 64_000},
		{"haiku 4.5 dated", "claude-haiku-4-5-20251001", 64_000},
		{"opus 4.5 dated", "claude-opus-4-5-20251101", 64_000},
		{"sonnet 4 dated", "claude-sonnet-4-20250514", 64_000},

		// 4.1 family caps lower
		{"opus 4.1 dated", "claude-opus-4-1-20250805", 32_000},
		{"opus 4 dated", "claude-opus-4-20250514", 32_000},

		// Dateless floating tags
		{"sonnet 4.5 floating", "claude-sonnet-4-5", 64_000},
		{"haiku 4.5 floating", "claude-haiku-4-5", 64_000},
		{"opus 4.5 floating", "claude-opus-4-5", 64_000},
		{"opus 4.1 floating", "claude-opus-4-1", 32_000},

		// GPT-5 / GPT-4.1
		{"gpt-5.1", "gpt-5.1", 128_000},
		{"gpt-5 mini", "gpt-5-mini-2025-08-07", 128_000},
		{"gpt-4.1", "gpt-4.1-2025-04-14", 32_000},

		// Forward-compat: future dated variant of a known dateless family
		// (e.g. a hypothetical "claude-sonnet-4-6-20260601") should match
		// via prefix lookup and inherit the family's cap.
		{"sonnet 4.6 future dated", "claude-sonnet-4-6-20260601", 64_000},
		{"opus 4.7 future dated", "claude-opus-4-7-20270101", 64_000},

		// Unknown model → fallback default. Critically, an unknown numeric
		// suffix on a known family (e.g. "claude-sonnet-4-60") must NOT
		// silently inherit 4-6's cap — it has to fall back to default so
		// callers don't ship a wrong assumption to the wire.
		{"unknown model", "some-unknown-model", defaultMaxOutputTokens},
		{"ollama model", "qwen3:8b", defaultMaxOutputTokens},
		{"sonnet numeric suffix is NOT a dated variant",
			"claude-sonnet-4-60", defaultMaxOutputTokens},

		// Empty string — never panic, return default
		{"empty string", "", defaultMaxOutputTokens},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MaxTokensForModel(tc.model)
			if got != tc.want {
				t.Errorf("MaxTokensForModel(%q) = %d, want %d", tc.model, got, tc.want)
			}
		})
	}
}

func TestMaxTokensPrefixOrderIsLongestFirst(t *testing.T) {
	// Sanity check: init() must have sorted prefixes by length descending so
	// "claude-sonnet-4-6-" beats hypothetical broader prefixes like
	// "claude-sonnet-4-". Without this, a future-dated 4-6 model could
	// silently match the wrong family.
	if len(maxTokensPrefixOrder) == 0 {
		t.Fatal("maxTokensPrefixOrder is empty — init() did not run")
	}
	for i := 1; i < len(maxTokensPrefixOrder); i++ {
		if len(maxTokensPrefixOrder[i]) > len(maxTokensPrefixOrder[i-1]) {
			t.Errorf("prefix order not longest-first at index %d: %q (%d) after %q (%d)",
				i, maxTokensPrefixOrder[i], len(maxTokensPrefixOrder[i]),
				maxTokensPrefixOrder[i-1], len(maxTokensPrefixOrder[i-1]))
		}
	}
}
