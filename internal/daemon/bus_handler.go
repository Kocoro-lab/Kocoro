package daemon

import (
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/audit"
)

// busEventHandler implements agent.EventHandler (and agent.RunStatusHandler)
// by forwarding callbacks to deps.EventBus. It is intended to be composed
// with a transport-specific handler (sseEventHandler or daemonEventHandler)
// via multiHandler so bus emission is exactly-once regardless of transport.
type busEventHandler struct {
	deps      *ServerDeps
	sessionID string
	agent     string
}

// SetSessionID captures the session ID post-session-resolution so every bus
// event carries it. Matches the optional interface checked by RunAgent.
func (h *busEventHandler) SetSessionID(id string) { h.sessionID = id }

// OnText is a no-op on the bus path: final-answer text reaches Desktop via
// EventAgentReply (emitted by runner.go), so duplicating it on EventAssistantText
// would render the same answer twice in the client. Mid-turn preamble flows
// through OnPreamble below.
func (h *busEventHandler) OnText(text string) {}

// OnPreamble emits mid-turn agent narration (preamble emitted alongside native
// tool_use blocks) to the bus so SSE subscribers (Desktop, web UIs) can render
// the text inline between tool calls. Empty text is dropped defensively even
// though the agent loop already filters serialized-tool-call pseudo-preambles.
func (h *busEventHandler) OnPreamble(text string) {
	if text == "" {
		return
	}
	h.emitJSON(EventAssistantText, map[string]any{
		"text":       text,
		"session_id": h.sessionID,
		"agent":      h.agent,
		"ts":         nowISO(),
	})
}

// No-op passthroughs — bus emits progress signals + agent narration above, not token deltas.
func (h *busEventHandler) OnStreamDelta(delta string)              {}
func (h *busEventHandler) OnApprovalNeeded(tool, args string) bool { return false }

// OnToolCall emits a "running" event when a tool is invoked. Args are redacted
// (secrets) and truncated (size budget) before emission.
func (h *busEventHandler) OnToolCall(name, args string) {
	h.emitJSON(EventToolStatus, map[string]any{
		"tool":       name,
		"status":     "running",
		"args":       redactAndTruncate(args, 200),
		"session_id": h.sessionID,
		"ts":         nowISO(),
	})
}

// OnToolResult emits a "completed" event when a tool finishes. The result
// preview is extracted from Content or ContentBlocks, redacted, and truncated.
func (h *busEventHandler) OnToolResult(name, args string, result agent.ToolResult, elapsed time.Duration) {
	h.emitJSON(EventToolStatus, map[string]any{
		"tool":       name,
		"status":     "completed",
		"elapsed":    elapsed.Seconds(),
		"is_error":   result.IsError,
		"preview":    redactAndTruncate(toolResultPreview(result), 200),
		"session_id": h.sessionID,
		"ts":         nowISO(),
	})
}
func (h *busEventHandler) OnUsage(u agent.TurnUsage) {
	h.emitJSON(EventUsage, map[string]any{
		"input_tokens":       u.InputTokens,
		"output_tokens":      u.OutputTokens,
		"cache_read_tokens":  u.CacheReadTokens,
		"cache_write_tokens": u.CacheCreationTokens,
		"cost_usd":           u.CostUSD,
		"llm_calls":          u.LLMCalls,
		"model":              u.Model,
		"session_id":         h.sessionID,
		"ts":                 nowISO(),
	})
}

const maxCloudPlanContent = 2048

func (h *busEventHandler) OnCloudAgent(agentID, status, message string) {
	h.emitJSON(EventCloudAgent, map[string]any{
		"agent_id":   agentID,
		"status":     status,
		"message":    audit.RedactSecrets(message),
		"session_id": h.sessionID,
	})
}

func (h *busEventHandler) OnCloudProgress(completed, total int) {
	h.emitJSON(EventCloudProgress, map[string]any{
		"completed":  completed,
		"total":      total,
		"session_id": h.sessionID,
	})
}

func (h *busEventHandler) OnCloudPlan(planType, content string, needsReview bool) {
	// Redact first (regex windows must see full content), then truncate on
	// a rune boundary via the shared helper before appending the marker.
	redacted := audit.RedactSecrets(content)
	if len(redacted) > maxCloudPlanContent {
		redacted = truncate(redacted, maxCloudPlanContent) + "… (truncated)"
	}
	h.emitJSON(EventCloudPlan, map[string]any{
		"type":         planType,
		"content":      redacted,
		"needs_review": needsReview,
		"session_id":   h.sessionID,
	})
}

func (h *busEventHandler) OnRunStatus(code, detail string) {
	h.emitJSON(EventRunStatus, map[string]any{
		"code":       code,
		"detail":     detail,
		"session_id": h.sessionID,
		"agent":      h.agent,
	})
}

// truncate returns s truncated to at most max bytes, but never slicing
// mid-rune — a byte-cut inside a multi-byte UTF-8 sequence produces invalid
// UTF-8 that JSON marshalling and downstream consumers (Desktop WKWebView,
// browsers subscribing to /events) reject or render as U+FFFD. We still
// cap on bytes (ring buffer capacity is byte-budgeted) but step back so
// the cut lands at a rune boundary. RuneStart returns true for ASCII and
// first bytes of multi-byte sequences; continuation bytes (0b10xxxxxx)
// fail the check, so we step past them. Max 3 decrements (UTF-8 runes are
// ≤ 4 bytes), so this is O(1) regardless of the string prefix contents.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// redactAndTruncate applies audit.RedactSecrets first (so regex windows see
// the full content) then truncates. Used for tool_status args/preview
// and cloud_agent.message.
func redactAndTruncate(s string, max int) string {
	return truncate(audit.RedactSecrets(s), max)
}

// emitJSON marshals payload and emits to the bus, tolerating marshal errors
// silently (bus is non-critical path; missing events are preferable to panics).
func (h *busEventHandler) emitJSON(eventType string, payload any) {
	if h.deps == nil || h.deps.EventBus == nil {
		return
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	h.deps.EventBus.Emit(Event{Type: eventType, Payload: data})
}

// nowISO returns the current wall time in RFC3339 for the `ts` field.
func nowISO() string { return time.Now().UTC().Format(time.RFC3339) }

// toolResultPreview returns a plain-text preview of the tool result. Prefers
// the canonical `Content` string; falls back to concatenating text from the
// structured `ContentBlocks` when `Content` is empty (some tools only populate
// the structured path). The full result is NEVER included — bus is a
// notification channel; Desktop pulls full content via the session endpoints.
func toolResultPreview(r agent.ToolResult) string {
	if r.Content != "" {
		return r.Content
	}
	var b strings.Builder
	for _, block := range r.ContentBlocks {
		if block.Type == "text" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(block.Text)
			if b.Len() >= 200 {
				break
			}
		}
	}
	return b.String()
}
