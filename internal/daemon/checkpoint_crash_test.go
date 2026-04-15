package daemon

import (
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// TestCheckpoint_CrashRecovery_DiskLevel simulates a daemon crash between
// a mid-turn checkpoint and the final save, then reloads the session
// from disk and asserts the partial state is preserved with
// InProgress=true. This is the end-to-end disk-level guarantee of
// Slice 4 that can't be exercised via Desktop UI without actually
// force-quitting at the right moment.
func TestCheckpoint_CrashRecovery_DiskLevel(t *testing.T) {
	dir := t.TempDir()
	mgr := session.NewManager(dir)
	defer mgr.Close()

	// --- Set up an active session and simulate a pre-loop user append. ---
	sess := mgr.NewSession()
	sess.CWD = dir
	sess.Messages = append(sess.Messages,
		client.Message{Role: "user", Content: client.NewTextContent("do thing")},
	)
	sess.MessageMeta = append(sess.MessageMeta,
		session.MessageMeta{Source: "test", Timestamp: session.TimePtr(time.Now())},
	)
	if err := mgr.Save(); err != nil {
		t.Fatalf("pre-turn save: %v", err)
	}

	// --- Turn starts: capture baseline (as the daemon runner does). ---
	base := captureTurnBaseline(sess, "test", true)

	// --- Fire a mid-turn checkpoint: simulates tool batch completion. ---
	loop := agent.NewAgentLoop(nil, agent.NewToolRegistry(), "m", "", 1, 1, 1, nil, nil, nil)
	agent.SetRunMessagesForTest(loop, []client.Message{
		{Role: "user", Content: client.NewTextContent("do thing")},
		{Role: "assistant", Content: client.NewTextContent("[tool_use]")},
		{Role: "user", Content: client.NewTextContent("[tool_result payload]")},
	})
	applyTurnState(sess, loop, nil, base) // no usage provider → messages only
	sess.InProgress = true
	if err := mgr.Save(); err != nil {
		t.Fatalf("mid-turn checkpoint save: %v", err)
	}

	// --- DAEMON CRASHES HERE. No final save. ---
	sessionID := sess.ID
	mgr.Close() // drops in-memory state

	// --- Recovery: reload manager + session from disk. ---
	mgr2 := session.NewManager(dir)
	defer mgr2.Close()
	reloaded, err := mgr2.Load(sessionID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	// 1. InProgress flag survives on disk.
	if !reloaded.InProgress {
		t.Fatal("expected InProgress=true on crash-recovered session — partial state would be invisible")
	}

	// 2. Partial transcript is preserved (baseline + tool batch).
	if got := len(reloaded.Messages); got != 3 {
		t.Fatalf("want 3 messages (1 baseline + 2 tool batch), got %d", got)
	}
	if reloaded.Messages[1].Content.Text() != "[tool_use]" {
		t.Fatalf("tool_use message missing or wrong: %q", reloaded.Messages[1].Content.Text())
	}
	if reloaded.Messages[2].Content.Text() != "[tool_result payload]" {
		t.Fatalf("tool_result payload lost")
	}

	// 3. MessageMeta tracks messages (no drift).
	if len(reloaded.MessageMeta) != len(reloaded.Messages) {
		t.Fatalf("meta drift: %d messages vs %d meta", len(reloaded.Messages), len(reloaded.MessageMeta))
	}
}

// TestCheckpoint_ResumeAfterCrash_FinalSaveClears is the companion:
// the resumed session, once it completes its next turn cleanly, must
// end with InProgress=false — proving the flag is a reliable signal
// (not a sticky one-way marker).
func TestCheckpoint_ResumeAfterCrash_FinalSaveClears(t *testing.T) {
	dir := t.TempDir()
	mgr := session.NewManager(dir)
	defer mgr.Close()

	// Simulate a previously-crashed session on disk.
	sess := mgr.NewSession()
	sess.CWD = dir
	sess.InProgress = true
	sess.Messages = []client.Message{
		{Role: "user", Content: client.NewTextContent("earlier prompt")},
		{Role: "assistant", Content: client.NewTextContent("[partial]")},
	}
	sess.MessageMeta = []session.MessageMeta{
		{Source: "test"}, {Source: "test"},
	}
	if err := mgr.Save(); err != nil {
		t.Fatalf("save crashed state: %v", err)
	}
	sessID := sess.ID
	mgr.Close()

	// Reload and run a fresh clean turn.
	mgr2 := session.NewManager(dir)
	defer mgr2.Close()
	_, err := mgr2.Resume(sessID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	current := mgr2.Current()
	if !current.InProgress {
		t.Fatal("resumed session should start with InProgress=true from disk")
	}

	// Daemon runs a successful turn — the final-save path clears the flag.
	current.InProgress = false
	if err := mgr2.Save(); err != nil {
		t.Fatalf("clean final save: %v", err)
	}

	// Reload once more to prove the flag went to disk.
	mgr3 := session.NewManager(dir)
	defer mgr3.Close()
	final, err := mgr3.Load(sessID)
	if err != nil {
		t.Fatalf("final reload: %v", err)
	}
	if final.InProgress {
		t.Fatal("InProgress=true persisted across clean final save — flag is sticky (bug)")
	}
}
