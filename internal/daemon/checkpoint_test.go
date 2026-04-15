package daemon

import (
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// usageStub fulfills agent.UsageProvider for applyTurnUsage tests.
type usageStub struct{ usage agent.AccumulatedUsage }

func (u *usageStub) Usage() agent.AccumulatedUsage { return u.usage }

// checkpointTestLoop exposes a way to inject run messages without a live
// agent loop, for unit-testing applyRunMessagesToSession's idempotency.
type checkpointTestLoop struct {
	*agent.AgentLoop
	msgs []client.Message
}

// We directly construct a real AgentLoop, then use its public
// RunMessages(). Since that getter reads from internal state only set
// inside Run(), we fall back to constructing a test harness below.

// Here we just exercise applyRunMessagesToSession directly with a hand-
// built session and fake loop-messages. The function is the idempotency
// linchpin, so it deserves direct coverage.
func TestApplyTurnMessages_Idempotent(t *testing.T) {
	// Baseline: session with system + one pre-loop user message already.
	sess := &session.Session{
		ID: "sess-1",
		Messages: []client.Message{
			{Role: "system", Content: client.NewTextContent("system")},
			{Role: "user", Content: client.NewTextContent("hello")},
		},
		MessageMeta: []session.MessageMeta{
			{Source: "web"},
			{Source: "web", Timestamp: session.TimePtr(time.Now())},
		},
	}
	base := captureTurnBaseline(sess, "web", true)

	loop := agent.NewAgentLoop(nil, agent.NewToolRegistry(), "m", "", 1, 1, 1, nil, nil, nil)

	// Round 1.
	agent.SetRunMessagesForTest(loop, []client.Message{
		{Role: "user", Content: client.NewTextContent("hello")},
		{Role: "assistant", Content: client.NewTextContent("call tool")},
		{Role: "user", Content: client.NewTextContent("tool result")},
	})
	applyTurnMessages(sess, loop, base)
	if got := len(sess.Messages); got != base.msgCount+2 {
		t.Fatalf("round 1: want %d msgs, got %d", base.msgCount+2, got)
	}

	// Round 2.
	agent.SetRunMessagesForTest(loop, []client.Message{
		{Role: "user", Content: client.NewTextContent("hello")},
		{Role: "assistant", Content: client.NewTextContent("call tool 1")},
		{Role: "user", Content: client.NewTextContent("result 1")},
		{Role: "assistant", Content: client.NewTextContent("call tool 2")},
		{Role: "user", Content: client.NewTextContent("result 2")},
	})
	applyTurnMessages(sess, loop, base)
	if got := len(sess.Messages); got != base.msgCount+4 {
		t.Fatalf("round 2: want %d msgs, got %d", base.msgCount+4, got)
	}

	// Round 3: compaction shrink.
	agent.SetRunMessagesForTest(loop, []client.Message{
		{Role: "user", Content: client.NewTextContent("hello")},
		{Role: "assistant", Content: client.NewTextContent("compacted summary")},
	})
	applyTurnMessages(sess, loop, base)
	if got := len(sess.Messages); got != base.msgCount+1 {
		t.Fatalf("round 3 (compaction): want %d msgs, got %d", base.msgCount+1, got)
	}
	if len(sess.Messages) != len(sess.MessageMeta) {
		t.Fatalf("meta drift: %d vs %d", len(sess.Messages), len(sess.MessageMeta))
	}
	if sess.Messages[0].Role != "system" || sess.Messages[1].Role != "user" {
		t.Fatalf("baseline corrupted")
	}
}

// Regression for finding #1: a turn that produces a mid-turn checkpoint
// followed by a final save must end with ONE canonical transcript, not
// a duplicated one. Both paths share applyTurnMessages + the same baseline
// so iteration count is irrelevant.
func TestApplyTurnMessages_CheckpointThenFinalSave_NoDuplicate(t *testing.T) {
	sess := &session.Session{
		Messages: []client.Message{
			{Role: "system", Content: client.NewTextContent("sys")},
			{Role: "user", Content: client.NewTextContent("hi")},
		},
		MessageMeta: []session.MessageMeta{{Source: "web"}, {Source: "web"}},
	}
	base := captureTurnBaseline(sess, "web", true)
	loop := agent.NewAgentLoop(nil, agent.NewToolRegistry(), "m", "", 1, 1, 1, nil, nil, nil)

	// Simulate: tool batch completes → checkpoint fires.
	agent.SetRunMessagesForTest(loop, []client.Message{
		{Role: "user", Content: client.NewTextContent("hi")},
		{Role: "assistant", Content: client.NewTextContent("[tool_use]")},
		{Role: "user", Content: client.NewTextContent("[tool_result]")},
	})
	applyTurnMessages(sess, loop, base) // mid-turn checkpoint

	// Turn completes: final text appended to RunMessages.
	agent.SetRunMessagesForTest(loop, []client.Message{
		{Role: "user", Content: client.NewTextContent("hi")},
		{Role: "assistant", Content: client.NewTextContent("[tool_use]")},
		{Role: "user", Content: client.NewTextContent("[tool_result]")},
		{Role: "assistant", Content: client.NewTextContent("final answer")},
	})
	applyTurnMessages(sess, loop, base) // final save

	// Expected: baseline(2) + 3 post-user messages = 5. No duplicates.
	if got := len(sess.Messages); got != 5 {
		t.Fatalf("expected 5 messages (2 baseline + 3 turn), got %d", got)
	}
	// Check the sequence has exactly one [tool_use] and one [tool_result].
	var countToolUse, countToolResult, countFinal int
	for _, m := range sess.Messages {
		switch m.Content.Text() {
		case "[tool_use]":
			countToolUse++
		case "[tool_result]":
			countToolResult++
		case "final answer":
			countFinal++
		}
	}
	if countToolUse != 1 || countToolResult != 1 || countFinal != 1 {
		t.Fatalf("duplicated transcript: tool_use=%d tool_result=%d final=%d",
			countToolUse, countToolResult, countFinal)
	}
}

// Regression for hard-error-after-checkpoint: a non-soft failure after
// one or more successful mid-turn checkpoints must NOT duplicate the
// transcript (checkpoint already persisted it) and must NOT double-count
// usage (additive AddUsage on top of already-folded usage was the bug).
// This test mirrors the runner's hard-error path inline.
func TestApplyTurnState_HardErrorAfterCheckpoint_NoDuplicate(t *testing.T) {
	sess := &session.Session{
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("do thing")},
		},
		MessageMeta: []session.MessageMeta{{Source: "web"}},
		Usage:       &session.UsageSummary{InputTokens: 100, LLMCalls: 1},
	}
	base := captureTurnBaseline(sess, "web", true)
	loop := agent.NewAgentLoop(nil, agent.NewToolRegistry(), "m", "", 1, 1, 1, nil, nil, nil)
	up := &usageStub{usage: agent.AccumulatedUsage{
		LLM: agent.TurnUsage{InputTokens: 50, LLMCalls: 1},
	}}

	// Step 1: mid-turn checkpoint after a successful tool batch.
	agent.SetRunMessagesForTest(loop, []client.Message{
		{Role: "user", Content: client.NewTextContent("do thing")},
		{Role: "assistant", Content: client.NewTextContent("[tool_use]")},
		{Role: "user", Content: client.NewTextContent("[tool_result]")},
	})
	applyTurnMessages(sess, loop, base)
	applyTurnUsage(sess, up, base)
	// Sanity: 1 baseline + 2 turn msgs = 3. Usage: 100+50=150.
	if len(sess.Messages) != 3 {
		t.Fatalf("after checkpoint: want 3 msgs, got %d", len(sess.Messages))
	}
	if sess.Usage.InputTokens != 150 {
		t.Fatalf("after checkpoint: want 150 input tokens, got %d", sess.Usage.InputTokens)
	}

	// Step 2: hard error fires. The runner's hard-error path rebuilds
	// from baseline + current RunMessages, appends a friendly error stub,
	// then applies usage. The accumulator has grown slightly (e.g., one
	// more failed LLM call).
	up.usage.LLM.InputTokens = 70 // +20 since checkpoint
	up.usage.LLM.LLMCalls = 2
	applyTurnMessages(sess, loop, base)
	sess.Messages = append(sess.Messages,
		client.Message{Role: "assistant", Content: client.NewTextContent("Sorry, something failed.")},
	)
	sess.MessageMeta = append(sess.MessageMeta,
		session.MessageMeta{Source: "web", Timestamp: session.TimePtr(time.Now())},
	)
	applyTurnUsage(sess, up, base)

	// Expected: 1 baseline + 2 turn + 1 error stub = 4 total. No duplicates.
	if len(sess.Messages) != 4 {
		t.Fatalf("after hard error: want 4 msgs (1 baseline + 2 turn + 1 error), got %d", len(sess.Messages))
	}
	// Usage: 100 baseline + 70 current = 170. NOT 100+50+70=220 (double-count).
	if sess.Usage.InputTokens != 170 {
		t.Fatalf("after hard error: want 170 input tokens (baseline+current), got %d (double-counted)", sess.Usage.InputTokens)
	}
	if sess.Usage.LLMCalls != 3 {
		t.Fatalf("after hard error: want 3 LLMCalls (1 baseline + 2 current), got %d", sess.Usage.LLMCalls)
	}
	// Duplicate scan: exactly one tool_use and one tool_result.
	var toolUse, toolResult, errStub int
	for _, m := range sess.Messages {
		switch m.Content.Text() {
		case "[tool_use]":
			toolUse++
		case "[tool_result]":
			toolResult++
		case "Sorry, something failed.":
			errStub++
		}
	}
	if toolUse != 1 || toolResult != 1 || errStub != 1 {
		t.Fatalf("duplicate in hard-error path: tool_use=%d tool_result=%d err=%d",
			toolUse, toolResult, errStub)
	}
}

// Regression for finding #3: usage survives mid-turn checkpoint + final
// save without being double-counted. Baseline + current accumulator is
// the authoritative value at every save.
func TestApplyTurnUsage_IdempotentAcrossCheckpointAndFinalSave(t *testing.T) {
	sess := &session.Session{Usage: &session.UsageSummary{
		InputTokens: 100, OutputTokens: 50, TotalTokens: 150, LLMCalls: 1,
	}}
	base := captureTurnBaseline(sess, "web", false)
	up := &usageStub{usage: agent.AccumulatedUsage{
		LLM: agent.TurnUsage{InputTokens: 20, OutputTokens: 10, TotalTokens: 30, LLMCalls: 1},
	}}

	// First call: mid-turn checkpoint after first LLM call.
	applyTurnUsage(sess, up, base)
	if sess.Usage.InputTokens != 120 || sess.Usage.OutputTokens != 60 || sess.Usage.LLMCalls != 2 {
		t.Fatalf("after checkpoint: %+v", sess.Usage)
	}

	// Second call: accumulator grew (second LLM call). Final save uses
	// the SAME baseline — must not double-count the first call.
	up.usage = agent.AccumulatedUsage{
		LLM: agent.TurnUsage{InputTokens: 40, OutputTokens: 20, TotalTokens: 60, LLMCalls: 2},
	}
	applyTurnUsage(sess, up, base)
	// Expected: baseline(100/50/1) + current(40/20/2) = 140/70/3
	if sess.Usage.InputTokens != 140 || sess.Usage.OutputTokens != 70 || sess.Usage.LLMCalls != 3 {
		t.Fatalf("after final save (double-count regression): %+v", sess.Usage)
	}
}

func TestSessionInProgress_FlagCycles(t *testing.T) {
	sess := &session.Session{}
	if sess.InProgress {
		t.Fatal("fresh session should not be InProgress")
	}
	sess.InProgress = true
	sess.InProgress = false
	if sess.InProgress {
		t.Fatal("toggle off didn't clear")
	}
}
