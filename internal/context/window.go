package context

import (
	"math"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

const (
	// charsPerToken is the conservative estimation ratio.
	// 3.5 chars/token handles mixed English/code/CJK better than 4.
	charsPerToken = 3.5

	// overheadPerMessage accounts for role, formatting, and separator tokens.
	overheadPerMessage = 4

	// compactThreshold is the fraction of context window that triggers compaction.
	// 0.90 leaves the cliff at 180K on a 200K cap, vs 170K at the historical
	// 0.85 setting; ~5% of sessions in the 170K–180K band skip the cliff entirely.
	// The preflight emergency gate at 0.95 (internal/agent/loop.go) is the safety
	// net for the remaining 10K headroom.
	compactThreshold = 0.90

	// defaultKeepLast is the default number of recent turn pairs to keep.
	defaultKeepLast = 20

	// minKeepLast is the minimum recent turn pairs to keep, even under budget pressure.
	minKeepLast = 3
)

// MinShapeable returns the minimum number of messages needed for shaping to
// have any effect: system + first user + at least minKeepLast turn pairs.
func MinShapeable() int {
	return 3 + minKeepLast*2 // 9
}

// EstimateTokens returns a heuristic token count for a slice of messages.
// Uses chars/3.5 + 4 overhead per message.
func EstimateTokens(messages []client.Message) int {
	total := 0
	for _, m := range messages {
		chars := countChars(m)
		tokens := int(math.Ceil(float64(chars) / charsPerToken))
		total += tokens + overheadPerMessage
	}
	return total
}

// ShouldCompact returns true if the total tokens (input + output) exceed
// 90% of the context window.
func ShouldCompact(inputTokens, outputTokens, contextWindow int) bool {
	if contextWindow <= 0 {
		return false
	}
	threshold := int(float64(contextWindow) * compactThreshold)
	return inputTokens+outputTokens >= threshold
}

// ShapeHistory builds a sliding window over messages:
//
//	[system] + [first user message] + [summary] + [last N turn pairs]
//
// If the history is short enough to fit without shaping, it's returned as-is.
// After shaping, if estimated tokens still exceed the context window,
// keepLast is reduced iteratively down to minKeepLast.
func ShapeHistory(messages []client.Message, summary string, contextWindow int) []client.Message {
	// Skip shaping if too few messages to meaningfully shape (need system + first user + at least minKeepLast pairs)
	if len(messages) <= 3+minKeepLast*2 {
		return messages
	}
	// Skip if both message count is low AND estimated tokens fit in budget
	if len(messages) <= 3+defaultKeepLast*2 && (contextWindow <= 0 || EstimateTokens(messages) < contextWindow) {
		return messages
	}

	// Extract system message (index 0) and first user message
	system := messages[0]
	firstUser := messages[1]

	// All remaining messages after system + first user
	rest := messages[2:]

	keepLast := defaultKeepLast
	for keepLast >= minKeepLast {
		shaped := buildShaped(system, firstUser, summary, rest, keepLast)
		if contextWindow <= 0 || EstimateTokens(shaped) < contextWindow {
			return shaped
		}
		keepLast--
	}

	// Floor: return with minKeepLast even if over budget
	return buildShaped(system, firstUser, summary, rest, minKeepLast)
}

// buildShaped assembles the shaped message array.
//
// The recent slice is taken positionally from the tail of rest, which means
// the slice boundary can land between an assistant tool_use and the matching
// user tool_result, leaving an orphaned tool_result at recent[0] (or, at the
// other end, an orphaned tool_use at recent[end] when the trailing tool_result
// got dropped). Anthropic's API rejects either with HTTP 400.
//
// We re-run stripOrphanedToolPairs on the assembled output to strip those
// boundary orphans. This intentionally avoids the rest of SanitizeHistory:
// mergeConsecutiveRoles would collapse firstUser and the summary-as-user
// message (both role=user) and drop the original first prompt, which is
// load-bearing as the conversation primer. Boundary tool-pair stripping
// only touches blocks whose pair is genuinely missing — not roles.
func buildShaped(system, firstUser client.Message, summary string, rest []client.Message, keepLast int) []client.Message {
	keepMsgs := keepLast * 2 // turn pairs = user + assistant
	if keepMsgs > len(rest) {
		keepMsgs = len(rest)
	}

	recent := rest[len(rest)-keepMsgs:]

	result := make([]client.Message, 0, 3+len(recent))
	result = append(result, system, firstUser)

	if summary != "" {
		result = append(result, client.Message{
			Role:    "user",
			Content: client.NewTextContent("Previous context summary: " + summary),
		})
	}

	result = append(result, recent...)
	return stripOrphanedToolPairs(result)
}

// TruncateOversizedLastUserMessage applies single-message rune-safe head+tail
// truncation to the most recent user message when the message count is too
// small for ShapeHistory to help but the total prompt estimate already
// exceeds the compaction threshold.
//
// This guards against the "single huge user input" failure mode: a user
// pastes a 195K-token document as one message, len(messages) is far below
// MinShapeable() (=9), so both ShapeHistory and the preflight emergency
// path are gated off and the request escapes to the API untouched.
// Observed during 2026-05-11 stress testing as Stress D (191K input, no
// client-side defense fired).
//
// Returns messages unchanged when:
//   - contextWindow is non-positive (caller didn't configure)
//   - total estimate already fits under the compaction threshold
//   - the last user message has structured content (tool_result / image
//     blocks): truncating those is unsafe; ShapeHistory's deeper paths
//     would be the right place to handle them
//   - the user message's text body already fits its share of the budget
//
// On truncation, replaces the user message's text content with a head+tail
// concatenation joined by a human-readable marker so the model can note
// the gap. Always UTF-8 rune-aligned — never splits a codepoint mid-sequence.
// Returns (messages, droppedChars). droppedChars > 0 means truncation
// actually happened; callers can use it to emit OnRunStatus or audit.
func TruncateOversizedLastUserMessage(messages []client.Message, contextWindow int) ([]client.Message, int) {
	if contextWindow <= 0 || len(messages) == 0 {
		return messages, 0
	}
	target := int(float64(contextWindow) * compactThreshold)
	estimated := EstimateTokens(messages)
	if estimated <= target {
		return messages, 0
	}

	// Find the LARGEST plain-text user message — the one actually pushing
	// the prompt over the threshold. "Most recent" misses the resume case
	// (daemon/TUI loaded a huge prior user message from session.json, the
	// new user message is a small follow-up — truncating the small one
	// does nothing while the huge one continues to blow up the prompt).
	// Structured content (tool_result / image blocks) is skipped here;
	// ShapeHistory's full path handles those when message count allows.
	idx := -1
	maxLen := 0
	for i := 0; i < len(messages); i++ {
		if messages[i].Role != "user" {
			continue
		}
		if messages[i].Content.HasBlocks() {
			continue
		}
		if l := len(messages[i].Content.Text()); l > maxLen {
			maxLen = l
			idx = i
		}
	}
	if idx < 0 {
		return messages, 0
	}
	text := messages[idx].Content.Text()
	if text == "" {
		return messages, 0
	}

	// EstimateTokens measures content in RUNES (chars/3.5 ratio), but
	// truncateMessageBody slices by BYTES. For pure ASCII bytes == runes so
	// the two collapse to the same number, but for CJK/emoji (~3 bytes per
	// rune) a rune-count budget interpreted as byte cap would over-truncate
	// the content to ~1/3 of the intended size. Convert by sampling this
	// text's actual bytes-per-rune ratio. (PR review item #5.)
	runeCount := utf8.RuneCountInString(text)
	if runeCount == 0 {
		return messages, 0
	}
	userTokenEst := int(math.Ceil(float64(runeCount)/charsPerToken)) + overheadPerMessage
	otherTokens := estimated - userTokenEst
	if otherTokens < 0 {
		otherTokens = 0
	}
	userTokenBudget := target - otherTokens
	const minUserTokenFloor = 1000
	if userTokenBudget < minUserTokenFloor {
		userTokenBudget = minUserTokenFloor
	}
	userRuneBudget := int(float64(userTokenBudget) * charsPerToken)
	bytesPerRune := float64(len(text)) / float64(runeCount)
	userByteBudget := int(float64(userRuneBudget) * bytesPerRune)

	if len(text) <= userByteBudget {
		return messages, 0
	}

	truncated := truncateMessageBody(text, userByteBudget)
	dropped := len(text) - len(truncated)
	messages[idx] = client.Message{
		Role:    messages[idx].Role,
		Content: client.NewTextContent(truncated),
	}
	return messages, dropped
}

// truncateMessageBody returns s capped at `cap` bytes via head+tail
// rune-aligned slicing. Same UTF-8-safety contract as
// capTranscriptForSummarize in summarize.go: head/tail boundaries are
// adjusted to rune starts so multibyte content (CJK/emoji) is never
// split mid-codepoint.
func truncateMessageBody(s string, cap int) string {
	if len(s) <= cap {
		return s
	}
	const marker = "\n\n[... user message truncated for size — middle elided ...]\n\n"
	if cap <= len(marker) {
		// Cap is smaller than the marker itself: skip the marker and just
		// keep the prefix. Rune-align the head end.
		head := cap
		for head > 0 && !utf8.RuneStart(s[head]) {
			head--
		}
		return s[:head]
	}
	half := (cap - len(marker)) / 2

	headEnd := half
	for headEnd > 0 && !utf8.RuneStart(s[headEnd]) {
		headEnd--
	}

	tailStart := len(s) - half
	for tailStart < len(s) && !utf8.RuneStart(s[tailStart]) {
		tailStart++
	}

	return s[:headEnd] + marker + s[tailStart:]
}

// imageTokenEstimate is the approximate token cost of an image block.
// Anthropic charges ~1600 tokens for a typical image; 1000 is a conservative floor.
const imageTokenChars = 3500 // 1000 tokens * 3.5 chars/token

// countChars counts total characters in a message's content.
// Images are estimated as a fixed char cost since their base64 data is not
// representative of actual token usage.
func countChars(m client.Message) int {
	if m.Content.HasBlocks() {
		total := 0
		for _, b := range m.Content.Blocks() {
			switch b.Type {
			case "text":
				total += len([]rune(b.Text))
			case "tool_use":
				total += len([]rune(b.Name)) + len(b.Input)
			case "tool_result":
				total += countToolResultChars(b)
			case "image":
				total += imageTokenChars
			}
		}
		return total
	}
	return len([]rune(m.Content.Text()))
}

// countToolResultChars counts chars in a tool_result, including nested blocks.
func countToolResultChars(b client.ContentBlock) int {
	switch v := b.ToolContent.(type) {
	case string:
		return len([]rune(v))
	case []client.ContentBlock:
		total := 0
		for _, nb := range v {
			switch nb.Type {
			case "text":
				total += len([]rune(nb.Text))
			case "image":
				total += imageTokenChars
			}
		}
		return total
	}
	return 0
}
