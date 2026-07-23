package daemon

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

func writeInterruptedSession(t *testing.T, dir, id string, state *session.InterruptedTurn) {
	t.Helper()
	mgr := session.NewManager(dir)
	defer mgr.Close()
	sess := mgr.NewSessionWithID(id)
	now := time.Now().Add(-time.Minute)
	sess.Messages = []client.Message{
		{Role: "user", Content: client.NewTextContent("inspect the project")},
		{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolUseBlock("tool-1", "file_read", []byte(`{"path":"README.md"}`)),
		})},
		{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolResultBlock("tool-1", "saved file contents", false),
		})},
	}
	sess.MessageMeta = []session.MessageMeta{
		{Source: "desktop", Timestamp: &now},
		{Source: "desktop", Timestamp: &now},
		{Source: "desktop", Timestamp: &now},
	}
	sess.Source = "desktop"
	sess.InProgress = true
	sess.InterruptedTurn = state
	if err := mgr.Save(); err != nil {
		t.Fatalf("save interrupted session: %v", err)
	}
}

func TestDiscoverInterruptedTurns_DefaultAndNamedAgent(t *testing.T) {
	shannonDir := t.TempDir()
	writeInterruptedSession(t, filepath.Join(shannonDir, "sessions"), "recovery-default-001", nil)
	writeInterruptedSession(t, filepath.Join(shannonDir, "agents", "reviewer", "sessions"), "recovery-agent-001", &session.InterruptedTurn{
		Agent:  "wrong-persisted-agent",
		Source: "web",
	})

	mgr := session.NewManager(filepath.Join(shannonDir, "sessions"))
	complete := mgr.NewSessionWithID("complete-session-001")
	complete.Messages = []client.Message{{Role: "user", Content: client.NewTextContent("done")}}
	if err := mgr.Save(); err != nil {
		t.Fatal(err)
	}
	_ = mgr.Close()

	got, err := discoverInterruptedTurns(shannonDir)
	if err != nil {
		t.Fatalf("discoverInterruptedTurns: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("candidates = %d, want 2: %#v", len(got), got)
	}
	byID := map[string]interruptedTurnCandidate{}
	for _, candidate := range got {
		byID[candidate.SessionID] = candidate
	}
	if byID["recovery-default-001"].Agent != "" {
		t.Fatalf("default candidate agent = %q", byID["recovery-default-001"].Agent)
	}
	if byID["recovery-agent-001"].Agent != "reviewer" ||
		byID["recovery-agent-001"].State.Agent != "reviewer" {
		t.Fatalf("named candidate must use directory authority: %#v", byID["recovery-agent-001"])
	}
	if _, exists := byID["complete-session-001"]; exists {
		t.Fatal("completed session was incorrectly scheduled for recovery")
	}
}

func TestResumeInterruptedTurns_ContinuesCheckpointWithoutToolReplay(t *testing.T) {
	gw := &fakeGatewayBackend{reply: "continued from the saved result"}
	ts := httptest.NewServer(gw.handler())
	defer ts.Close()

	deps := runAgentContractTestDeps(t, ts.URL)
	defer deps.SessionCache.CloseAll()
	id := "recovery-session-001"
	writeInterruptedSession(t, filepath.Join(deps.ShannonDir, "sessions"), id, &session.InterruptedTurn{
		Source: "heartbeat",
	})

	srv := &Server{deps: deps}
	srv.resumeInterruptedTurns(context.Background())

	mgr := session.NewManager(filepath.Join(deps.ShannonDir, "sessions"))
	defer mgr.Close()
	sess, err := mgr.Load(id)
	if err != nil {
		t.Fatalf("load resumed session: %v", err)
	}
	if sess.InProgress {
		t.Fatal("successful recovery left InProgress set")
	}
	if sess.InterruptedTurn != nil {
		t.Fatalf("successful recovery left metadata: %#v", sess.InterruptedTurn)
	}

	var injectedContinuation int
	var assistantReply bool
	for i, msg := range sess.Messages {
		if msg.Role == "user" && msg.Content.Text() == interruptedTurnContinuation {
			injectedContinuation++
			if i >= len(sess.MessageMeta) || !sess.MessageMeta[i].SystemInjected {
				t.Fatalf("continuation at %d is not marked SystemInjected", i)
			}
		}
		if msg.Role == "assistant" && msg.Content.Text() == "continued from the saved result" {
			assistantReply = true
		}
	}
	if injectedContinuation != 1 {
		t.Fatalf("injected continuation count = %d, want 1", injectedContinuation)
	}
	if !assistantReply {
		t.Fatal("resumed assistant reply missing")
	}
	for _, msg := range sess.HistoryForLoop() {
		if msg.Role == "user" && msg.Content.Text() == interruptedTurnContinuation {
			t.Fatal("system-injected recovery marker leaked into later turn history")
		}
	}

	requests := gw.requests()
	if len(requests) == 0 {
		t.Fatal("recovery did not call the gateway")
	}
	var sawSavedToolResult bool
	for _, request := range requests {
		for _, msg := range request.Messages {
			for _, block := range msg.Content.Blocks() {
				if block.Type == "tool_result" && block.ToolUseID == "tool-1" {
					sawSavedToolResult = true
				}
			}
		}
	}
	if !sawSavedToolResult {
		t.Fatal("gateway did not receive the checkpointed tool result")
	}
}
