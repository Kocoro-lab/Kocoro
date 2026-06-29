package daemon

import (
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// multiHandler fans agent.EventHandler callbacks to multiple wrapped handlers.
//
// Propagation rules:
//   - Base methods (OnToolCall, OnText, OnUsage, etc.): call every wrapped handler in order.
//   - OnApprovalNeeded: call every wrapped handler; return the OR of all results.
//     This means any handler returning "approved" approves the call.
//   - SetSessionID (Task 8): propagate only to wrapped handlers that implement it.
//   - OnRunStatus (Task 9): propagate only to wrapped handlers that implement RunStatusHandler.
type multiHandler struct {
	handlers []agent.EventHandler
}

func (m *multiHandler) OnToolCall(name, args, toolUseID string) {
	for _, h := range m.handlers {
		h.OnToolCall(name, args, toolUseID)
	}
}

func (m *multiHandler) OnToolResult(name, args, toolUseID string, result agent.ToolResult, elapsed time.Duration) {
	// Defense in depth: agent.ToolResult.InternalOnly results are addressed
	// to the LLM (transcript / tool_use-result pairing) and must never reach
	// SSE / WS clients. The agent loop already skips OnToolResult for these,
	// so under normal flow this branch is never taken — but pinning the
	// invariant here keeps the contract local rather than ambient, so any
	// future force-stop site or checkpoint-replay path that forgets to
	// suppress the dispatch cannot leak a synthetic [output_truncated] card
	// to the user.
	if result.InternalOnly {
		return
	}
	for _, h := range m.handlers {
		h.OnToolResult(name, args, toolUseID, result, elapsed)
	}
}

func (m *multiHandler) OnText(text string) {
	for _, h := range m.handlers {
		h.OnText(text)
	}
}

func (m *multiHandler) OnPreamble(text string) {
	for _, h := range m.handlers {
		h.OnPreamble(text)
	}
}

func (m *multiHandler) OnStreamDelta(delta string) {
	for _, h := range m.handlers {
		h.OnStreamDelta(delta)
	}
}

func (m *multiHandler) OnApprovalNeeded(tool, args string) bool {
	approved := false
	for _, h := range m.handlers {
		if h.OnApprovalNeeded(tool, args) {
			approved = true
		}
	}
	return approved
}

func (m *multiHandler) OnUsage(u agent.TurnUsage) {
	for _, h := range m.handlers {
		h.OnUsage(u)
	}
}

func (m *multiHandler) OnCloudAgent(agentID, status, message string) {
	for _, h := range m.handlers {
		h.OnCloudAgent(agentID, status, message)
	}
}

func (m *multiHandler) OnCloudProgress(completed, total int) {
	for _, h := range m.handlers {
		h.OnCloudProgress(completed, total)
	}
}

func (m *multiHandler) OnCloudPlan(planType, content string, needsReview bool) {
	for _, h := range m.handlers {
		h.OnCloudPlan(planType, content, needsReview)
	}
}

// SetSessionID propagates the session ID to every wrapped handler that
// implements the optional interface. Handlers that don't implement it are
// skipped silently — matching how RunAgent itself type-asserts the top-level
// handler (runner.go SetSessionID injection path).
func (m *multiHandler) SetSessionID(id string) {
	for _, h := range m.handlers {
		if setter, ok := h.(interface{ SetSessionID(string) }); ok {
			setter.SetSessionID(id)
		}
	}
}

// OnRunStatus propagates watchdog/retry events to wrapped handlers that
// implement agent.RunStatusHandler. The method is present on multiHandler
// itself (even though it's optional for arbitrary EventHandlers) so that
// the agent loop's type assertion `a.handler.(RunStatusHandler)` succeeds
// when the loop handler is a multiHandler.
func (m *multiHandler) OnRunStatus(code, detail string) {
	for _, h := range m.handlers {
		if rsh, ok := h.(agent.RunStatusHandler); ok {
			rsh.OnRunStatus(code, detail)
		}
	}
}

// OnInjectedCommitted propagates the mid-run inject "committed" event to wrapped
// handlers that implement agent.InjectCommitHandler (the per-request SSE handler
// does). Present on multiHandler itself so the agent loop's type assertion
// `a.handler.(InjectCommitHandler)` succeeds when the loop handler is a
// multiHandler — without this, the wrapped SSE handler never sees the event.
func (m *multiHandler) OnInjectedCommitted(clientMessageID, text string) {
	for _, h := range m.handlers {
		if ich, ok := h.(agent.InjectCommitHandler); ok {
			ich.OnInjectedCommitted(clientMessageID, text)
		}
	}
}

// Usage forwards the accumulated usage snapshot from the first wrapped handler
// that implements agent.UsageProvider (in production that's the transport
// handler — daemon WS / SSE / HTTP / schedule — which embeds an
// agent.UsageAccumulator; the bus handler does not). Present on multiHandler
// itself so RunAgent's `handler.(agent.UsageProvider)` assertions succeed when
// the loop handler is a *multiHandler. Without this, BOTH usage resolvers go
// dead: computeReportedUsage silently takes the loop-TurnUsage fallback, and —
// the load-bearing failure — applyTurnUsage receives a nil provider and never
// writes sess.Usage (that persisted field has no fallback, so it stayed nil at
// schema_version 1 on every daemon-routed session). See issue #196.
func (m *multiHandler) Usage() agent.AccumulatedUsage {
	for _, h := range m.handlers {
		if up, ok := h.(agent.UsageProvider); ok {
			return up.Usage()
		}
	}
	return agent.AccumulatedUsage{}
}

// OnIntermediateAnswer propagates a superseded turn's final answer to wrapped
// handlers that implement agent.IntermediateAnswerHandler (the daemon WS handler
// does, completing that turn's own channel reply addressed to cloudMessageID).
// Present on multiHandler itself so the agent loop's type assertion
// `a.handler.(IntermediateAnswerHandler)` succeeds when the loop handler is a
// multiHandler — without this the wrapped daemon handler never sees the event,
// and a turn's answer is dropped from the IM channel whenever an injected
// follow-up merges turns (the "first reply went missing" bug).
func (m *multiHandler) OnIntermediateAnswer(text, cloudMessageID string) {
	for _, h := range m.handlers {
		if iah, ok := h.(agent.IntermediateAnswerHandler); ok {
			iah.OnIntermediateAnswer(text, cloudMessageID)
		}
	}
}
