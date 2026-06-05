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
	// The length check is the garbage gate (the model spat a wall of text
	// instead of a title). It MUST be rune-based: byte length rejects a pure-CJK
	// title of ~67+ runes (~201 bytes) wholesale instead of letting it fall
	// through to the maxTitleRunes truncation below the way a Latin title does.
	if strings.Contains(low, "[incomplete response") ||
		strings.Contains(low, "token limit") ||
		strings.Contains(low, "truncated") || utf8.RuneCountInString(t) > 200 {
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

// buildTitleTranscript collects ONLY user-message text and final-assistant
// reply text (assistant messages with no tool_use block — the same predicate
// CountCompletedTurns uses), then tail-caps to maxTitleInputRunes as a final
// safety bound on the cleaned text. Excluding tool_use / tool_result content
// keeps the user's opening question from being evicted by a tool-heavy first
// turn (e.g. 3 tools ≈ 1800 runes), which buildTranscript would serialize and
// the tail-cap would then drop the question for.
func buildTitleTranscript(messages []client.Message) string {
	var sb strings.Builder
	for _, m := range messages {
		switch m.Role {
		case "user":
			// Tool RESULTS arrive as user-role messages; their text is tool
			// noise. Skip any user message that carries blocks (a plain user
			// turn is text-only with no blocks).
			if m.Content.HasBlocks() {
				continue
			}
			if t := strings.TrimSpace(m.Content.Text()); t != "" {
				fmt.Fprintf(&sb, "[user]: %s\n\n", t)
			}
		case "assistant":
			if hasToolUseBlock(m) {
				continue
			}
			if t := strings.TrimSpace(m.Content.Text()); t != "" {
				fmt.Fprintf(&sb, "[assistant]: %s\n\n", t)
			}
		}
	}
	full := sb.String()
	if r := []rune(full); len(r) > maxTitleInputRunes {
		return string(r[len(r)-maxTitleInputRunes:])
	}
	return full
}

// hasToolUseBlock reports whether an assistant message contains a tool_use
// block (i.e. is a mid-turn tool call, not a final reply). Shared by
// buildTitleTranscript and CountCompletedTurns so the "final reply" predicate
// stays identical between the title input and the trigger-turn count.
func hasToolUseBlock(m client.Message) bool {
	for _, b := range m.Content.Blocks() {
		if b.Type == "tool_use" {
			return true
		}
	}
	return false
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

// brandDisplayNames overrides the default upper-first casing for sources whose
// canonical brand spelling differs (LINE is all-caps, WeCom is camel-cased).
// slack/feishu/lark/telegram/webhook upper-first to their correct form, so they
// need no entry. routeTitle (internal/daemon/runner.go) does the same
// upper-first transform on the raw source — keep that in sync; the helper here
// is the single source of truth, but routeTitle builds its own prefix string
// for the instant placeholder before this package is reachable.
var brandDisplayNames = map[string]string{
	"line":  "LINE",
	"wecom": "WeCom",
}

// SourceLabel returns the human label for an IM-style source ("slack" →
// "Slack"), or "" for interactive sources (desktop/kocoro/empty).
func SourceLabel(source string) string {
	s := strings.ToLower(strings.TrimSpace(source))
	// Exclusion set mirrors daemon routeTitle (internal/daemon/runner.go);
	// keep both in sync when adding an interactive (non-IM) source.
	switch s {
	case "", "desktop", "shanclaw", "kocoro":
		return ""
	}
	if name, ok := brandDisplayNames[s]; ok {
		return name
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// DecorateTitle rebuilds the channel+sender prefix routeTitle produces (see
// internal/daemon/runner.go) so an upgraded title keeps the same shape as the
// instant placeholder: "Slack · Wayland · <title>" when sender is set,
// "Slack · <title>" when it isn't, and the bare title for interactive sources.
// The " · " separator must match routeTitle's exactly.
func DecorateTitle(source, sender, smartTitle string) string {
	label := SourceLabel(source)
	if label == "" {
		return smartTitle
	}
	if sender != "" {
		return label + " · " + sender + " · " + smartTitle
	}
	return label + " · " + smartTitle
}

// CountCompletedTurns counts completed conversation turns — assistant messages
// that are a FINAL reply (no tool_use block). A single user turn that uses
// tools yields several assistant messages (one tool_use message per iteration)
// plus the final reply; only the final reply marks a completed turn. Counting
// raw assistant messages instead would make the TitleTriggerTurns {1,3} gate
// fire unpredictably — a 1-tool turn yields 2 assistant messages, skipping both
// triggers. Tool RESULTS arrive as user-role messages and are ignored here.
func CountCompletedTurns(messages []client.Message) int {
	n := 0
	for _, m := range messages {
		if m.Role != "assistant" {
			continue
		}
		if !hasToolUseBlock(m) {
			n++
		}
	}
	return n
}

// UpgradeTitle generates a smart title, decorates it for the source, and
// persists it via the patcher. Best-effort: returns the final title written,
// or "" if generation failed / the patcher skipped (locked / straggler). The
// caller keeps the existing placeholder on "".
func UpgradeTitle(ctx context.Context, c Completer, p AutoTitlePatcher, sessionID, source, sender string, msgs []client.Message, atTurns int) string {
	smart, err := GenerateTitle(ctx, c, msgs)
	if err != nil {
		return ""
	}
	final := DecorateTitle(source, sender, smart)
	if ok, err := p.PatchAutoTitle(sessionID, final, atTurns); err != nil || !ok {
		return ""
	}
	return final
}
