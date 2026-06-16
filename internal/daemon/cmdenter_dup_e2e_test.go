package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// blockingE2ETool holds its Run open until released, giving the test a
// deterministic mid-tool window — the exact state the user is in when they
// press Cmd+Enter while a long bash command runs.
type blockingE2ETool struct {
	startedOnce sync.Once
	started     chan struct{}
	release     chan struct{}
}

func newBlockingE2ETool() *blockingE2ETool {
	return &blockingE2ETool{started: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingE2ETool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        "slow_dance",
		Description: "long-running tool for the cmd+enter e2e",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

func (b *blockingE2ETool) Run(ctx context.Context, args string) (agent.ToolResult, error) {
	b.startedOnce.Do(func() { close(b.started) })
	// Deliberately ignore ctx: in the production incident the bash tool ran
	// to completion (8s) while the cancel was already pending — the loop
	// appends its result and only THEN observes ctx.Err() at the top of the
	// next iteration, which is the Bug-A flush window.
	<-b.release
	return agent.ToolResult{Content: "dance loop running"}, nil
}

func (b *blockingE2ETool) RequiresApproval() bool     { return false }
func (b *blockingE2ETool) IsReadOnlyCall(string) bool { return true }

// contentRoutedGateway scripts completion responses by inspecting the
// request's user text instead of by call order, so unrelated concurrent LLM
// calls (smart-title upgrades) cannot steal a scripted slot.
func contentRoutedGateway(t *testing.T, preamble, primaryText, followUpText string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req client.CompletionRequest
		_ = json.Unmarshal(body, &req)

		resp := client.CompletionResponse{Model: "test-model", FinishReason: "end_turn", OutputText: "ok"}
		// The primary prompt gets the preamble + blocking tool call; anything
		// carrying the follow-up text gets a plain final answer.
		var lastUser string
		for _, m := range req.Messages {
			if m.Role == "user" {
				lastUser = m.Content.Text()
			}
		}
		switch {
		case strings.Contains(lastUser, followUpText):
			resp.OutputText = "adjusted the dance"
		case strings.Contains(lastUser, primaryText):
			resp.OutputText = preamble
			resp.FinishReason = "tool_use"
			resp.ToolCalls = []client.FunctionCall{{
				ID:        "toolu_e2e_1",
				Name:      "slow_dance",
				Arguments: json.RawMessage(`{}`),
			}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// countInSessions sums substring occurrences across the MESSAGE CONTENTS of
// every persisted session file under shanDir/sessions. Deliberately scoped to
// messages: other session fields (e.g. the async smart title, which the fake
// gateway also answers) may legitimately echo conversation text.
func countInSessions(t *testing.T, shanDir, needle string) int {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(shanDir, "sessions", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var sess struct {
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &sess); err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, m := range sess.Messages {
			total += strings.Count(string(m.Content), needle)
		}
	}
	return total
}

// TestE2E_CmdEnterForceSend_NoDuplicates replays the full Cmd+Enter incident
// timeline through the real runner + loop + SessionCache + on-disk session,
// asserting BOTH duplication bugs stay dead:
//
//	run 1: user prompt → LLM answers preamble + slow tool → user queues a
//	follow-up (steering inject) → Cmd+Enter: retract (status "retracted"),
//	mailbox cascade, cancel → tool finishes → loop observes ctx.Err()
//	run 2: the force-send payload (the queued text) starts a fresh run
//
// Disk truth afterwards:
//   - the preamble is persisted EXACTLY once (Bug A: the cancel teardown used
//     to re-append lastText already saved inside the text+tool_use message —
//     session 224BE076 idx 150/152)
//   - the queued text is persisted EXACTLY once (Bug B/C: an un-retracted
//     inject survivor used to re-enter via mailbox prepend as "text\ntext")
func TestE2E_CmdEnterForceSend_NoDuplicates(t *testing.T) {
	const preamble = "Process stable, checking the daemon WebSocket connection:"
	const primary = "start the dance loop"
	const queued = "make the dance smaller please"

	ts := httptest.NewServer(contentRoutedGateway(t, preamble, primary, queued))
	defer ts.Close()

	deps := runAgentContractTestDeps(t, ts.URL)
	defer deps.SessionCache.CloseAll()
	tool := newBlockingE2ETool()
	deps.Registry.Register(tool)

	// Run 0 establishes the session, mirroring the Desktop flow where the
	// interrupted message is a follow-up carrying session_id (RunAgent
	// recomputes the route key from the request, so a preset RouteKey would
	// be discarded — "session:<id>" only materializes via SessionID).
	res0, err := RunAgent(context.Background(), deps,
		RunAgentRequest{Text: "hello", Source: "desktop"}, &accumulatingHandler{})
	if err != nil || res0 == nil || res0.SessionID == "" {
		t.Fatalf("run 0: res=%+v err=%v", res0, err)
	}
	sid := res0.SessionID
	route := "session:" + sanitizeRouteValue(sid)

	// Run 1 — the long task the user interrupts.
	run1Done := make(chan error, 1)
	go func() {
		_, err := RunAgent(context.Background(), deps,
			RunAgentRequest{Text: primary, Source: "desktop", SessionID: sid},
			&accumulatingHandler{})
		run1Done <- err
	}()

	select {
	case <-tool.started:
	case <-time.After(10 * time.Second):
		t.Fatal("tool never started — run 1 did not reach the mid-tool window")
	}

	// Enter while running: steering inject fires immediately.
	if got := deps.SessionCache.InjectMessage(route, agent.InjectedMessage{
		Text: queued, ClientMessageID: "local-q1",
	}); got != InjectOK {
		t.Fatalf("InjectMessage = %v, want InjectOK", got)
	}

	// Cmd+Enter: the Desktop awaits the retract, cascades the mailbox, then
	// cancels — same order as interruptAndSend.
	if status := deps.SessionCache.RetractInjectWithStatus(route, "local-q1"); status != "retracted" {
		t.Fatalf("retract status = %q, want retracted (inject not yet drained)", status)
	}
	deps.SessionCache.RetractMailboxByClientMessageID(route, "local-q1")
	deps.SessionCache.CancelRoute(route)
	close(tool.release)

	select {
	case <-run1Done:
		// The runner reports a cancelled run as a normal completion carrying
		// the partial result — only the on-disk assertions below matter.
	case <-time.After(10 * time.Second):
		t.Fatal("run 1 did not finish after cancel")
	}

	// Run 2 — the force-send payload as a fresh message on the same session.
	if _, err := RunAgent(context.Background(), deps,
		RunAgentRequest{Text: queued, Source: "desktop", SessionID: sid},
		&accumulatingHandler{}); err != nil {
		t.Fatalf("run 2 (force-send): %v", err)
	}

	if got := countInSessions(t, deps.ShannonDir, preamble); got != 1 {
		files, _ := filepath.Glob(filepath.Join(deps.ShannonDir, "sessions", "*.json"))
		for _, f := range files {
			raw, _ := os.ReadFile(f)
			t.Logf("session %s:\n%s", filepath.Base(f), raw)
		}
		t.Errorf("Bug A regression: preamble persisted %d times, want exactly 1", got)
	}
	if got := countInSessions(t, deps.ShannonDir, queued); got != 1 {
		t.Errorf("Bug B/C regression: queued text persisted %d times, want exactly 1", got)
	}
	if got := countInSessions(t, deps.ShannonDir, queued+"\n"+queued); got != 0 {
		t.Errorf(`"text\ntext" mailbox-prepend fingerprint present %d times, want 0`, got)
	}
}

// TestE2E_LateRetract_ReportsAlreadyCommitted covers the losing side of the
// race through the real runner wiring: the inject drains into the live turn
// BEFORE any retract arrives, so a later retract — even after the run ended —
// must answer "already_committed" (the committed ledger survives run end).
// A client following the contract then drops the draft from its force-send
// payload / composer restore instead of re-sending persisted text.
func TestE2E_LateRetract_ReportsAlreadyCommitted(t *testing.T) {
	const preamble = "Working through the steps:"
	const primary = "run the long analysis"
	const queued = "also translate it to English"

	ts := httptest.NewServer(contentRoutedGateway(t, preamble, primary, queued))
	defer ts.Close()

	deps := runAgentContractTestDeps(t, ts.URL)
	defer deps.SessionCache.CloseAll()
	tool := newBlockingE2ETool()
	deps.Registry.Register(tool)

	res0, err := RunAgent(context.Background(), deps,
		RunAgentRequest{Text: "hello", Source: "desktop"}, &accumulatingHandler{})
	if err != nil || res0 == nil || res0.SessionID == "" {
		t.Fatalf("run 0: res=%+v err=%v", res0, err)
	}
	sid := res0.SessionID
	route := "session:" + sanitizeRouteValue(sid)

	run1Done := make(chan error, 1)
	go func() {
		_, err := RunAgent(context.Background(), deps,
			RunAgentRequest{Text: primary, Source: "desktop", SessionID: sid},
			&accumulatingHandler{})
		run1Done <- err
	}()

	select {
	case <-tool.started:
	case <-time.After(10 * time.Second):
		t.Fatal("tool never started")
	}

	if got := deps.SessionCache.InjectMessage(route, agent.InjectedMessage{
		Text: queued, ClientMessageID: "local-late-1",
	}); got != InjectOK {
		t.Fatalf("InjectMessage = %v, want InjectOK", got)
	}

	// No retract, no cancel: the tool finishes and the loop drains + commits
	// the follow-up at the next iteration boundary, then ends the turn.
	close(tool.release)
	select {
	case err := <-run1Done:
		if err != nil {
			t.Fatalf("run 1: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run 1 did not finish")
	}

	// The committed inject must be visible on disk as one injected user turn.
	if got := countInSessions(t, deps.ShannonDir, queued); got != 1 {
		t.Fatalf("committed follow-up persisted %d times, want 1", got)
	}

	// Late retract (e.g. a pop-back racing the commit): the ledger answers
	// already_committed even though the owning run is gone.
	if status := deps.SessionCache.RetractInjectWithStatus(route, "local-late-1"); status != "already_committed" {
		t.Fatalf("late retract status = %q, want already_committed", status)
	}
}
