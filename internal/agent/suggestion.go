package agent

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// SuggestionPrompt is the synthetic user message appended to the main
// turn's message history to elicit a single short follow-up suggestion.
// Format constraints mirror Claude Code's promptSuggestion (2-12 words,
// match user tone, no Claude voice). The model's reply is filtered further
// by FilterSuggestion before display.
//
// CHANGE WITH CARE: this string is part of the forked request's tail tokens
// (uncached). Edits do not invalidate the main turn's cache prefix, but they
// do change suggestion behavior in non-obvious ways.
const SuggestionPrompt = `<system-instruction>Predict the user's most likely next message in this conversation. Respond with ONLY that next message — 2 to 12 words, matching the user's tone. No quotes, no preamble, no explanation. If you cannot confidently predict a useful next message, respond with exactly the word "skip".</system-instruction>`

// allowedSingleWords contains short imperative action verbs acceptable as
// 1-word suggestions. Anything outside this set requires ≥2 words. Filler
// conversational tokens such as "yes"/"yeah"/"ok"/"sure" are intentionally
// excluded — they read as Claude-voice fillers rather than concrete user
// follow-ups, and would crowd out more useful predictions. "skip" is omitted
// because the meta-marker check at the top of FilterSuggestion rejects it
// before the allowlist runs.
var allowedSingleWords = map[string]bool{
	"continue": true, "commit": true, "push": true,
	"deploy": true, "merge": true, "test": true,
	"retry": true, "stop": true, "cancel": true,
	"go": true, "run": true,
}

// evaluativeWords are common Claude-voice or filler tokens that we drop.
// Keys are compared after stripping leading/trailing ".,!?" and lowercasing,
// so e.g. "sure!" → "sure" before lookup — only the trimmed form is meaningful here.
var evaluativeWords = map[string]bool{
	"great": true, "perfect": true, "excellent": true, "absolutely": true,
	"certainly": true, "of": true, "course": true, // "of course"
}

// claudeVoicePatterns are substring markers that indicate the model is talking
// AS itself, not predicting the user.
var claudeVoicePatterns = []string{
	"i'll", "i will", "let me", "i can", "i think",
	"sure, i", "ok, i", "yes, i",
}

// isCJKDominant returns true when more than half of the non-space runes
// belong to CJK / Japanese / Korean scripts. These languages are usually
// written without spaces between words, so word-based length thresholds
// (strings.Fields = 1) wrongly reject them. CJK strings are evaluated by
// rune count instead. Threshold of 50%+ catches mixed strings as
// CJK-dominant when the CJK characters carry the meaning.
func isCJKDominant(s string) bool {
	var cjk, nonSpace int
	for _, r := range s {
		if r == ' ' || r == '\t' {
			continue
		}
		nonSpace++
		if unicode.Is(unicode.Han, r) || // Chinese
			unicode.Is(unicode.Hiragana, r) || // Japanese hiragana
			unicode.Is(unicode.Katakana, r) || // Japanese katakana
			unicode.Is(unicode.Hangul, r) { // Korean
			cjk++
		}
	}
	if nonSpace == 0 {
		return false
	}
	return cjk*2 > nonSpace
}

// FilterSuggestion validates a model-generated suggestion against the
// constraints declared in SuggestionPrompt. Returns the cleaned suggestion
// and true if acceptable, or empty string and false if rejected.
//
// Length thresholds adapt to script (a hard upper rune cap of 65 applies
// to both before the script-specific gates):
//   - Latin / Cyrillic / etc (space-separated): 2-13 words
//   - CJK-dominant: 4-30 runes (one CJK char ≈ one "word")
//
// Other rejection reasons (apply to both scripts):
//   - empty or whitespace-only
//   - meta marker like "skip" / "done" / "none" / Chinese "跳过" "无"
//   - multi-sentence (contains . ! ? 。 ！ ？ before final char)
//   - contains format chars (newline, markdown wrap)
//   - contains evaluative word at start (English only; CJK uses different idioms,
//     not blocked at MVP — revisit when feedback shows Claude voice leaking through)
//   - contains Claude-voice pattern (English substrings)
func FilterSuggestion(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}

	lower := strings.ToLower(s)
	if lower == "skip" || lower == "done" || lower == "none" {
		return "", false
	}
	// CJK meta markers — "跳过" (skip), "无" (none), "完成" (done)
	if s == "跳过" || s == "无" || s == "完成" || s == "なし" || s == "スキップ" {
		return "", false
	}

	// Format chars
	if strings.ContainsAny(s, "\n\r\t*_`#") {
		return "", false
	}

	runeCount := utf8.RuneCountInString(s)

	// Defense-in-depth hard upper bound. The per-script caps below (30 runes
	// for CJK, 65 for Latin) are the primary enforcement; this exists as a
	// belt-and-suspenders guard against accidental relaxation of those.
	if runeCount > 100 {
		return "", false
	}

	// Multi-sentence — any rune after an end-punct rune (regardless of script
	// or whitespace) means multiple sentences. ASCII uses ". "; CJK has no
	// space after 。 — the unified "if anything follows, reject" rule handles
	// both. A trailing end-punct as the final rune is fine (single sentence).
	for i, r := range s {
		isEndPunct := r == '.' || r == '!' || r == '?' ||
			r == '。' || r == '！' || r == '？'
		if !isEndPunct {
			continue
		}
		if i+utf8.RuneLen(r) < len(s) {
			return "", false
		}
	}

	if isCJKDominant(s) {
		// CJK path: count runes (excluding whitespace), thresholds 4-30
		var nonSpace int
		for _, r := range s {
			if r != ' ' && r != '\t' {
				nonSpace++
			}
		}
		if nonSpace < 4 || nonSpace > 30 {
			return "", false
		}
		// CJK-specific evaluative-word check is skipped at MVP. Track in P1 backlog.
		return s, true
	}

	// Latin / non-CJK path: original word-count logic. The latin char cap
	// (65 runes) is tighter than the global 100-rune cap so that long-but-
	// few-words suggestions still get rejected.
	if runeCount > 65 {
		return "", false
	}
	words := strings.Fields(s)
	if len(words) == 0 {
		return "", false
	}
	if len(words) > 13 {
		return "", false
	}
	if len(words) < 2 {
		if !allowedSingleWords[strings.ToLower(strings.Trim(words[0], ".,!?"))] {
			return "", false
		}
	}

	// Evaluative word at start
	first := strings.ToLower(strings.Trim(words[0], ".,!?"))
	if evaluativeWords[first] {
		return "", false
	}

	// Claude voice
	for _, p := range claudeVoicePatterns {
		if strings.Contains(lower, p) {
			return "", false
		}
	}

	return s, true
}

// BuildForkedSuggestionRequest is a thin wrapper over BuildForkedRequest
// specialized for prompt suggestion: appends a single user message containing
// SuggestionPrompt and sets SkipCacheWrite + DebugKind.
//
// CACHE SAFETY: inherits the BuildForkedRequest invariant — byte-equal to
// main except for the one appended message and SkipCacheWrite. Do not add
// any further customization here; if a future use case needs to also restrict
// tools or override params, route it through ForkOptions on the primitive
// (and add an audit row — see forkedrequest.go).
func BuildForkedSuggestionRequest(main client.CompletionRequest) client.CompletionRequest {
	out, _ := BuildForkedRequest(main, ForkOptions{
		AppendMessages: []client.Message{{Role: "user", Content: client.NewTextContent(SuggestionPrompt)}},
		SkipCacheWrite: true,
		DebugKind:      "suggestion",
	})
	// Tag the wire request so Shannon Cloud can apply the dedicated
	// "prompt_suggestion" billing class: the daemon emits this call as an
	// internal feature, the user did not request it explicitly, so the cost
	// is computed normally for telemetry/internal cost tracking but is NOT
	// charged to the user's `token_usage` / quota. The cloud-side rule is
	// the load-bearing half of this contract — until it lands, this mark is
	// inert and user accounts are still charged.
	//
	// TTL policy: prompt_suggestion ALWAYS resolves to the 5m TTL bucket on
	// the cloud side (the default for any source not in
	// `_LONG_CACHE_SOURCES`). This is a deliberate design choice, not a
	// requirement to mirror the parent caller. Consequence: if a future
	// cloud release routes the parent main source (e.g. "shanclaw") to the
	// 1h bucket, the main turn's cache_control bytes and the fork's
	// cache_control bytes will diverge — Anthropic cache keys change,
	// fork-input drops from cache_read (~$0.001) to full price (~$0.015),
	// suggestion cost rises ~10×. The suggestion call is small and
	// per-turn, so we accept that regression rather than thread parent TTL
	// through the wire schema. Re-evaluate this trade-off the day
	// `_LONG_CACHE_SOURCES` stops being a frozenset of size 0.
	out.CacheSource = "prompt_suggestion"
	return out
}

// GenerateSuggestionResult bundles the filtered suggestion text with the
// cache/usage data from the forked call so the caller can audit cache health.
// Text is the post-FilterSuggestion string (may be empty if the filter rejects
// the model's reply); Usage and Model come from the gateway response and are
// populated even when Text is empty so audit rows still capture cost data.
type GenerateSuggestionResult struct {
	Text  string       // filtered suggestion ("" if filter rejected)
	Usage client.Usage // raw gateway usage (cache_read_tokens etc.)
	Model string       // model id used (for audit)
}

// GenerateSuggestion runs a single forked LLM call to elicit a next-prompt
// suggestion. Returns the filtered suggestion text (≤12 words) or empty string
// if the model returned no usable suggestion. Returns a non-nil error only on
// transport failure; filter rejection is signaled by empty string + nil error
// (caller treats both as "no suggestion to display").
//
// Cost: 1 LLM call. With a warm prompt cache, input cost ≈ cache_read for the
// prefix + full price for ~150 tokens (SuggestionPrompt + small overhead).
// Output is capped by the filter to ≤100 chars (~30 tokens). Skipped by the
// caller (suggestion_handler) when the cache is cold per
// agent.prompt_suggestion.cache_cold_threshold_tokens.
//
// Thin wrapper over GenerateSuggestionWithUsage that discards the Usage/Model
// fields. Callers that need cache-health auditing (daemon post-Run hook) call
// the WithUsage variant directly.
func GenerateSuggestion(ctx context.Context, llm client.LLMClient, main client.CompletionRequest) (string, error) {
	res, err := GenerateSuggestionWithUsage(ctx, llm, main)
	return res.Text, err
}

// GenerateSuggestionWithUsage is the Usage-returning variant of
// GenerateSuggestion. Used by the daemon's post-Run hook to write audit rows
// that record the forked call's cache_read_tokens / cache_creation_tokens —
// the only signal that the suggestion path is actually piggybacking on the
// main turn's prompt cache as designed.
func GenerateSuggestionWithUsage(ctx context.Context, llm client.LLMClient, main client.CompletionRequest) (GenerateSuggestionResult, error) {
	req := BuildForkedSuggestionRequest(main)

	resp, err := llm.Complete(ctx, req)
	if err != nil {
		return GenerateSuggestionResult{}, fmt.Errorf("suggestion gateway call: %w", err)
	}
	if resp == nil {
		return GenerateSuggestionResult{}, nil
	}
	result := GenerateSuggestionResult{
		Usage: resp.Usage,
		Model: resp.Model,
	}
	if resp.OutputText == "" {
		return result, nil
	}
	filtered, _ := FilterSuggestion(resp.OutputText)
	result.Text = filtered
	return result, nil
}

// ShouldGenerateArgs is the bundle of state the runner provides to decide
// whether to fire a suggestion call after a turn completes. Every field
// represents one independent gate — see ShouldGenerateSuggestion for the
// exact semantics.
type ShouldGenerateArgs struct {
	// Enabled is the master switch (config.Agent.PromptSuggestion.Enabled).
	Enabled bool
	// CompletedTurns is the number of assistant messages in the session so
	// far. Used together with MinTurns to skip very early turns where the
	// model has too little context to predict useful follow-ups.
	CompletedTurns int
	// MinTurns is the minimum value of CompletedTurns at which suggestions
	// may fire.
	MinTurns int
	// LastTurnUncachedTokens is input_tokens − cache_read_tokens from the
	// most recent main-turn Usage. Used together with CacheColdThresholdTokens
	// to skip suggestions when the cache is cold (otherwise the suggestion
	// call would pay full price on a large prefix).
	LastTurnUncachedTokens int
	// CacheColdThresholdTokens is the gate threshold; 0 disables the gate
	// (no cache-cold skipping).
	CacheColdThresholdTokens int
	// LastTurnHadError indicates the previous main-turn call did not
	// complete cleanly. Skip suggestion — the context may be incomplete or
	// inconsistent with what the user sees.
	LastTurnHadError bool
	// PlanMode is true when the session is in plan-mode (user is reviewing
	// an assistant-proposed plan). A prompt suggestion would be off-topic.
	PlanMode bool
}

// ShouldGenerateSuggestion returns true iff every gate passes. Returning
// false is the fail-cheap default — every gate is opt-in to firing. Gate
// order is irrelevant; this function is pure (no I/O, no state).
func ShouldGenerateSuggestion(a ShouldGenerateArgs) bool {
	if !a.Enabled {
		return false
	}
	if a.LastTurnHadError {
		return false
	}
	if a.PlanMode {
		return false
	}
	if a.CompletedTurns < a.MinTurns {
		return false
	}
	if a.CacheColdThresholdTokens > 0 && a.LastTurnUncachedTokens > a.CacheColdThresholdTokens {
		return false
	}
	return true
}
