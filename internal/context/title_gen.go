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

// AutoTitlePatcher persists a guarded title upgrade. Satisfied by both
// *session.Manager (daemon/TUI — also syncs the active session) and
// *session.Store (one-shot/tests), without this package importing session.
type AutoTitlePatcher interface {
	PatchAutoTitle(id, title string, atTurns int) (bool, error)
}

// TitleTriggerTurns are the assistant-turn counts at which a smart title is
// (re)generated: turn 1 (upgrade the placeholder) and turn 3 (richer context).
// Mirrors Claude Code's count==1 / count==3 pattern.
var TitleTriggerTurns = map[int]bool{1: true, 3: true}

// SourceLabel returns the human label for an IM-style source ("slack" →
// "Slack"), or "" for interactive sources (desktop/kocoro/empty).
func SourceLabel(source string) string {
	s := strings.ToLower(strings.TrimSpace(source))
	switch s {
	case "", "desktop", "shanclaw", "kocoro":
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// DecorateTitle prefixes a smart title with its IM source ("Slack · <title>")
// so IM sessions stay distinguishable by channel while gaining real content.
func DecorateTitle(source, smartTitle string) string {
	if label := SourceLabel(source); label != "" {
		return label + " · " + smartTitle
	}
	return smartTitle
}

// UpgradeTitle generates a smart title, decorates it for the source, and
// persists it via the patcher. Best-effort: returns the final title written,
// or "" if generation failed / the patcher skipped (locked / straggler). The
// caller keeps the existing placeholder on "".
func UpgradeTitle(ctx context.Context, c Completer, p AutoTitlePatcher, sessionID, source string, msgs []client.Message, atTurns int) string {
	smart, err := GenerateTitle(ctx, c, msgs)
	if err != nil {
		return ""
	}
	final := DecorateTitle(source, smart)
	if ok, err := p.PatchAutoTitle(sessionID, final, atTurns); err != nil || !ok {
		return ""
	}
	return final
}
