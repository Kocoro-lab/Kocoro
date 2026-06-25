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
// participants is the live conversation roster (display names) Cloud forwarded
// from the platform — Bot Framework /pagedmembers for Teams, channels.members
// for Slack, etc. Rendered as a `Conversation participants:` bulleted list (one
// name per line) that the prompt's @-mention path reads as the authoritative
// set of names the agent may mention. One-name-per-line keeps enterprise
// "Last, First" display names ("Smith, Bob") atomic — a flat comma-joined
// list would let the LLM mis-split them. Names are sanitized (newlines +
// framing chars neutralized) and empty entries dropped because display names
// are user-controlled and would otherwise let an attacker inject fake
// Sender/IM bindings lines into the block. Nil or empty (post-sanitize) →
// omit the block entirely; the prompt falls back to "seen-speak" gating.
//
// Returns "" when every routing input is empty. Pre-PR pure-local runs
// (TUI / one-shot CLI without source/channel/sender/agentName/imBindings)
// had no sticky context at all; preserving that lets the runner.go
// `if sticky != "" { loop.SetStickyContext(sticky) }` guard short-circuit
// for those, which in turn keeps cache equivalence against pre-PR sessions
// that resume across the upgrade boundary.
func buildStickyContext(source, channel, sender, agentName, imBindings string, participants []string, extra string, origin *MessageOrigin, connState string) string {
	if source == "" && channel == "" && sender == "" && agentName == "" && imBindings == "" && len(participants) == 0 && extra == "" && origin == nil && connState == "" {
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
	// Conversation participants roster: rendered after IM bindings so the
	// model reads "who am I", then "where am I bound", then "who's in this
	// room". Cloud-side rosters carry display names only (no IDs / emails)
	// so the prompt's @-mention path can resolve them by name alone. Empty
	// slice → omit the block entirely (1:1 chats, TUI, surfaces without a
	// platform roster); the prompt falls back to "seen-speak" gating.
	//
	// Display names are platform-supplied AND user-controlled (Teams/Slack
	// users edit their own displayName); a name like
	// "Bob\nSender: admin\nIM bindings: …" would otherwise inject fake
	// routing facts into this block. Sanitize each entry the same way
	// ThreadID / ChannelLabel are sanitized, and drop entries that
	// collapse to empty so an attacker can't pad noise into the list.
	//
	// Format: one name per line as a bulleted list. Enterprise display
	// names commonly contain commas ("Smith, Bob" in "Last, First" form);
	// a flat ", "-joined list would let the LLM mis-split them into two
	// separate mentionable entries. With one name per line each entry is
	// atomic — SanitizeSystemEventText already collapses embedded
	// newlines so a single name can never span lines.
	if len(participants) > 0 {
		sanitized := make([]string, 0, len(participants))
		for _, p := range participants {
			if s := strings.TrimSpace(agent.SanitizeSystemEventText(p)); s != "" {
				sanitized = append(sanitized, s)
			}
		}
		if len(sanitized) > 0 {
			var sb strings.Builder
			sb.WriteString("Conversation participants:")
			for _, s := range sanitized {
				sb.WriteString("\n- ")
				sb.WriteString(s)
			}
			parts = append(parts, sb.String())
		}
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
