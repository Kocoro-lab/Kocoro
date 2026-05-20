package agent

import (
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/prompt"
)

// TestAssembleUserMessage_InstructionsOnlyEmitsCacheBreak is the end-to-end
// guard for the instructions-in-StableContext move: when only shared
// instructions are present (no sticky facts), the assembled user message must
// still emit the <!-- cache_break --> marker with instructions sitting in the
// cacheable prefix. This is what locks in the "caching win" contract; a unit
// test on buildStableContext alone would pass even if assembleUserMessage
// regressed to skip the marker.
func TestAssembleUserMessage_InstructionsOnlyEmitsCacheBreak(t *testing.T) {
	parts := prompt.BuildSystemPrompt(prompt.PromptOptions{
		BasePrompt:   "You are Shannon.",
		Instructions: "Never push to main without review.",
	})

	result := assembleUserMessage(parts, "ship the release")

	idx := strings.Index(result, "<!-- cache_break -->")
	if idx < 0 {
		t.Fatalf("expected cache_break marker, got:\n%s", result)
	}

	prefix := result[:idx]
	suffix := result[idx:]

	if !strings.Contains(prefix, "<user_instructions>") {
		t.Error("Instructions wrapper <user_instructions> should be in the cached prefix (before cache_break)")
	}
	if !strings.Contains(prefix, "Never push to main without review.") {
		t.Error("instructions body should be in the cached prefix")
	}
	if strings.Contains(suffix, "Never push to main without review.") {
		t.Error("instructions body must not appear after cache_break")
	}
	if !strings.HasSuffix(result, "ship the release") {
		t.Error("raw user message should be at the end")
	}
}

func TestAssembleUserMessage_CacheBreakRegression(t *testing.T) {
	t.Run("empty stable omits marker", func(t *testing.T) {
		result := assembleUserMessage(prompt.PromptParts{
			StableContext:  "",
			VolatileContext: "current date: 2026-04-03",
		}, "hello")
		if strings.Contains(result, "cache_break") {
			t.Error("cache_break should not appear when StableContext is empty")
		}
	})

	t.Run("non-empty stable includes marker", func(t *testing.T) {
		result := assembleUserMessage(prompt.PromptParts{
			StableContext:  "system instructions",
			VolatileContext: "current date: 2026-04-03",
		}, "hello")
		if !strings.Contains(result, "cache_break") {
			t.Error("cache_break should appear when StableContext is non-empty")
		}
	})

	t.Run("marker separates stable from volatile", func(t *testing.T) {
		result := assembleUserMessage(prompt.PromptParts{
			StableContext:  "stable-prefix",
			VolatileContext: "volatile-suffix",
		}, "user-query")

		idx := strings.Index(result, "<!-- cache_break -->")
		if idx < 0 {
			t.Fatal("marker not found")
		}
		if !strings.Contains(result[:idx], "stable-prefix") {
			t.Error("stable content should be before marker")
		}
		if !strings.Contains(result[idx:], "volatile-suffix") {
			t.Error("volatile content should be after marker")
		}
		if !strings.HasSuffix(result, "user-query") {
			t.Error("user message should be at the end")
		}
	})
}

func TestAssembleUserMessage_SessionPlaceholderEmitsCacheBreak(t *testing.T) {
	parts := prompt.PromptParts{
		System:          "static",
		StableContext:   "## Session\nActive agent context.",
		VolatileContext: "## Context\nDate: 2026-04-14",
	}
	msg := assembleUserMessage(parts, "hello")
	if !strings.Contains(msg, "<!-- cache_break -->") {
		t.Fatalf("cache_break marker missing when only session placeholder present")
	}
	if !strings.Contains(msg, "Active agent context.") {
		t.Fatalf("session placeholder not preserved: %q", msg)
	}
}

// TestAppendDynamicUserBlocks_LanguageAfterSkillListing is the load-bearing
// regression guard for issue #157. Skill listings carry multilingual trigger
// keywords (e.g. "日:一覧/表示/...") which previously sat at the absolute end
// of the user message and pulled response language toward Japanese under
// recency bias. The Language directive MUST be appended after the skill
// listing so it remains the last system block before the model generates.
//
// Without this test, a refactor that reorders the two appends inside
// AgentLoop.Run silently reintroduces the bug — all other tests still pass.
func TestAppendDynamicUserBlocks_LanguageAfterSkillListing(t *testing.T) {
	// Realistic skill listing — wrapped in <system-reminder> by the real
	// buildSkillListing, with a kocoro entry carrying the same multilingual
	// trigger keywords that originally caused the drift.
	skillListing := "<system-reminder>\n## Available Skills\n" +
		"- kocoro: Inspect Kocoro platform state. " +
		"中:列出/查看/查询/创建/修改 agent/skill/MCP/计划. " +
		"日:一覧/表示/確認/検索/作成 agent/skill/MCP/スケジュール.\n" +
		"</system-reminder>"
	langDirective := prompt.LanguageDirective()

	out := appendDynamicUserBlocks("user-query", skillListing, langDirective)

	skillIdx := strings.Index(out, "## Available Skills")
	langIdx := strings.Index(out, "## Language")
	jpIdx := strings.Index(out, "日:一覧")

	if skillIdx < 0 || langIdx < 0 || jpIdx < 0 {
		t.Fatalf("expected all markers present; got skill=%d lang=%d jp=%d in:\n%s",
			skillIdx, langIdx, jpIdx, out)
	}
	if langIdx <= skillIdx {
		t.Errorf("issue #157: ## Language at idx=%d must come AFTER ## Available Skills at idx=%d. "+
			"Otherwise multilingual skill trigger keywords (e.g. 日:一覧) become the recency anchor "+
			"and short English prompts get answered in Japanese.",
			langIdx, skillIdx)
	}
	if langIdx <= jpIdx {
		t.Errorf("issue #157: ## Language at idx=%d must come AFTER the Japanese skill trigger "+
			"keywords at idx=%d for the same reason", langIdx, jpIdx)
	}
}

// TestAppendDynamicUserBlocks_OmittedBlocksAreNoOp asserts the helper is a
// no-op for empty inputs — AgentLoop.Run relies on this so it can pass `""`
// for skill listing when no new skills are present without branching.
func TestAppendDynamicUserBlocks_OmittedBlocksAreNoOp(t *testing.T) {
	if got := appendDynamicUserBlocks("user-query", "", ""); got != "user-query" {
		t.Errorf("expected unchanged input when both blocks empty, got: %q", got)
	}
	if got := appendDynamicUserBlocks("user-query", "", "## Language\nfoo"); !strings.HasSuffix(got, "## Language\nfoo") {
		t.Errorf("expected Language appended when no skill listing, got: %q", got)
	}
	if got := appendDynamicUserBlocks("user-query", "skills", ""); !strings.HasSuffix(got, "skills") {
		t.Errorf("expected skill listing appended when no language directive, got: %q", got)
	}
}
