package daemon

import (
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// buildStickyContext composes the per-run metadata block injected into the
// LLM context. Always emits an Agent: line — "default" when agentName is
// empty — so the model knows which Kocoro agent identity it's running as.
// imBindings is the formatted `<agent>=<type>:<channel>; ...` line from
// formatIMBindings; pass "" to omit the IM bindings sticky line entirely
// (no bindings, or Cloud fetch failed — both legitimate "unknown" states).
// Extra is an optional caller-provided block appended verbatim.
//
// origin, when non-nil, supplies the specific channel/group/DM + thread the
// inbound message came from (S1); it upgrades the coarse `Channel: <channel>`
// line to e.g. `Channel: slack · #shannon · channel` plus a `Thread:` line.
// Pass nil for non-IM runs or platforms whose blob lacks chat identity (Lark
// pre-S1b) — the coarse line is used.
//
// connState, when non-empty, is a live connection/membership status line for
// this run's channel (S3), e.g. "the bot was removed from this channel ...".
// Rendered as a `Connection:` line; empty omits it.
//
// Returns "" when every routing input is empty. Pre-PR pure-local runs
// (TUI / one-shot CLI without source/channel/sender/agentName/imBindings)
// had no sticky context at all; preserving that lets the runner.go
// `if sticky != "" { loop.SetStickyContext(sticky) }` guard short-circuit
// for those, which in turn keeps cache equivalence against pre-PR sessions
// that resume across the upgrade boundary.
func buildStickyContext(source, channel, sender, agentName, imBindings, extra string, origin *MessageOrigin, connState string) string {
	if source == "" && channel == "" && sender == "" && agentName == "" && imBindings == "" && extra == "" && origin == nil && connState == "" {
		return ""
	}
	var parts []string
	if source != "" {
		parts = append(parts, "Source: "+source)
	}
	if origin != nil && origin.ChannelID != "" {
		parts = append(parts, "Channel: "+origin.renderChannelLine())
		if origin.ThreadID != "" {
			parts = append(parts, "Thread: "+agent.SanitizeSystemEventText(origin.ThreadID))
		}
	} else if channel != "" {
		parts = append(parts, "Channel: "+channel)
	}
	if connState != "" {
		parts = append(parts, "Connection: "+connState)
	}
	if sender != "" {
		parts = append(parts, "Sender: "+sender)
	}
	// Always emit Agent: even for the default (empty) case. The LLM needs
	// this to reason "I am the agent the Cloud router delivered this to".
	if agentName == "" {
		parts = append(parts, "Agent: default")
	} else {
		parts = append(parts, "Agent: "+agentName)
	}
	if imBindings != "" {
		parts = append(parts, "IM bindings: "+imBindings)
	}
	// Output discipline for scheduled runs. The system prompt already explains
	// HOW delivery works (auto-broadcast, no send tool); the failure mode here
	// is the agent NARRATING that to the user — scheduled replies were leaking
	// "there's no Slack send tool, this auto-broadcasts to <channel>" plus
	// session-index / "based on history" bookkeeping into the user-facing
	// message. The agent may still search/reason internally; it just must not
	// surface any of it. Scoped to schedule runs so interactive/IM turns are
	// byte-stable.
	if source == ChannelSchedule {
		parts = append(parts, "This run is a scheduled task. Your final reply IS the message "+
			"delivered to the user (auto-broadcast to the originating channel when the "+
			"schedule's broadcast applies); there is no separate send step or send tool. "+
			"Output ONLY the user-facing message: do not explain how it will be delivered, "+
			"do not mention tools / broadcast / Source / channel routing, and do not expose "+
			"internal details (session indices, your search steps, or \"based on history\" "+
			"narration). Produce just the deliverable, as if speaking directly to the user.")
	}
	if extra != "" {
		parts = append(parts, extra)
	}
	return strings.Join(parts, "\n")
}
