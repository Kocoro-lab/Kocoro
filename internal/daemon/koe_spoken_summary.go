package daemon

import (
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// Koe voice projection: for source=koe turns the model writes its full reply and
// ENDS with a <spoken_summary>…</spoken_summary> block reporting the completed
// outcome (see prompt.formatGuidance("koe")). Result-last on purpose: Koe's
// do_task result is ONLY this spoken line plus a status (SayResult) — never the
// full reply — so the sentence must describe what actually happened, which the
// model can only write after finishing the work. The daemon extracts the block
// into the spoken_summary field Koe reads aloud and strips it from every reply
// copy (the agent_reply bus event, the HTTP reply, and the persisted transcript).
//
// A missing or malformed block falls back to the mechanical makeSpokenSummary,
// which (matching the result-last convention) takes the tail line, with any stray
// tag scrubbed so a half-written "<spoken_summary>foo" is not read aloud verbatim.

const (
	spokenSummaryOpenTag  = "<spoken_summary>"
	spokenSummaryCloseTag = "</spoken_summary>"
)

// splitSpokenSummary extracts the LAST well-formed <spoken_summary>…</spoken_summary>
// block from a reply and returns the trimmed spoken text, the reply with the block
// removed, and ok=true only when the block is well-formed AND its inner text is
// non-empty. Last-occurrence (not head-anchored): the model writes the block at the
// END of its reply, but a head block still resolves (last == only occurrence). An
// unclosed opening tag returns ok=false with the reply untouched — the fallback
// scrubs the dangling token rather than guessing where the block ends.
func splitSpokenSummary(reply string) (spoken, rest string, ok bool) {
	closeIdx := strings.LastIndex(reply, spokenSummaryCloseTag)
	if closeIdx < 0 {
		return "", reply, false
	}
	openIdx := strings.LastIndex(reply[:closeIdx], spokenSummaryOpenTag)
	if openIdx < 0 {
		return "", reply, false
	}
	spoken = strings.TrimSpace(reply[openIdx+len(spokenSummaryOpenTag) : closeIdx])
	rest = strings.TrimSpace(reply[:openIdx] + reply[closeIdx+len(spokenSummaryCloseTag):])
	return spoken, rest, spoken != ""
}

// stripStraySpokenTags removes any leftover spoken_summary tag tokens and trims
// surrounding whitespace. Used on the mechanical-fallback path so a malformed or
// mid-content tag is never read aloud or persisted as visible text.
func stripStraySpokenTags(s string) string {
	s = strings.ReplaceAll(s, spokenSummaryOpenTag, "")
	s = strings.ReplaceAll(s, spokenSummaryCloseTag, "")
	return strings.TrimSpace(s)
}

// cleanSpokenTags returns text with the spoken_summary block removed. When the
// block is the ENTIRE content (a trivial task with no detail), it returns the
// spoken line itself rather than an empty string, so the persisted transcript /
// Desktop message is never blank.
func cleanSpokenTags(text string) string {
	spoken, rest, ok := splitSpokenSummary(text)
	if !ok {
		return stripStraySpokenTags(text)
	}
	if strings.TrimSpace(rest) == "" {
		return spoken
	}
	return rest
}

// projectKoeVoice turns a raw Koe reply into the spoken line + the tag-free reply
// body. authored=true means the model wrote a usable <spoken_summary> block;
// authored=false falls back to the mechanical projection over the scrubbed reply.
func projectKoeVoice(reply string) (spoken, cleaned string, authored bool) {
	if s, rest, ok := splitSpokenSummary(reply); ok {
		cleaned = strings.TrimSpace(rest)
		if cleaned == "" {
			cleaned = s // tag-only reply: keep the spoken line so Desktop isn't blank
		}
		return s, cleaned, true
	}
	scrubbed := stripStraySpokenTags(reply)
	return makeSpokenSummary(scrubbed), scrubbed, false
}

// stripSpokenSummaryFromContent removes the spoken_summary block from a message's
// text. For block content it cleans only the FIRST text block and preserves every
// other block — koe runs the default gateway+thinking path, so the persisted
// assistant message is [thinking, text] and collapsing it to plain text would drop
// the thinking block.
func stripSpokenSummaryFromContent(mc client.MessageContent) client.MessageContent {
	if !mc.HasBlocks() {
		cleaned := cleanSpokenTags(mc.Text())
		if cleaned == mc.Text() {
			return mc
		}
		return client.NewTextContent(cleaned)
	}
	blocks := mc.Blocks()
	out := make([]client.ContentBlock, len(blocks))
	copy(out, blocks)
	for i := range out {
		if out[i].Type == "text" {
			out[i].Text = cleanSpokenTags(out[i].Text)
			break
		}
	}
	return client.NewBlockContent(out)
}

// stripSpokenSummaryFromAssistants cleans the spoken_summary block out of EVERY
// assistant message in the slice, in place. Callers pass the run's freshly
// persisted messages (sess.Messages[turnBase.msgCount:]): a run that absorbed
// injected follow-ups persists more than one assistant answer, and each
// intermediate answer carries its own head <spoken_summary> tag — cleaning only
// the last would leak every earlier answer's raw tag into the transcript / FTS /
// Desktop history. Safe after applyTurnMessages because that appends COPIES of the
// loop's run messages, so mutating sess.Messages never touches the loop's
// cache-attribution slice — same shape as SanitizedRunMessages replacing content
// before persist, so no client.LogCacheCompactEvent is required.
func stripSpokenSummaryFromAssistants(messages []client.Message) {
	for i := range messages {
		if messages[i].Role != "assistant" {
			continue
		}
		messages[i].Content = stripSpokenSummaryFromContent(messages[i].Content)
	}
}
