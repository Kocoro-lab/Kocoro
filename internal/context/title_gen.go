package context

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// maxTitleRunes caps the final title. Workload: a session-list row in Desktop /
// TUI. Symptom if it binds: a trailing "...". Mirrors Cloud's 60-rune cap
// (shannon-cloud .../activities/session_title.go); not user-tunable.
const maxTitleRunes = 60

// maxTitleInputRunes tail-caps the transcript sent to the title model —
// recent turns matter most (mirrors Claude Code's 1000-char window).
// Workload: a single chat's first few turns. Symptom if it binds: a long
// early conversation has its opening dropped from the title input, so the
// title reflects only the most recent ~1000 runes. Not user-tunable (const).
const maxTitleInputRunes = 1000

// titleSystemPrompt is adapted from Cloud's title_generator. Language is
// inferred from the input — do NOT hardcode English.
const titleSystemPrompt = `You are a multilingual title generator. Generate a concise, descriptive title for a chat session in the SAME LANGUAGE as the user's input. For CJK languages (Chinese/Japanese/Korean) use 5-15 characters. For English use 3-5 words in Title Case. No quotes, no trailing punctuation, no emojis. Output ONLY the title, nothing else.`

const titleUserPromptPrefix = "Generate a chat session title from this conversation. Use the SAME LANGUAGE as the user. Output ONLY the title:\n\n"

// GenerateTitle calls the small-tier LLM to produce a one-line session title.
// Returns the sanitized title or an error; on error callers keep the placeholder.
func GenerateTitle(ctx context.Context, c Completer, messages []client.Message) (string, error) {
	transcript := buildTitleTranscript(messages)
	if strings.TrimSpace(transcript) == "" {
		return "", fmt.Errorf("empty transcript")
	}
	resp, err := c.Complete(ctx, client.CompletionRequest{
		Messages: []client.Message{
			{Role: "system", Content: client.NewTextContent(titleSystemPrompt)},
			{Role: "user", Content: client.NewTextContent(titleUserPromptPrefix + transcript)},
		},
		ModelTier:   "small",
		Temperature: 0.3,
		MaxTokens:   64,
		CacheSource: "helper",
	})
	if err != nil {
		return "", fmt.Errorf("title generation failed: %w", err)
	}
	title := sanitizeTitle(resp.OutputText)
	if title == "" {
		return "", fmt.Errorf("model returned empty/invalid title")
	}
	return title, nil
}

// sanitizeTitle trims, strips wrapping quotes, rejects error/truncation
// markers, and caps to maxTitleRunes. Returns "" for unusable output.
func sanitizeTitle(raw string) string {
	t := strings.TrimSpace(strings.Trim(strings.TrimSpace(raw), "\"'`"))
	if t == "" {
		return ""
	}
	low := strings.ToLower(t)
	if strings.Contains(low, "[incomplete response") ||
		strings.Contains(low, "token limit") ||
		strings.Contains(low, "truncated") || len(t) > 200 {
		return ""
	}
	if idx := strings.IndexAny(t, "\n\r"); idx >= 0 {
		t = strings.TrimSpace(t[:idx])
	}
	if utf8.RuneCountInString(t) > maxTitleRunes {
		t = string([]rune(t)[:maxTitleRunes-3]) + "..."
	}
	return t
}

// buildTitleTranscript serializes user/assistant text and tail-caps it so the
// title reflects the most recent context. Reuses buildTranscript (summarize.go).
func buildTitleTranscript(messages []client.Message) string {
	full := buildTranscript(messages)
	if r := []rune(full); len(r) > maxTitleInputRunes {
		return string(r[len(r)-maxTitleInputRunes:])
	}
	return full
}
