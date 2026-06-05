package cmd

import (
	"context"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	ctxwin "github.com/Kocoro-lab/ShanClaw/internal/context"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// fakeTitleCompleter is a stand-in for the gateway in the one-shot title path:
// it returns a fixed title without an LLM round-trip. Mirrors the fakeCompleter
// used in internal/context/title_gen_test.go.
type fakeTitleCompleter struct{ out string }

func (f *fakeTitleCompleter) Complete(_ context.Context, _ client.CompletionRequest) (*client.CompletionResponse, error) {
	return &client.CompletionResponse{OutputText: f.out}, nil
}

// TestOneShotTitleUsesCompletedTurnCount exercises the one-shot smart-title
// persistence seam end to end (root.go ~L432): a real session.Manager backed by
// a tempdir, a one-shot-shaped transcript (exactly one completed turn), and the
// exact callsite expression — UpgradeTitle with atTurns =
// CountCompletedTurns(sess.Messages). It asserts the persisted session ends with
// the decorated smart title and that PatchAutoTitle recorded the completed-turn
// count (not a hardcoded constant). A one-tool turn produces two assistant
// messages; a raw assistant-message count would record 2 here, so this pins the
// gap-B change to CountCompletedTurns.
func TestOneShotTitleUsesCompletedTurnCount(t *testing.T) {
	mgr := session.NewManager(t.TempDir())
	defer mgr.Close()

	sess := mgr.NewSession()
	// One-shot is a non-IM entry point: Source is left empty, so the smart
	// title is persisted undecorated (DecorateTitle returns the bare title).
	// TitleAuto is stamped true on the placeholder before the run (mirrors the
	// send/runRemote placeholder); it is the precondition PatchAutoTitle checks.
	sess.TitleAuto = true
	sess.Messages = []client.Message{
		{Role: "user", Content: client.NewTextContent("set up a cron job")},
		// A 1-tool turn: tool_use assistant message + final text reply. Two
		// assistant messages, ONE completed turn.
		{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "tool_use", ID: "t1", Name: "bash"},
		})},
		{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "tool_result", ToolUseID: "t1"},
		})},
		{Role: "assistant", Content: client.NewTextContent("Done — scheduled.")},
	}
	if err := mgr.Save(); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	completedTurns := ctxwin.CountCompletedTurns(sess.Messages)
	if completedTurns != 1 {
		t.Fatalf("setup: CountCompletedTurns = %d, want 1", completedTurns)
	}

	gw := &fakeTitleCompleter{out: "Schedule A Cron Job"}
	// Byte-for-byte the root.go one-shot callsite (atTurns wired to
	// CountCompletedTurns, sender/channel "").
	final := ctxwin.UpgradeTitle(context.Background(), gw, mgr, sess.ID, sess.Source, "", "", sess.Messages, ctxwin.CountCompletedTurns(sess.Messages))
	if final != "Schedule A Cron Job" {
		t.Fatalf("UpgradeTitle returned %q, want undecorated smart title", final)
	}

	persisted, err := mgr.Load(sess.ID)
	if err != nil {
		t.Fatalf("reload session: %v", err)
	}
	if persisted.Title != "Schedule A Cron Job" {
		t.Errorf("persisted Title = %q, want the smart title", persisted.Title)
	}
	if persisted.TitleTurns != 1 {
		t.Errorf("persisted TitleTurns = %d, want 1 (the completed-turn count, not the 2 raw assistant messages)", persisted.TitleTurns)
	}
}
