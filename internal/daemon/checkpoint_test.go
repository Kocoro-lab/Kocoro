package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	ctxwin "github.com/Kocoro-lab/ShanClaw/internal/context"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// usageStub fulfills agent.UsageProvider for applyTurnUsage tests.
type usageStub struct{ usage agent.AccumulatedUsage }

func (u *usageStub) Usage() agent.AccumulatedUsage { return u.usage }

func TestLastAssistantAtForSession(t *testing.T) {
	assistantAt := time.Now().Add(-2 * time.Minute).Round(0)
	legacyUpdatedAt := assistantAt.Add(time.Minute)
	tests := []struct {
		name string
		sess *session.Session
		want time.Time
	}{
		{name: "nil session"},
		{
			name: "current user message does not hide prior assistant timestamp",
			sess: &session.Session{
				UpdatedAt: legacyUpdatedAt,
				Messages: []client.Message{
					{Role: "user"}, {Role: "assistant"}, {Role: "user"},
				},
				MessageMeta: []session.MessageMeta{
					{}, {Timestamp: session.TimePtr(assistantAt)}, {Timestamp: session.TimePtr(time.Now())},
				},
			},
			want: assistantAt,
		},
		{
			name: "legacy assistant falls back to pre-turn session update",
			sess: &session.Session{
				UpdatedAt: legacyUpdatedAt,
				Messages:  []client.Message{{Role: "assistant"}},
			},
			want: legacyUpdatedAt,
		},
		{
			name: "session without assistant stays cold",
			sess: &session.Session{
				UpdatedAt: legacyUpdatedAt,
				Messages:  []client.Message{{Role: "user"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := lastAssistantAtForSession(tt.sess); !got.Equal(tt.want) {
				t.Fatalf("last assistant time = %v, want %v", got, tt.want)
			}
		})
	}
}

// Here we exercise applyRunMessagesToSession directly with a hand-built
// session and fake loop-messages via agent.SetRunMessagesForTest. The
// function is the idempotency linchpin, so it deserves direct coverage.
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
	applyHardErrorTurnMessages(sess, loop, base, "Sorry, something failed.", time.Now())
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

func TestApplyTurnState_CopiesToolResultReplacements(t *testing.T) {
	sess := &session.Session{}
	base := captureTurnBaseline(sess, "web", false)
	loop := agent.NewAgentLoop(nil, agent.NewToolRegistry(), "m", "", 1, 1, 1, nil, nil, nil)
	loop.SetToolResultBudgetState(
		map[string]string{"toolu_saved": "[Tool result omitted from context: saved]"},
		map[string]bool{"toolu_seen": true},
	)

	applyTurnState(sess, loop, nil, base)

	if sess.ToolResultReplacements["toolu_saved"] != "[Tool result omitted from context: saved]" {
		t.Fatalf("replacement state was not copied into session: %#v", sess.ToolResultReplacements)
	}
	if !sess.ToolResultSeen["toolu_seen"] || !sess.ToolResultSeen["toolu_saved"] {
		t.Fatalf("seen state was not copied into session: %#v", sess.ToolResultSeen)
	}
}

func TestApplyTurnState_PersistsCompactionCheckpointWithoutReplacingArchive(t *testing.T) {
	sess := &session.Session{
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("old user")},
			{Role: "assistant", Content: client.NewTextContent("old reply")},
		},
		MessageMeta: []session.MessageMeta{{Source: "web"}, {Source: "web"}},
	}
	base := captureTurnBaseline(sess, "web", false)
	loop := agent.NewAgentLoop(nil, agent.NewToolRegistry(), "m", "", 1, 1, 1, nil, nil, nil)
	agent.SetRunMessagesForTest(loop, []client.Message{
		{Role: "user", Content: client.NewTextContent("new user")},
		{Role: "assistant", Content: client.NewTextContent("new reply")},
	})
	// The fixture must look like real ShapeHistory output (primer + prefixed
	// summary) — HistoryForLoop rejects markerless checkpoints.
	agent.SetCompactionCheckpointMessagesForTest(loop, []client.Message{
		{Role: "user", Content: client.NewTextContent("primer")},
		{Role: "user", Content: client.NewTextContent("Previous context summary: stable summary")},
		{Role: "assistant", Content: client.NewTextContent("new reply")},
	})

	applyTurnState(sess, loop, nil, base)

	if len(sess.Messages) != 4 {
		t.Fatalf("archive was replaced instead of appended: %#v", sess.Messages)
	}
	wantArchive := []string{"old user", "old reply", "new user", "new reply"}
	for i := range wantArchive {
		if got := sess.Messages[i].Content.Text(); got != wantArchive[i] {
			t.Fatalf("archive[%d] = %q, want %q", i, got, wantArchive[i])
		}
	}
	cp := sess.CompactionCheckpoint
	if cp == nil || cp.ArchiveThroughIndex != len(sess.Messages) || cp.SchemaVersion != session.CompactionCheckpointSchemaVersion {
		t.Fatalf("checkpoint not bound to persisted archive: %#v", cp)
	}
	if got := sess.HistoryForLoop(); len(got) != 3 || got[0].Content.Text() != "primer" {
		t.Fatalf("live history did not use checkpoint: %#v", got)
	}

	// A later run with no compaction must leave the existing checkpoint active.
	fresh := agent.NewAgentLoop(nil, agent.NewToolRegistry(), "m", "", 1, 1, 1, nil, nil, nil)
	nextBase := captureTurnBaseline(sess, "web", false)
	agent.SetRunMessagesForTest(fresh, []client.Message{
		{Role: "user", Content: client.NewTextContent("tail user")},
		{Role: "assistant", Content: client.NewTextContent("tail reply")},
	})
	applyTurnState(sess, fresh, nil, nextBase)
	if sess.CompactionCheckpoint != cp {
		t.Fatal("non-compacting turn replaced the durable checkpoint")
	}
	got := sess.HistoryForLoop()
	if len(got) != 5 || got[3].Content.Text() != "tail user" || got[4].Content.Text() != "tail reply" {
		t.Fatalf("raw archive tail was not appended after checkpoint: %#v", got)
	}
}

func TestCompactionCheckpoint_SecondRunDoesNotResummarizeArchive(t *testing.T) {
	var summaryCalls atomic.Int32
	var mainMu sync.Mutex
	var mainRequests [][]client.Message
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req client.CompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		response := client.CompletionResponse{
			OutputText:   "done",
			FinishReason: "end_turn",
			Usage:        client.Usage{InputTokens: 500, OutputTokens: 10, TotalTokens: 510},
		}
		if req.ModelTier == "small" {
			summaryCalls.Add(1)
			response.OutputText = `<summary>
## Current task & next steps
Continue the deterministic checkpoint test.
## User corrections & decisions
Keep the transcript lossless and reuse one stable summary.
			</summary>`
		} else {
			mainMu.Lock()
			mainRequests = append(mainRequests, req.Messages)
			mainMu.Unlock()
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	sess := &session.Session{}
	primer := strings.Repeat("stable history payload ", 350)
	for i := 0; i < 15; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		sess.Messages = append(sess.Messages, client.Message{
			Role: role, Content: client.NewTextContent(primer),
		})
		sess.MessageMeta = append(sess.MessageMeta, session.MessageMeta{Source: "web"})
	}

	newLoop := func() *agent.AgentLoop {
		loop := agent.NewAgentLoop(client.NewGatewayClient(server.URL, ""), agent.NewToolRegistry(), "medium", "", 3, 2000, 200, nil, nil, nil)
		loop.SetContextWindowExplicit(30000)
		loop.SetSkillDiscovery(false)
		return loop
	}

	first := newLoop()
	firstBase := captureTurnBaseline(sess, "web", false)
	if _, _, err := first.Run(context.Background(), "first prompt", nil, sess.HistoryForLoop()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	applyTurnState(sess, first, nil, firstBase)
	if sess.CompactionCheckpoint == nil {
		t.Fatal("first run crossed preflight threshold but persisted no checkpoint")
	}
	firstSummaryCalls := summaryCalls.Load()
	if firstSummaryCalls == 0 {
		t.Fatal("test did not trigger a summary on the first run")
	}
	checkpointJSON, err := json.Marshal(sess.CompactionCheckpoint)
	if err != nil {
		t.Fatalf("marshal first checkpoint: %v", err)
	}
	archiveAfterFirst := len(sess.Messages)

	second := newLoop()
	secondBase := captureTurnBaseline(sess, "web", false)
	if _, _, err := second.Run(context.Background(), "short follow-up", nil, sess.HistoryForLoop()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	applyTurnState(sess, second, nil, secondBase)

	if got := summaryCalls.Load(); got != firstSummaryCalls {
		t.Fatalf("second run re-summarized the lossless archive: summary calls %d -> %d", firstSummaryCalls, got)
	}
	mainMu.Lock()
	if len(mainRequests) == 0 {
		mainMu.Unlock()
		t.Fatal("second run sent no main request")
	}
	lastMain := append([]client.Message(nil), mainRequests[len(mainRequests)-1]...)
	mainMu.Unlock()
	if len(lastMain) < 4 {
		t.Fatalf("second-run request too short: %#v", lastMain)
	}
	if got := lastMain[1].Content.Text(); got != primer {
		t.Fatalf("second run lost the original first user: got %q, want %q", got, primer)
	}
	if got := lastMain[2].Content.Text(); !strings.HasPrefix(got, ctxwin.CompactionSummaryPrefix) {
		t.Fatalf("second run lost the compacted summary: %q", got)
	}
	checkpointAfter, err := json.Marshal(sess.CompactionCheckpoint)
	if err != nil {
		t.Fatalf("marshal checkpoint after second run: %v", err)
	}
	if string(checkpointAfter) != string(checkpointJSON) {
		t.Fatalf("non-compacting second run rewrote stable checkpoint\nbefore=%s\nafter=%s", checkpointJSON, checkpointAfter)
	}
	if len(sess.Messages) <= archiveAfterFirst {
		t.Fatalf("second run did not append to archive: before=%d after=%d", archiveAfterFirst, len(sess.Messages))
	}
	if live := sess.HistoryForLoop(); len(live) >= len(sess.Messages) {
		t.Fatalf("live context did not remain compacted: live=%d archive=%d", len(live), len(sess.Messages))
	}
}

// applyTurnState must persist the estimator calibration alongside the
// tool-result budget state, and clear a stale persisted sample when the loop
// holds none (e.g. the restore-time validation rejected it).
func TestApplyTurnState_CopiesCalibration(t *testing.T) {
	sess := &session.Session{}
	base := captureTurnBaseline(sess, "web", false)
	loop := agent.NewAgentLoop(nil, agent.NewToolRegistry(), "m", "", 1, 1, 1, nil, nil, nil)
	// Seed via the public restore path: snapshot the fingerprint of the
	// loop's own registry so validation accepts the sample.
	fp := loop.ToolsFingerprint()
	loop.SetEstOverheadState(4321, "test-model", fp)

	applyTurnState(sess, loop, nil, base)

	cal := sess.CompactionCalibration
	if cal == nil || cal.OverheadTokens != 4321 || cal.Model != "test-model" || cal.ToolsFingerprint != fp {
		t.Fatalf("calibration was not copied into session: %#v", cal)
	}

	// A loop with no sample clears the persisted state.
	fresh := agent.NewAgentLoop(nil, agent.NewToolRegistry(), "m", "", 1, 1, 1, nil, nil, nil)
	applyTurnState(sess, fresh, nil, base)
	if sess.CompactionCalibration != nil {
		t.Fatalf("stale calibration must be cleared when the loop holds no sample: %#v", sess.CompactionCalibration)
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

// Enters through applyTurnState — the helper that backs the mid-turn
// checkpoint — rather than calling stripSpokenSummaryForKoeTurn directly. The
// unit is already covered; what this pins is the wiring: that the save path
// reads the Koe source off turnBaseline and cleans BOTH durable copies. A
// process that dies right after this checkpoint leaves whatever it wrote, and
// the checkpoint is what the next run reloads as live context.
func TestApplyTurnState_KoeCheckpointIsTagStripped(t *testing.T) {
	const tag = "<spoken_summary>said aloud.</spoken_summary>"
	sess := &session.Session{
		Messages:    []client.Message{{Role: "user", Content: client.NewTextContent("older")}},
		MessageMeta: []session.MessageMeta{{Source: "koe"}},
	}
	base := captureTurnBaseline(sess, "koe", false)
	loop := agent.NewAgentLoop(nil, agent.NewToolRegistry(), "m", "", 1, 1, 1, nil, nil, nil)
	agent.SetRunMessagesForTest(loop, []client.Message{
		{Role: "user", Content: client.NewTextContent("ask")},
		{Role: "assistant", Content: client.NewTextContent("answer\n" + tag)},
	})
	agent.SetCompactionCheckpointMessagesForTest(loop, []client.Message{
		{Role: "user", Content: client.NewTextContent("Previous context summary: …")},
		{Role: "assistant", Content: client.NewTextContent("answer\n" + tag)},
	})

	applyTurnState(sess, loop, nil, base)

	for i, m := range sess.Messages {
		if strings.Contains(m.Content.Text(), "spoken_summary") {
			t.Errorf("archive[%d] kept the raw tag: %q", i, m.Content.Text())
		}
	}
	cp := sess.CompactionCheckpoint
	if cp == nil {
		t.Fatal("no checkpoint persisted")
	}
	for i, m := range cp.Messages {
		if strings.Contains(m.Content.Text(), "spoken_summary") {
			t.Errorf("checkpoint[%d] kept the raw tag — it would reach the model next turn: %q", i, m.Content.Text())
		}
	}
	if got := cp.Messages[1].Content.Text(); !strings.Contains(got, "answer") {
		t.Errorf("checkpoint lost the reply body: %q", got)
	}

	// A non-Koe source must not be touched by this path.
	web := &session.Session{Messages: []client.Message{{Role: "user", Content: client.NewTextContent("older")}}}
	webBase := captureTurnBaseline(web, "web", false)
	webLoop := agent.NewAgentLoop(nil, agent.NewToolRegistry(), "m", "", 1, 1, 1, nil, nil, nil)
	agent.SetRunMessagesForTest(webLoop, []client.Message{
		{Role: "assistant", Content: client.NewTextContent("literal " + tag + " in a web transcript")},
	})
	applyTurnState(web, webLoop, nil, webBase)
	if !strings.Contains(web.Messages[1].Content.Text(), "spoken_summary") {
		t.Error("non-Koe transcript was rewritten by the Koe strip")
	}
}

// The hard-error save differs from applyTurnState: it binds the checkpoint
// before appending a synthetic friendly-error message. Exercise that exact
// ordering and the Koe cleanup through the same helper the runner calls.
func TestApplyHardErrorTurnMessages_KoeCheckpointIsTagStripped(t *testing.T) {
	const tag = "<spoken_summary>hard-error reply was spoken.</spoken_summary>"
	sess := &session.Session{
		Messages:    []client.Message{{Role: "user", Content: client.NewTextContent("older")}},
		MessageMeta: []session.MessageMeta{{Source: "koe"}},
	}
	base := captureTurnBaseline(sess, "koe", false)
	loop := agent.NewAgentLoop(nil, agent.NewToolRegistry(), "m", "", 1, 1, 1, nil, nil, nil)
	agent.SetRunMessagesForTest(loop, []client.Message{
		{Role: "user", Content: client.NewTextContent("ask")},
		{Role: "assistant", Content: client.NewTextContent("detail\n" + tag)},
	})
	agent.SetCompactionCheckpointMessagesForTest(loop, []client.Message{
		{Role: "user", Content: client.NewTextContent(ctxwin.CompactionSummaryPrefix + "saved state")},
		{Role: "assistant", Content: client.NewTextContent("detail\n" + tag)},
	})

	applyHardErrorTurnMessages(sess, loop, base, "Friendly failure.", time.Unix(1, 0))

	cp := sess.CompactionCheckpoint
	if cp == nil {
		t.Fatal("hard-error path persisted no checkpoint")
	}
	if cp.ArchiveThroughIndex != len(sess.Messages)-1 {
		t.Fatalf("checkpoint index = %d, want %d before friendly error", cp.ArchiveThroughIndex, len(sess.Messages)-1)
	}
	if got := sess.Messages[len(sess.Messages)-1].Content.Text(); got != "Friendly failure." {
		t.Fatalf("last archive message = %q, want friendly error", got)
	}
	for i, msg := range sess.Messages {
		if strings.Contains(msg.Content.Text(), "spoken_summary") {
			t.Errorf("archive[%d] kept raw Koe markup: %q", i, msg.Content.Text())
		}
	}
	for i, msg := range cp.Messages {
		if strings.Contains(msg.Content.Text(), "spoken_summary") {
			t.Errorf("checkpoint[%d] kept raw Koe markup: %q", i, msg.Content.Text())
		}
	}
	if !strings.Contains(cp.Messages[1].Content.Text(), "detail") {
		t.Fatalf("checkpoint lost reply body: %q", cp.Messages[1].Content.Text())
	}
}

func TestApplyHardErrorTurnMessages_UsesNormalizedBaselineSource(t *testing.T) {
	sess := &session.Session{
		Messages:    []client.Message{{Role: "user", Content: client.NewTextContent("older")}},
		MessageMeta: []session.MessageMeta{{Source: "unknown"}},
	}
	base := captureTurnBaseline(sess, "unknown", false)
	loop := agent.NewAgentLoop(nil, agent.NewToolRegistry(), "m", "", 1, 1, 1, nil, nil, nil)
	agent.SetRunMessagesForTest(loop, []client.Message{
		{Role: "user", Content: client.NewTextContent("ask")},
	})

	applyHardErrorTurnMessages(sess, loop, base, "Friendly failure.", time.Unix(1, 0))

	if got := sess.MessageMeta[len(sess.MessageMeta)-1].Source; got != "unknown" {
		t.Fatalf("hard-error source = %q, want normalized baseline source %q", got, "unknown")
	}
}
