package tui

import (
	"context"

	"encoding/json"
	"errors"
	tea "github.com/charmbracelet/bubbletea"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

func TestCompact_TooShort(t *testing.T) {
	m := newCommandTestModel(t)
	// Session has 0 messages — too short
	result := m.runCompact(context.Background(), "", 0)()
	if result.err == nil {
		t.Error("expected error for too-short conversation")
	}
	if result.err != nil && !strings.Contains(result.err.Error(), "too short") {
		t.Errorf("expected 'too short' error, got: %v", result.err)
	}
}

func TestFormatCompactResult(t *testing.T) {
	msg := compactDoneMsg{
		beforeTokens: 50000,
		afterTokens:  8000,
		summary:      "User worked on TUI improvements",
	}
	result := formatCompactResult(msg)
	if !strings.Contains(result, "50,000") || !strings.Contains(result, "8,000") {
		t.Errorf("expected formatted token counts in result: %s", result)
	}
	if !strings.Contains(result, "TUI improvements") {
		t.Errorf("expected summary text in result: %s", result)
	}
}

func TestCompact_PreservesArchiveAndMetadataAndPersistsLiveCheckpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req client.CompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		output := `<summary>
## Current task & next steps
Continue the checkpoint migration.
## User corrections & decisions
Keep the transcript lossless.
</summary>`
		if len(req.Messages) > 0 && strings.Contains(req.Messages[0].Content.Text(), "extracting durable knowledge") {
			output = "NONE"
		}
		_ = json.NewEncoder(w).Encode(client.CompletionResponse{
			OutputText: output,
			Usage:      client.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
		})
	}))
	defer server.Close()

	m := newCommandTestModel(t)
	m.gateway = client.NewGatewayClient(server.URL, "")
	m.cfg.Agent.ContextWindow = 128000
	sess := m.sessions.Current()
	now := time.Now()
	for i := 0; i < 12; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		sess.Messages = append(sess.Messages, client.Message{Role: role, Content: client.NewTextContent(strings.Repeat("history ", 100))})
		sess.MessageMeta = append(sess.MessageMeta, session.MessageMeta{
			Source:    "local",
			MessageID: string(rune('a' + i)),
			Timestamp: session.TimePtr(now.Add(time.Duration(i) * time.Second)),
		})
	}
	originalMessages := append([]client.Message(nil), sess.Messages...)
	originalMeta := append([]session.MessageMeta(nil), sess.MessageMeta...)

	result := m.runCompact(context.Background(), "", 0)()
	if result.err != nil {
		t.Fatalf("runCompact: %v", result.err)
	}
	// Mutation happens in the Update handler (deferred apply after the
	// generation check), not in the worker.
	if sess.CompactionCheckpoint != nil {
		t.Fatal("worker must not mutate the session; apply belongs to the handler")
	}
	if updated, _ := m.Update(result); updated == nil {
		t.Fatal("compactDoneMsg handler returned no model")
	}
	if !reflect.DeepEqual(sess.Messages, originalMessages) {
		t.Fatal("manual compaction mutated the lossless transcript")
	}
	if !reflect.DeepEqual(sess.MessageMeta, originalMeta) {
		t.Fatal("manual compaction mutated message metadata")
	}
	cp := sess.CompactionCheckpoint
	if cp == nil || cp.SchemaVersion != session.CompactionCheckpointSchemaVersion || cp.ArchiveThroughIndex != len(originalMessages) {
		t.Fatalf("checkpoint was not persisted against archive: %#v", cp)
	}
	if len(cp.Messages) >= len(originalMessages) {
		t.Fatalf("checkpoint did not compact live context: %d >= %d", len(cp.Messages), len(originalMessages))
	}
	if got := sess.HistoryForLoop(); !reflect.DeepEqual(got, cp.Messages) {
		t.Fatalf("live history does not resolve to checkpoint: got=%#v cp=%#v", got, cp.Messages)
	}

	loaded, err := m.sessions.Load(sess.ID)
	if err != nil {
		t.Fatalf("reload compacted session: %v", err)
	}
	if loaded.CompactionCheckpoint == nil || len(loaded.Messages) != len(originalMessages) {
		t.Fatalf("disk round-trip lost checkpoint or archive: cp=%#v archive=%d", loaded.CompactionCheckpoint, len(loaded.Messages))
	}
}

// A failed Save() must leave the in-memory session on its previous checkpoint.
// Otherwise the user is told compaction failed while the next turn already runs
// on the new compacted view — and the next successful Save() persists it anyway.
func TestCompact_FailedSaveRollsBackCheckpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(client.CompletionResponse{
			OutputText: "<summary>\n## Current task & next steps\nkeep going.\n</summary>",
			Usage:      client.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
		})
	}))
	defer server.Close()

	sessDir := t.TempDir()
	m := newCommandTestModelInDir(t, sessDir)
	m.gateway = client.NewGatewayClient(server.URL, "")
	m.cfg.Agent.ContextWindow = 128000

	sess := m.sessions.Current()
	for i := 0; i < 12; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		sess.Messages = append(sess.Messages, client.Message{Role: role, Content: client.NewTextContent(strings.Repeat("history ", 100))})
		sess.MessageMeta = append(sess.MessageMeta, session.MessageMeta{Source: "local"})
	}
	prior := &session.CompactionCheckpoint{
		SchemaVersion:       session.CompactionCheckpointSchemaVersion,
		ArchiveThroughIndex: 2,
		Messages:            []client.Message{{Role: "user", Content: client.NewTextContent("earlier checkpoint")}},
	}
	sess.CompactionCheckpoint = prior

	result := m.runCompact(context.Background(), "", 0)()
	if result.err != nil {
		t.Fatalf("worker phase must succeed: %v", result.err)
	}

	if os.Geteuid() == 0 {
		t.Skip("chmod-based write denial is a no-op under root")
	}
	// Drop write permission so the atomic session write in the apply phase fails.
	if err := os.Chmod(sessDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(sessDir, 0o755) })

	if err := m.applyCompactResult(result); err == nil {
		t.Fatal("expected the failed save to surface as an error")
	}
	if sess.CompactionCheckpoint != prior {
		t.Fatalf("checkpoint was not rolled back after a failed save: %#v", sess.CompactionCheckpoint)
	}
}

// The live loop's window (auto-adjusted from response.model) and estimator
// calibration must drive /compact — a static config fallback of 128K on a
// 1M-window session over-compacts by ~8x, and overhead=0 disables the same
// calibration every daemon-path compaction decision uses.
func TestCompactWindowAndOverhead_PrefersLoopState(t *testing.T) {
	m := newCommandTestModel(t)

	m.cfg.Agent.ContextWindow = 0
	win, overhead := m.compactWindowAndOverhead()
	if win != 128000 || overhead != 0 {
		t.Fatalf("no loop, no cfg: got win=%d overhead=%d, want 128000/0", win, overhead)
	}

	m.cfg.Agent.ContextWindow = 200000
	win, _ = m.compactWindowAndOverhead()
	if win != 200000 {
		t.Fatalf("cfg fallback: got win=%d, want 200000", win)
	}

	loop := agent.NewAgentLoop(nil, agent.NewToolRegistry(), "medium", "", 1, 0, 0, nil, nil, nil)
	loop.SetContextWindow(50000)
	loop.SetEstOverheadState(1234, "test-model", loop.ToolsFingerprint())
	m.agentLoop = loop
	win, overhead = m.compactWindowAndOverhead()
	if win != 50000 {
		t.Fatalf("live loop window must win over cfg: got %d, want 50000", win)
	}
	if overhead != 1234 {
		t.Fatalf("loop calibration must flow into /compact: got %d, want 1234", overhead)
	}

	// The shaped history's system slot is a placeholder, so the real system
	// prompt's estimate must ride on top of the calibration or shaping and
	// restoration budgets treat the whole prompt as free headroom.
	agent.SetLastSystemPromptEstimateForTest(loop, 500)
	_, overhead = m.compactWindowAndOverhead()
	if overhead != 1234+500 {
		t.Fatalf("system prompt estimate must add to overhead: got %d, want 1734", overhead)
	}
}

// Manual /compact restores recently read files into the checkpoint like the
// daemon's proactive/preflight paths do — otherwise exact file content the
// summary paraphrases is lost precisely when the user asked to compact.
func TestCompact_AppendsFileRestoreBlock(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(client.CompletionResponse{
			OutputText: "<summary>\n## Current task & next steps\nkeep going.\n</summary>",
			Usage:      client.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
		})
	}))
	defer server.Close()

	m := newCommandTestModel(t)
	m.gateway = client.NewGatewayClient(server.URL, "")
	m.cfg.Agent.ContextWindow = 128000
	m.fileRestoreBuilder = func(shaped []client.Message, overhead int) (client.Message, bool) {
		return client.Message{Role: "user", Content: client.NewTextContent("<system-reminder>restored-content</system-reminder>")}, true
	}
	sess := m.sessions.Current()
	for i := 0; i < 12; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		sess.Messages = append(sess.Messages, client.Message{Role: role, Content: client.NewTextContent(strings.Repeat("history ", 100))})
		sess.MessageMeta = append(sess.MessageMeta, session.MessageMeta{Source: "local"})
	}

	result := m.runCompact(context.Background(), "", 0)()
	if result.err != nil {
		t.Fatalf("runCompact: %v", result.err)
	}
	if err := m.applyCompactResult(result); err != nil {
		t.Fatalf("apply: %v", err)
	}
	cp := sess.CompactionCheckpoint
	if cp == nil || len(cp.Messages) == 0 {
		t.Fatalf("no checkpoint persisted: %#v", cp)
	}
	last := cp.Messages[len(cp.Messages)-1]
	if !strings.Contains(last.Content.Text(), "restored-content") {
		t.Fatalf("restore block missing from checkpoint tail: %q", last.Content.Text())
	}
}

// A shaped tail that already ends with a user message must NOT get the
// user-role restore block appended: SanitizeCompactedHistory's keep-later
// merge would delete the earlier user message (e.g. a persisted tool_result)
// on the next load, and orphan-stripping would then remove its paired
// tool_use — content loss in the exact flow that asked to preserve context.
func TestCompact_SkipsRestoreWhenTailEndsWithUser(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(client.CompletionResponse{
			OutputText: "<summary>\n## Current task & next steps\nkeep going.\n</summary>",
			Usage:      client.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
		})
	}))
	defer server.Close()

	m := newCommandTestModel(t)
	m.gateway = client.NewGatewayClient(server.URL, "")
	m.cfg.Agent.ContextWindow = 128000
	m.fileRestoreBuilder = func(shaped []client.Message, overhead int) (client.Message, bool) {
		return client.Message{Role: "user", Content: client.NewTextContent("restored-content")}, true
	}
	sess := m.sessions.Current()
	for i := 0; i < 13; i++ { // odd count: history ends on a USER message
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		sess.Messages = append(sess.Messages, client.Message{Role: role, Content: client.NewTextContent(strings.Repeat("history ", 100))})
		sess.MessageMeta = append(sess.MessageMeta, session.MessageMeta{Source: "local"})
	}

	result := m.runCompact(context.Background(), "", 0)()
	if result.err != nil {
		t.Fatalf("runCompact: %v", result.err)
	}
	if len(result.shaped) == 0 {
		t.Fatal("shaped payload is empty")
	}
	last := result.shaped[len(result.shaped)-1]
	// Precondition: the fixture must actually produce a user-ending tail, or
	// this test passes vacuously and no longer covers the guard.
	if last.Role != "user" {
		t.Fatalf("fixture no longer yields a user-ending tail (got role %q); rework the fixture", last.Role)
	}
	if strings.Contains(last.Content.Text(), "restored-content") {
		t.Fatalf("restore block must be skipped on a user-ending tail: %q", last.Content.Text())
	}
}

// An applied mid-run compaction must survive a crash before run end: the
// summary was paid for, and losing it forces the next run to re-summarize
// (the exact cross-run waste the durable checkpoint exists to prevent).
func TestPersistMidTurnCompactionCheckpoint(t *testing.T) {
	sessDir := t.TempDir()
	m := newCommandTestModelInDir(t, sessDir)
	sess := m.sessions.Current()
	sess.Messages = []client.Message{
		{Role: "user", Content: client.NewTextContent("prompt")},
	}

	// Empty checkpoint = nothing applied yet: must be a no-op.
	if err := m.persistMidTurnCompactionCheckpoint(sess, nil); err != nil {
		t.Fatalf("empty checkpoint: %v", err)
	}
	if sess.CompactionCheckpoint != nil {
		t.Fatal("empty checkpoint must not create a checkpoint")
	}

	cp := []client.Message{
		{Role: "user", Content: client.NewTextContent("primer")},
		{Role: "user", Content: client.NewTextContent("Previous context summary: mid-turn")},
	}
	if err := m.persistMidTurnCompactionCheckpoint(sess, cp); err != nil {
		t.Fatalf("persist: %v", err)
	}
	got := sess.CompactionCheckpoint
	if got == nil || got.ArchiveThroughIndex != len(sess.Messages) || len(got.Messages) != 2 {
		t.Fatalf("checkpoint not applied: %#v", got)
	}
	loaded, err := m.sessions.Load(sess.ID)
	if err != nil || loaded.CompactionCheckpoint == nil {
		t.Fatalf("mid-turn checkpoint not durable: err=%v cp=%#v", err, loaded)
	}

	// A failed save must roll back so the in-memory view matches disk.
	prior := sess.CompactionCheckpoint
	if err := os.Chmod(sessDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(sessDir, 0o755) })
	if err := m.persistMidTurnCompactionCheckpoint(sess, cp[:1]); err == nil {
		t.Fatal("expected failed save to surface")
	}
	if sess.CompactionCheckpoint != prior {
		t.Fatalf("failed save must roll back the checkpoint: %#v", sess.CompactionCheckpoint)
	}
}

// The timeout is operator-configurable for slow gateways; unset falls back
// to the 5-minute default that covers a full sequential fold.
func TestCompactTimeout_ConfigOverride(t *testing.T) {
	m := newCommandTestModel(t)
	if got := m.compactTimeout(); got != defaultCompactTimeout {
		t.Fatalf("unset config: got %v, want %v", got, defaultCompactTimeout)
	}
	m.cfg.Agent.CompactTimeoutSecs = 7
	if got := m.compactTimeout(); got != 7*time.Second {
		t.Fatalf("configured: got %v, want 7s", got)
	}
}

// Cancellation must abort the pass without producing a checkpoint payload.
// Covers both check sites: a pre-cancelled ctx bails before the first paid
// call, and a cancel arriving from inside the gateway handler (mid-flight)
// surfaces through the LLM error path.
func TestCompact_CancelledContextDoesNotPersistCheckpoint(t *testing.T) {
	m := newCommandTestModel(t)
	sess := m.sessions.Current()
	for i := 0; i < 12; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		sess.Messages = append(sess.Messages, client.Message{Role: role, Content: client.NewTextContent(strings.Repeat("history ", 100))})
		sess.MessageMeta = append(sess.MessageMeta, session.MessageMeta{Source: "local"})
	}

	// Site 1: already-cancelled ctx bails before any paid call.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := m.runCompact(ctx, "", 3)()
	if result.err == nil {
		t.Fatal("cancelled pass must surface an error")
	}
	if result.gen != 3 {
		t.Fatalf("result must carry its generation: got %d", result.gen)
	}
	if sess.CompactionCheckpoint != nil {
		t.Fatalf("cancelled pass must not persist a checkpoint: %#v", sess.CompactionCheckpoint)
	}

	// Site 2: cancel lands AFTER a successful summary, exactly at the
	// post-LLM guard before shaping/restore — the deterministic seam for the
	// check the first site never reaches.
	ctx2, cancel2 := context.WithCancel(context.Background())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(client.CompletionResponse{
			OutputText: "<summary>\n## Current task & next steps\nkeep going.\n</summary>",
			Usage:      client.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
		})
	}))
	defer server.Close()
	m.gateway = client.NewGatewayClient(server.URL, "")
	m.compactTestHookAfterSummary = cancel2
	result = m.runCompact(ctx2, "", 4)()
	// errors.Is pins the exit to the post-LLM guard (which returns ctx.Err()
	// unwrapped) — the "nothing to compact" bail would fail this assertion.
	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("expected context.Canceled from the post-summary guard, got %v", result.err)
	}
	if len(result.shaped) != 0 {
		t.Fatal("post-summary cancellation must not produce a checkpoint payload")
	}
	if sess.CompactionCheckpoint != nil {
		t.Fatalf("cancellation must not persist a checkpoint: %#v", sess.CompactionCheckpoint)
	}
}

// /compact and agent runs are mutually exclusive in both directions: the
// worker reads unsynchronized live-loop state, and an abandoned pass would
// otherwise bill for up to the full compact timeout with no cancel owner.
func TestCompactRunMutualExclusion(t *testing.T) {
	m := newCommandTestModel(t)
	// Seed past MinShapeable so /compact reaches the busy check instead of
	// bailing on length — without this, direction 1 passes vacuously.
	sess := m.sessions.Current()
	for i := 0; i < 12; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		sess.Messages = append(sess.Messages, client.Message{Role: role, Content: client.NewTextContent(strings.Repeat("history ", 100))})
		sess.MessageMeta = append(sess.MessageMeta, session.MessageMeta{Source: "local"})
	}
	renderedOutput := func() string {
		var b strings.Builder
		for _, o := range m.output {
			b.WriteString(o.rendered)
		}
		return b.String()
	}

	// Direction 1a: /compact refused while a run is processing.
	m.state = stateProcessing
	m.handleSlashCommand("/compact")
	if m.compactInFlight {
		t.Fatal("/compact must be refused while a run is active")
	}
	if !strings.Contains(renderedOutput(), "Busy") {
		t.Fatal("refusal must render the Busy line, not the too-short line")
	}

	// Direction 1c: after Esc during a run — state is back to input but the
	// run GOROUTINE is still unwinding (appending, checkpointing, saving) —
	// /compact must still be refused via runInFlight.
	m.state = stateInput
	m.runInFlight = 1
	m.handleSlashCommand("/compact")
	if m.cancelRun != nil || m.compactInFlight {
		t.Fatal("/compact must not start under a winding-down run")
	}
	if !strings.Contains(renderedOutput(), "Busy") {
		t.Fatal("the run-flag refusal must use the Busy line, not the compacting line")
	}
	m.runInFlight = 0

	// Direction 1d: remote tasks (/research) are the same door — refused, and
	// the compact result cannot resolve under them (gen bumped at their
	// start).
	m.compactInFlight = true
	genBefore := m.compactGen
	m.handleSlashCommand("/research anything")
	if m.runInFlight != 0 {
		t.Fatal("/research must be refused under an in-flight compact")
	}
	m.compactInFlight = false
	_ = genBefore

	// Direction 1b: /compact also refused while a (possibly Esc-cancelled but
	// still alive) compact worker holds the flag.
	m.state = stateInput
	m.compactInFlight = true
	m.handleSlashCommand("/compact")
	if m.cancelRun != nil {
		t.Fatal("a second compact must not start while a worker is alive")
	}
	if !strings.Contains(renderedOutput(), "try again in a moment") {
		t.Fatal("the flag-case refusal must use the compacting message, not the run message")
	}

	// Direction 2: a new local run refused while a compact is in flight —
	// including after Esc returned the UI to input (the worker is still
	// alive; only its compactDoneMsg releases the flag). The early refusal
	// fires before the echo, so the transcript gains nothing and the raw
	// composer text (placeholder form) survives.
	m.state = stateInput
	msgsBefore := len(sess.Messages)
	outputBefore := len(m.output)
	m.textarea.SetValue("hello")
	updated, _ := m.handleSubmit()
	m = updated.(*Model)
	if m.cancelRun != nil {
		t.Fatal("a run must not start under an in-flight compact")
	}
	if len(sess.Messages) != msgsBefore {
		t.Fatalf("refused submit must not touch the session: messages grew %d -> %d", msgsBefore, len(sess.Messages))
	}
	if got := m.textarea.Value(); got != "hello" {
		t.Fatalf("refused submit must restore the composer input, got %q", got)
	}
	echoed := false
	for _, o := range m.output[outputBefore:] {
		if strings.Contains(o.rendered, "hello") {
			echoed = true
		}
	}
	if echoed {
		t.Fatal("refused submit must not echo the turn into the transcript")
	}
	if !strings.Contains(renderedOutput(), "try again in a moment") {
		t.Fatal("refusal must tell the user why")
	}

	// Direction 2b: custom slash commands are a second door into runAgentLoop
	// and must be refused the same way, restoring the full command text.
	m.customCommands["mytask"] = "do the thing"
	m.textarea.Reset()
	m.handleSlashCommand("/mytask some args")
	if m.cancelRun != nil {
		t.Fatal("a custom-command run must not start under an in-flight compact")
	}
	if got := m.textarea.Value(); got != "/mytask some args" {
		t.Fatalf("custom-command refusal must restore the command text, got %q", got)
	}

	// Release: the compactDoneMsg handler is the single release point. (A
	// matching gen exercises the failure-print branch; the Esc-drop branch is
	// covered by TestEscDuringCompact_InvalidatesGeneration.)
	updated, _ = m.Update(compactDoneMsg{gen: m.compactGen, err: context.Canceled})
	m = updated.(*Model)
	if m.compactInFlight {
		t.Fatal("compactDoneMsg must release the flag")
	}
}

// Busy must win over the length message: with a too-short session and an
// active run, /compact reports Busy (and must not read HistoryForLoop while
// the run goroutine may be appending).
func TestCompactBusyCheckPrecedesLengthCheck(t *testing.T) {
	m := newCommandTestModel(t) // 0-message session, well under MinShapeable
	m.state = stateProcessing
	m.handleSlashCommand("/compact")
	var out strings.Builder
	for _, o := range m.output {
		out.WriteString(o.rendered)
	}
	if !strings.Contains(out.String(), "Busy") || strings.Contains(out.String(), "too short") {
		t.Fatalf("busy must win over too-short: %q", out.String())
	}
}

// Esc must NOT release the run-side flag — only agentDoneMsg does. Releasing
// on Esc would re-open the mirror race (/compact starting under a
// winding-down run's session writes).
func TestEscDoesNotReleaseRunInFlight(t *testing.T) {
	m := newCommandTestModel(t)
	m.state = stateProcessing
	m.runInFlight = 1
	m.cancelRun = func() {}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	m = updated.(*Model)
	if m.runInFlight != 1 {
		t.Fatal("Esc must not release runInFlight; only agentDoneMsg does")
	}

	// End-to-end U1 flow: right after the Esc, /compact must be refused by
	// the still-held run guard.
	m.handleSlashCommand("/compact")
	if m.compactInFlight {
		t.Fatal("/compact must be refused right after Esc while the run unwinds")
	}

	// Overlap safety: a second run starts (Esc-then-Enter); the FIRST run's
	// done message must not unblock /compact under the second.
	m.runInFlight++                       // run B starts
	updated, _ = m.Update(agentDoneMsg{}) // run A finishes
	m = updated.(*Model)
	if m.runInFlight != 1 {
		t.Fatalf("first done must not release the guard under a live second run: %d", m.runInFlight)
	}
	updated, _ = m.Update(agentDoneMsg{}) // run B finishes
	m = updated.(*Model)
	if m.runInFlight != 0 {
		t.Fatalf("all runs done must fully release: %d", m.runInFlight)
	}
}
