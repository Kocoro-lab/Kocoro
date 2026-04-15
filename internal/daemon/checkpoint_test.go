package daemon

import (
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

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
func TestApplyRunMessagesToSession_Idempotent(t *testing.T) {
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
	baselineMsgs := len(sess.Messages)
	baselineMeta := len(sess.MessageMeta)

	// Simulate an AgentLoop whose RunMessages returns a growing transcript
	// by swapping the underlying runMessages field through successive calls.
	loop := agent.NewAgentLoop(nil, agent.NewToolRegistry(), "m", "", 1, 1, 1, nil, nil, nil)

	// Round 1: one tool-use pair beyond the user prompt.
	agent.SetRunMessagesForTest(loop, []client.Message{
		{Role: "user", Content: client.NewTextContent("hello")},
		{Role: "assistant", Content: client.NewTextContent("call tool")},
		{Role: "user", Content: client.NewTextContent("tool result")},
	})
	applyRunMessagesToSession(sess, loop, "web", baselineMsgs, baselineMeta, true)
	if got := len(sess.Messages); got != baselineMsgs+2 {
		t.Fatalf("round 1: want %d msgs, got %d", baselineMsgs+2, got)
	}

	// Round 2: two tool pairs. Idempotent rebuild should produce the
	// expected shape regardless of prior state.
	agent.SetRunMessagesForTest(loop, []client.Message{
		{Role: "user", Content: client.NewTextContent("hello")},
		{Role: "assistant", Content: client.NewTextContent("call tool 1")},
		{Role: "user", Content: client.NewTextContent("result 1")},
		{Role: "assistant", Content: client.NewTextContent("call tool 2")},
		{Role: "user", Content: client.NewTextContent("result 2")},
	})
	applyRunMessagesToSession(sess, loop, "web", baselineMsgs, baselineMeta, true)
	if got := len(sess.Messages); got != baselineMsgs+4 {
		t.Fatalf("round 2: want %d msgs, got %d", baselineMsgs+4, got)
	}

	// Round 3: simulate compaction shrinking RunMessages back to 1 pair.
	// This is the critical case — a naive append-diff would leave stale
	// entries from round 2 behind.
	agent.SetRunMessagesForTest(loop, []client.Message{
		{Role: "user", Content: client.NewTextContent("hello")},
		{Role: "assistant", Content: client.NewTextContent("compacted summary")},
	})
	applyRunMessagesToSession(sess, loop, "web", baselineMsgs, baselineMeta, true)
	if got := len(sess.Messages); got != baselineMsgs+1 {
		t.Fatalf("round 3 (after compaction): want %d msgs, got %d — not idempotent", baselineMsgs+1, got)
	}

	// Metadata must track messages (no drift).
	if len(sess.Messages) != len(sess.MessageMeta) {
		t.Fatalf("meta drift: %d messages vs %d meta", len(sess.Messages), len(sess.MessageMeta))
	}

	// Baseline must never be touched.
	if sess.Messages[0].Role != "system" || sess.Messages[1].Role != "user" {
		t.Fatalf("baseline corrupted: %+v", sess.Messages[:2])
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
