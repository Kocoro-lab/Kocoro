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

	// stableUserBudgetFraction is the share of the compaction target that the
	// oversized plain-text user message(s) may keep after truncation; the
	// remaining 1−frac is reserved for the system prompt and other messages.
	// With several oversized messages the share is split equally among them
	// (see TruncateOversizedLastUserMessage), so the combined survivor still
	// fits this budget.
	//
	// Why a fraction of the (per-session-stable) target and NOT
	// target − EstimateTokens(messages): the slice-derived budget shifted the
	// byte boundary every turn as history grew, breaking the Anthropic
	// prompt-cache prefix at the truncated message and re-billing the whole
	// message as fresh cache_creation (~$0.67/follow-up turn — issue #124).
	// A fixed fraction makes the cut a pure function of (contextWindow, the
	// message, the oversized count), identical across turns. Scaling with
	// contextWindow rather than a flat char cap keeps 200K-era sizing from
	// silently over-truncating on 1M-context families (Hardcoded Limit Policy).
	//
	// Binds when: a single user message exceeds 0.80 of the target — ~504K
	// runes on a 200K cap, ~2.52M runes on the 1M default. truncateMessageBody
	// keeps head+tail so usable signal survives. Override: lower for a tighter
	// system-prompt reserve; raising past ~0.9 risks the clipped message alone
	// re-crossing the compaction threshold.
	stableUserBudgetFraction = 0.80

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

// TruncateOversizedLastUserMessage rune-safely head+tail truncates the
// oversized plain-text user message(s) when the message count is too small
// for ShapeHistory to help but the total prompt estimate already exceeds the
// compaction threshold.
//
// This guards against the "single huge user input" failure mode: a user
// pastes a 195K-token document as one message, len(messages) is far below
// MinShapeable() (=9), so both ShapeHistory and the preflight emergency
// path are gated off and the request escapes to the API untouched.
// Observed during 2026-05-11 stress testing as Stress D (191K input, no
// client-side defense fired).
//
// A message is truncated only when its own estimate exceeds singleCap (a fixed
// fraction of the compaction target). When several do, the cap is split equally
// so their combined post-truncation size still fits — the caller's convergence
// invariant. Each message's boundary depends only on (contextWindow, that
// message, the oversized count). For the common case — one huge paste plus
// small follow-ups — the oversized count stays 1 across turns, so the message
// reloaded from session.json truncates to the SAME bytes every follow-up turn
// and the Anthropic prompt-cache prefix is preserved (issue #124). Two rare
// edge cases trade that byte-stability for staying within budget: a SECOND huge
// paste on a later turn changes the count and re-clips the earlier ones; and
// several mid-size messages that individually fit but collectively overflow
// take the slice-aware aggregate fallback (truncateLargestUserMessageToFit).
// The common resume path is stable; the edge cases at least converge instead of
// escaping over budget.
//
// Returns messages unchanged when:
//   - contextWindow is non-positive (caller didn't configure)
//   - total estimate already fits under the compaction threshold
//   - the only over-budget content is structured (tool_result / image blocks
//     are skipped — ShapeHistory's deeper paths handle them)
//
// On truncation, replaces each oversized message's text content with a
// head+tail concatenation joined by a human-readable marker so the model can
// note the gap. Always UTF-8 rune-aligned — never splits a codepoint
// mid-sequence. Returns (messages, droppedChars). droppedChars > 0 means
// truncation actually happened; callers can use it to emit OnRunStatus or
// audit.
func TruncateOversizedLastUserMessage(messages []client.Message, contextWindow int) ([]client.Message, int) {
	if contextWindow <= 0 || len(messages) == 0 {
		return messages, 0
	}
	target := int(float64(contextWindow) * compactThreshold)
	estimated := EstimateTokens(messages)
	if estimated <= target {
		return messages, 0
	}

	// singleCap is the most a single plain-text user message may keep after
	// truncation — a fixed fraction of the (per-session-stable) target. A
	// message is "oversized" when its own estimate exceeds this cap; those are
	// the ones forcing the overflow. Normal follow-ups and assistant replies
	// fall below it, so the oversized SET is stable across turns even as small
	// history accumulates — that stability is what keeps the truncation
	// byte-identical across turns (issue #124).
	const minUserTokenFloor = 1000
	singleCap := int(float64(target) * stableUserBudgetFraction)
	if singleCap < minUserTokenFloor {
		singleCap = minUserTokenFloor
	}

	// Collect every oversized plain-text user message. This covers the resume
	// case ("most recent" would miss a huge prior message reloaded from
	// session.json behind a small new follow-up) and the multi-paste case (two
	// huge inputs in one short session). Structured content (tool_result /
	// image blocks) is skipped: truncating those is unsafe; ShapeHistory's
	// deeper paths handle them when message count allows.
	var oversized []int
	for i := range messages {
		if messages[i].Role != "user" || messages[i].Content.HasBlocks() {
			continue
		}
		text := messages[i].Content.Text()
		if text == "" {
			continue
		}
		msgTokens := int(math.Ceil(float64(utf8.RuneCountInString(text))/charsPerToken)) + overheadPerMessage
		if msgTokens > singleCap {
			oversized = append(oversized, i)
		}
	}
	if len(oversized) == 0 {
		// Aggregate overflow with no single oversized message: several mid-size
		// user messages each fit under singleCap but together exceed target.
		// The per-message cap can't engage, so fall back to clipping the single
		// largest message toward the remaining headroom (slice-aware, NOT
		// byte-stable). See truncateLargestUserMessageToFit for why this is
		// acceptable here.
		return truncateLargestUserMessageToFit(messages, target, estimated, minUserTokenFloor)
	}

	// Share singleCap across the oversized messages: N of them each capped at
	// singleCap/N, so their post-truncation SUM stays within singleCap and the
	// prompt drops below the preflight threshold even with several huge inputs
	// (the truncateUserMessageOverBudget caller's convergence invariant,
	// exercised by TestShortSessionTruncate_RepeatsUntilUnderPreflightThreshold).
	// Each message's boundary depends only on (contextWindow, that message, the
	// oversized count) — stable across turns AS LONG AS the oversized set is
	// stable. A small follow-up never enters the set (it's below singleCap), so
	// the common resume case is byte-stable; but a SECOND oversized paste
	// arriving on a later turn changes N and re-clips the earlier ones — a
	// bounded, rare cache cost for genuine multi-huge-paste resumes (#124).
	perMsgTokens := singleCap / len(oversized)
	if perMsgTokens < minUserTokenFloor {
		perMsgTokens = minUserTokenFloor
	}
	perMsgRunes := int(float64(perMsgTokens) * charsPerToken)

	totalDropped := 0
	for _, idx := range oversized {
		text := messages[idx].Content.Text()
		runeCount := utf8.RuneCountInString(text)
		if runeCount == 0 {
			continue
		}
		// EstimateTokens counts RUNES (chars/3.5) but truncateMessageBody
		// slices by BYTES. For ASCII bytes == runes; for CJK/emoji (~3
		// bytes/rune) a rune budget used as a byte cap would over-truncate to
		// ~1/3. Convert via this text's own bytes-per-rune ratio — a property
		// of the text, so the boundary stays byte-stable across turns.
		bytesPerRune := float64(len(text)) / float64(runeCount)
		byteBudget := int(float64(perMsgRunes) * bytesPerRune)
		if len(text) <= byteBudget {
			continue
		}
		truncated := truncateMessageBody(text, byteBudget)
		totalDropped += len(text) - len(truncated)
		oldContent := messages[idx].Content
		messages[idx] = client.Message{
			Role:    messages[idx].Role,
			Content: client.NewTextContent(truncated),
		}
		// Instrument the in-place content rewrite for cache-drift attribution
		// (CLAUDE.md "Prompt Cache" invariant). Without it, SHANNON_CACHE_DEBUG
		// shows a prefix flip on this path with no correlating compact row.
		client.LogCacheCompactEvent("user_truncate", idx, oldContent, messages[idx].Content)
	}
	return messages, totalDropped
}

// truncateLargestUserMessageToFit is the aggregate fallback for the rare short
// session where several mid-size plain-text user messages each fit under
// singleCap but together overflow target — the stable per-message cap in
// TruncateOversizedLastUserMessage can't engage. It clips the single largest
// plain-text user message toward the remaining headroom; the caller's loop
// (truncateUserMessageOverBudget) re-invokes until the prompt fits.
//
// This branch is slice-aware (budget = target − other messages' tokens) and so
// NOT byte-stable across turns — acceptable because it fires ONLY when the
// stable path cannot, where the alternative is the prompt escaping over budget
// to the API with no ShapeHistory / reactive backstop below MinShapeable (the
// Stress-D failure class). Single-huge-message #124 stability is unaffected:
// that case has an oversized message and takes the equal-split path instead.
func truncateLargestUserMessageToFit(messages []client.Message, target, estimated, minUserTokenFloor int) ([]client.Message, int) {
	idx := -1
	maxLen := 0
	for i := range messages {
		if messages[i].Role != "user" || messages[i].Content.HasBlocks() {
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
	runeCount := utf8.RuneCountInString(text)
	if runeCount == 0 {
		return messages, 0
	}
	userTokenEst := int(math.Ceil(float64(runeCount)/charsPerToken)) + overheadPerMessage
	otherTokens := estimated - userTokenEst
	if otherTokens < 0 {
		otherTokens = 0
	}
	budgetTokens := target - otherTokens
	if budgetTokens < minUserTokenFloor {
		budgetTokens = minUserTokenFloor
	}
	bytesPerRune := float64(len(text)) / float64(runeCount)
	byteBudget := int(float64(budgetTokens) * charsPerToken * bytesPerRune)
	if len(text) <= byteBudget {
		return messages, 0
	}
	truncated := truncateMessageBody(text, byteBudget)
	oldContent := messages[idx].Content
	messages[idx] = client.Message{
		Role:    messages[idx].Role,
		Content: client.NewTextContent(truncated),
	}
	client.LogCacheCompactEvent("user_truncate_aggregate", idx, oldContent, messages[idx].Content)
	return messages, len(text) - len(truncated)
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
