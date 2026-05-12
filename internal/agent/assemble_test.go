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
