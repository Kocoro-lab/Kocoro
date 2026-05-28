package daemon

import "strings"

// buildStickyContext composes the per-run metadata block injected into the
// LLM context. Always emits an Agent: line — "default" when agentName is
// empty — so the model knows which Kocoro agent identity it's running as.
// imBindings is the formatted `<agent>=<type>:<channel>; ...` line from
// formatIMBindings; pass "" to omit the IM bindings sticky line entirely
// (no bindings, or Cloud fetch failed — both legitimate "unknown" states).
// Extra is an optional caller-provided block appended verbatim.
//
// Returns "" when every routing input is empty. Pre-PR pure-local runs
// (TUI / one-shot CLI without source/channel/sender/agentName/imBindings)
// had no sticky context at all; preserving that lets the runner.go
// `if sticky != "" { loop.SetStickyContext(sticky) }` guard short-circuit
// for those, which in turn keeps cache equivalence against pre-PR sessions
// that resume across the upgrade boundary.
func buildStickyContext(source, channel, sender, agentName, imBindings, extra string) string {
	if source == "" && channel == "" && sender == "" && agentName == "" && imBindings == "" && extra == "" {
		return ""
	}
	var parts []string
	if source != "" {
		parts = append(parts, "Source: "+source)
	}
	if channel != "" {
		parts = append(parts, "Channel: "+channel)
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
	if extra != "" {
		parts = append(parts, extra)
	}
	return strings.Join(parts, "\n")
}
