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
	// The preflight emergency gate in internal/agent/loop.go provides the
	// independent safety net. Product traffic distributions and observed
	// compaction rates are maintained outside this public repository.
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
	// message as fresh cache_creation (issue #124).
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
	// Same value as compactRetargetFraction by coincidence only; do not unify.
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
	return inputTokens+outputTokens >= compactTargetTokens(contextWindow)
}

// compactTargetTokens is the line compaction layers judge "over" against:
// compactThreshold × contextWindow. Shared by ShouldCompact (the proactive
// trigger), ShapeHistory's skip gate and TruncateOversizedLastUserMessage so
// they all agree with the trigger. Before this helper the trigger fired at
// 90% while ShapeHistory accepted anything under 100%, so a session could
// trigger every iteration yet never shrink below the trigger — paying a
// summary per Run for nothing.
func compactTargetTokens(contextWindow int) int {
	fractional := int(float64(contextWindow) * compactThreshold)
	absolute := contextWindow - compactAbsoluteBufferTokens
	if absolute > fractional {
		return absolute
	}
	return fractional
}

// compactAbsoluteBufferTokens is the flat reserve the absolute trigger keeps
// free below the context window: room for the response (max_tokens tiers top
// out well under 32K) plus estimator-error and next-turn-tool-result margin.
// Workload that justifies it: the 1M-context families the default tiers route
// to, where the fractional 90% trigger alone forfeits ~100K usable tokens
// (leading agent harnesses budget absolutely for this reason). Symptom
// when it binds: sessions on 1M windows run ~40K tokens closer to the limit
// before compacting, leaning harder on the calibrated estimator; on windows
// ≤ ~600K the fractional floor wins and behavior is unchanged. Override:
// edit this constant (raising it returns headroom to the old fractional
// behavior).
const compactAbsoluteBufferTokens = 60_000

// compactRetargetFraction is where a compaction aims to LAND — deliberately
// below the compactThreshold trigger so one compaction buys real headroom
// (hysteresis) instead of stopping exactly at the line and re-triggering on
// the next large tool result (observed live: two full compactions within
// three iterations, each paying PersistLearnings + GenerateSummary + a full
// prompt-cache rebuild).
//   - Workload: sessions with large per-turn tool results (file reads,
//     browser snapshots) near the window limit.
//   - Symptom when it binds: keepLast shrinks further than strictly needed —
//     up to an extra (compactThreshold − compactRetargetFraction) × window of
//     recent turn pairs is summarized away per compaction.
//   - Override: raise toward compactThreshold for maximum context retention
//     at the cost of more frequent compactions; the gap between the two is
//     the hysteresis band.
//
// Same value as stableUserBudgetFraction by coincidence only — that one
// budgets a single user message inside the target; do not unify.
const compactRetargetFraction = 0.80

// compactLandingTokens is the acceptance budget for shaped candidates. It
// tracks the trigger's shape: fractional on small windows, window − 2×buffer
// on large ones — so the hysteresis band stays one buffer wide when the
// absolute trigger governs, instead of ballooning to (trigger − 0.80×window)
// and summarizing away far more history than the band requires.
func compactLandingTokens(contextWindow int) int {
	fractional := int(float64(contextWindow) * compactRetargetFraction)
	absolute := contextWindow - 2*compactAbsoluteBufferTokens
	if absolute > fractional {
		return absolute
	}
	return fractional
}

// CompactTriggerTokens exposes the compaction trigger line to callers that
// add content right after a compaction (post-compaction file restoration):
// the injected payload must keep the calibrated estimate under this line or
// the very next iteration re-triggers the compaction it just paid for.
// Budgeting against the trigger — not the landing line — is deliberate:
// ShapeHistory stops at the first keepLast that fits under the landing
// budget, so the slack below THAT line is typically less than one turn pair
// and restoration would almost never run. The cost is that restoration
// consumes part of the 90/80 hysteresis band.
func CompactTriggerTokens(contextWindow int) int {
	return compactTargetTokens(contextWindow)
}

// ShapeHistory builds a sliding window over messages:
//
//	[system] + [first user message] + [summary] + [last N turn pairs]
//
// If the history is short enough to fit without shaping, it's returned as-is.
// After shaping, if estimated tokens still exceed the compaction target,
// keepLast is reduced iteratively down to minKeepLast.
//
// overheadTokens calibrates EstimateTokens against the provider's real prompt
// accounting: callers that have observed real usage pass
// (real prompt tokens − estimate at send time), so tool schemas — which live
// outside messages — and the chars/3.5 underestimate on dense content both
// count against the budget. Known bias: the overhead fuses a fixed term
// (schemas) with one proportional to content, and is charged whole against
// shaped candidates that retain only part of the content — near the boundary
// this over-compacts by a pair or two. Direction is safe (never
// under-compacts); split into ratio + fixed terms if it ever matters.
// Real usage and the estimate disagreed by ~25% on
// code-heavy sessions, which put the whole [90% real, 100% est] band into a
// "trigger fires, shaper declines" dead zone (2026-08-04 e2e). Pass 0 when no
// real measurement exists (pure-estimate callers keep the old behavior, just
// against the 90% target).
//
// The shaped result is returned only when it actually drops messages.
// A candidate that merely inserts the summary (keepLast covers every pair)
// would break the prompt-cache prefix and grow the prompt without freeing
// anything, so the original slice is returned unchanged instead — callers can
// detect the no-op via len() and must NOT treat it as an applied compaction.
func ShapeHistory(messages []client.Message, summary string, contextWindow int, overheadTokens int) []client.Message {
	if overheadTokens < 0 {
		overheadTokens = 0
	}
	// Skip shaping if too few messages to meaningfully shape (need system + first user + at least minKeepLast pairs)
	if len(messages) <= 3+minKeepLast*2 {
		return messages
	}
	// Skip if both message count is low AND calibrated tokens fit under the
	// compaction target. Judging against compactTargetTokens (not the raw
	// window) keeps this gate consistent with the ShouldCompact trigger:
	// when the trigger says "over the line", this gate must not say "fits".
	if len(messages) <= 3+defaultKeepLast*2 &&
		(contextWindow <= 0 || EstimateTokens(messages)+overheadTokens < compactTargetTokens(contextWindow)) {
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
		if contextWindow <= 0 || EstimateTokens(shaped)+overheadTokens < compactLandingTokens(contextWindow) {
			if len(shaped) >= len(messages) {
				// Fits, but nothing was dropped (summary-only insertion).
				return messages
			}
			return shaped
		}
		keepLast--
	}

	// Floor: return with minKeepLast even if over budget — unless even the
	// floor drops nothing, in which case shaping cannot help. With the
	// current constants the ≤ 3+minKeepLast*2 early return above already
	// excludes every history the floor could fail to shrink, so this guard
	// is defence-in-depth for future constant changes, not live logic.
	floor := buildShaped(system, firstUser, summary, rest, minKeepLast)
	if len(floor) >= len(messages) {
		return messages
	}
	return floor
}

// ForceShapeHistory shapes unconditionally on behalf of an explicit user
// request (TUI /compact): the caller has already paid for PersistLearnings
// and the summary, so the budget-based skip gates of ShapeHistory — which
// exist to avoid pointless compaction the user never asked for — do not
// apply. keepLast is capped so at least one message is actually dropped
// (net of the inserted summary); when even the minKeepLast floor cannot
// reduce the history, the original slice is returned unchanged and the
// caller should report "too short to compact".
func ForceShapeHistory(messages []client.Message, summary string, contextWindow int, overheadTokens int) []client.Message {
	if overheadTokens < 0 {
		overheadTokens = 0
	}
	if len(messages) <= 3+minKeepLast*2 {
		return messages
	}

	system := messages[0]
	firstUser := messages[1]
	rest := messages[2:]

	// Largest keepLast that still nets a reduction: buildShaped keeps
	// keepLast*2 tail messages and adds the summary message (when present),
	// so require keepLast*2 ≤ len(rest) − (1 + summaryLen).
	minDrop := 1
	if summary != "" {
		minDrop = 2
	}
	keepLast := (len(rest) - minDrop) / 2
	if keepLast > defaultKeepLast {
		keepLast = defaultKeepLast
	}
	for keepLast > minKeepLast {
		shaped := buildShaped(system, firstUser, summary, rest, keepLast)
		if contextWindow <= 0 || EstimateTokens(shaped)+overheadTokens < compactLandingTokens(contextWindow) {
			if len(shaped) >= len(messages) {
				return messages
			}
			return shaped
		}
		keepLast--
	}

	floor := buildShaped(system, firstUser, summary, rest, minKeepLast)
	if len(floor) >= len(messages) {
		return messages
	}
	return floor
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
func TruncateOversizedLastUserMessage(messages []client.Message, contextWindow int, overheadTokens int) ([]client.Message, int) {
	if contextWindow <= 0 || len(messages) == 0 {
		return messages, 0
	}
	if overheadTokens < 0 {
		overheadTokens = 0
	}
	target := compactTargetTokens(contextWindow)
	// overheadTokens (real prompt tokens − estimate at send time, when the
	// caller has observed real usage) counts the same way here as in
	// ShapeHistory: it is non-user-text mass (tool schemas, estimator
	// underestimate), so it inflates `estimated` and thereby lands in the
	// "other messages" side of every budget computation below.
	estimated := EstimateTokens(messages) + overheadTokens
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
		//
		// Futility guard: only clip when plain-text user content can plausibly
		// fix the overflow. When the overflow is driven by structured content
		// (tool_result blocks in tool-heavy sessions), the only plain-text
		// user message is typically the scaffolded first message carrying
		// user_instructions — clipping it to the floor destroys the persona
		// and the request while leaving the prompt over target anyway
		// (observed 2026-08-04: instructions cut 52K→4K chars across three
		// passes, est still over, all tool_results untouched). If even
		// clipping every plain-text user message down to the floor cannot
		// reach target, skip: ShapeHistory / reactive handling own this case.
		maxRecoverable := 0
		for i := range messages {
			if messages[i].Role != "user" || messages[i].Content.HasBlocks() {
				continue
			}
			text := messages[i].Content.Text()
			if text == "" {
				continue
			}
			// Content tokens only — deliberately NOT + overheadPerMessage
			// (unlike the oversized scan above): a truncated message still
			// exists and still pays its per-message overhead, so only the
			// content above the floor is actually recoverable. Including the
			// overhead would make this guard optimistic by 4 tokens per
			// message — engaging truncation in cases that cannot reach target.
			msgTokens := int(math.Ceil(float64(utf8.RuneCountInString(text)) / charsPerToken))
			if msgTokens > minUserTokenFloor {
				maxRecoverable += msgTokens - minUserTokenFloor
			}
		}
		if estimated-maxRecoverable > target {
			return messages, 0
		}
		return truncateLargestUserMessageToFit(messages, target, estimated, minUserTokenFloor)
	}

	// No futility guard on this branch (unlike the aggregate fallback below):
	// an oversized message by definition holds > singleCap tokens, so clipping
	// it recovers a large, usually decisive share of the overflow — and this
	// branch is the giant-paste protection (2026-05-11 stress: a single
	// 191K-token user message escaped every gate), where declining would resend
	// an over-cap prompt with no cheaper layer left to fix it.
	//
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
