package agent

import (
	"strings"
	"time"
)

// SystemEvent is one out-of-band fact about the agent's own outbound channel
// (a delivery failure, a membership change) that should be surfaced to the
// model on its NEXT turn for a route. Ported from OpenClaw's enqueueSystemEvent
// primitive (src/infra/system-events.ts).
//
// Events are ephemeral: they ride in the scaffolded user message as
// <system-reminder> lines and are stripped from persisted history by the
// existing first-turn scaffold strip (captureRunMessages). They are NEVER
// written to the session transcript or fed into compaction.
type SystemEvent struct {
	// Text is the human-readable line shown to the model, e.g.
	// "reply to #shannon FAILED: bot was kicked — the user did not see it".
	Text string
	// ContextKey, when non-empty, collapses an enqueue against the immediately
	// preceding event carrying the same key (the queue keeps only the newer).
	// Empty disables dedup for that event.
	ContextKey string
	// Trusted=false marks platform-derived text. It renders with a
	// "System (untrusted)" prefix. The prefix is cosmetic; SanitizeSystemEventText
	// is the real prompt-injection defense and runs on ALL event text regardless.
	Trusted bool
	// TS is the production time, rendered as [HH:MM:SS] before the text.
	TS time.Time
}

// systemEventReplacer neutralizes the characters that could either break the
// <system-reminder> XML wrapper (angle brackets) or the bracketed timestamp
// prefix (square brackets), plus newlines. This is a superset of OpenClaw's
// sanitizeEnvelopeHeaderPart (src/auto-reply/envelope.ts), which neutralizes
// only []/newlines — ShanClaw adds <> because, unlike OpenClaw, it wraps the
// block in <system-reminder> framing that platform-derived text must not escape.
var systemEventReplacer = strings.NewReplacer(
	"\r\n", " ",
	"\r", " ",
	"\n", " ",
	"[", "(",
	"]", ")",
	"<", "(",
	">", ")",
)

// SanitizeSystemEventText neutralizes framing-sensitive characters and collapses
// whitespace runs. Applied to EVERY event's text (trusted and untrusted) so a
// crafted channel/group name cannot break out of the injected block.
func SanitizeSystemEventText(s string) string {
	s = systemEventReplacer.Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	return s
}
