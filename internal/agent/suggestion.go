package agent

import (
	"strings"
	"unicode"
	"unicode/utf8"
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
const SuggestionPrompt = `<system-instruction>Predict the user's most likely next message in this conversation. Respond with ONLY that next message — 2 to 12 words, matching the user's tone and language. No quotes, no preamble, no explanation. If you cannot confidently predict a useful next message, respond with exactly the word "skip".</system-instruction>`

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
