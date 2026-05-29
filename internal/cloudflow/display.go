package cloudflow

// CloudStatusLine formats a cloud agent event for a TERMINAL consumer (the TUI
// and the one-shot CLI) that has no separate label column. It re-applies the
// "[agentID] " prefix the daemon no longer bakes into the wire message (see
// dispatch.go — the message is now presentation-free), and substitutes an
// English fallback when cloud sent an empty message.
//
// Structured consumers (e.g. Kocoro Desktop) must NOT use this — they receive
// agent_id / status / message as separate fields and render the label
// themselves, with their own localized fallback.
func CloudStatusLine(agentID, status, message string) string {
	msg := message
	if msg == "" {
		msg = cloudStatusFallback(status)
	}
	// Deny-list, not allow-list: cloud's other non-nickname structured IDs
	// (final_output, synthesis, title_generator, swarm-lead) don't reach a
	// started/completed/thinking/tool OnCloudAgent call on the research/DAG
	// path, so only orchestrator/streaming (ControlSignalHandler IDs that ride
	// the dropped PROGRESS/DELEGATION events) need excluding here. Revisit if
	// cloud ever emits AGENT_* with one of those IDs.
	if agentID != "" && agentID != "orchestrator" && agentID != "streaming" {
		return "[" + agentID + "] " + msg
	}
	return msg
}

// cloudStatusFallback is the terminal English text for an empty cloud message,
// keyed by status. Mirrors the labels the daemon used to bake in before the
// wire message was made presentation-free.
func cloudStatusFallback(status string) string {
	switch status {
	case "started":
		return "Agent working..."
	case "completed":
		return "Agent completed"
	case "thinking":
		return "Thinking..."
	case "tool":
		return "Calling tool..."
	case "processing":
		return "Processing data..."
	default:
		return "Working..."
	}
}
