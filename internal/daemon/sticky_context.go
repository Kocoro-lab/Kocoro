package daemon

import "strings"

// buildStickyContext composes the per-run metadata block injected into the
// LLM context. Always emits an Agent: line — "default" when agentName is
// empty — so the model knows which Kocoro agent identity it's running as.
// Extra is an optional caller-provided block appended verbatim.
func buildStickyContext(source, channel, sender, agentName, extra string) string {
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
	if extra != "" {
		parts = append(parts, extra)
	}
	return strings.Join(parts, "\n")
}
