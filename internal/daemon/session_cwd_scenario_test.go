package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
)

// Scenario: regression guard against the class of drift that produced the
// note.com Slack session incident. A cloud-routed request arrives with no
// CWD, the daemon allocates scratch under ~/.shannon/tmp/sessions/<id>/,
// wires it into the context, tools read/write against it, and session close
// reclaims the space. Any single hop failing here used to produce a
// 120-second `bash find` hang on the production session.
//
// Individual hops have their own unit tests (session_cwd_test.go for the
// allocator, internal/cwdctx for ResolveFilesystemPath, internal/tools for
// the MCP path-rewrite). This scenario pins down the integration between
// daemon allocator, cwdctx propagation, and session-close cleanup so future
// refactors cannot silently unlink them.
func TestScenario_CloudSessionCWD_EndToEnd(t *testing.T) {
	shannonDir := t.TempDir()
	const sessionID = "scenario-slack-session"

	// 1) Allocator — the runner calls this when request/resumed/agent CWD
	// are all empty and the source is cloud-routed.
	cwd, err := ensureCloudSessionTmpDir(shannonDir, sessionID, "slack")
	if err != nil || cwd == "" {
		t.Fatalf("allocator failed: cwd=%q err=%v", cwd, err)
	}
	expected := filepath.Join(shannonDir, "tmp", "sessions", sessionID)
	if cwd != expected {
		t.Errorf("unexpected scratch path: got %q, want %q", cwd, expected)
	}

	// 2) Wire the scratch dir into a context the way runner.go does via
	// cwdctx.WithSessionCWD.
	ctx := cwdctx.WithSessionCWD(context.Background(), cwd)

	// 3) Simulate a file-producing tool (browser_snapshot, screenshot, etc.)
	// writing into the scratch dir. The MCP adapter's path rewrite (tested
	// in internal/tools) is what guarantees the absolute path lands here;
	// we short-circuit that hop and write directly so the scenario focuses
	// on daemon-owned behavior.
	absFile := filepath.Join(cwd, "note_editor.md")
	if err := os.WriteFile(absFile, []byte("snapshot content"), 0o600); err != nil {
		t.Fatalf("simulated tool write: %v", err)
	}

	// 4) Now a downstream agent call (file_read, bash, etc.) passes the
	// same relative filename the model originally used. cwdctx must
	// resolve it against the scratch dir rather than erroring with
	// ErrNoSessionCWD — that error is the exact production failure mode
	// this scenario guards against.
	resolved, err := cwdctx.ResolveFilesystemPath(ctx, "note_editor.md")
	if err != nil {
		t.Fatalf("ResolveFilesystemPath on relative name failed: %v", err)
	}
	if resolved != absFile {
		t.Errorf("resolver returned %q, want %q", resolved, absFile)
	}
	data, err := os.ReadFile(resolved)
	if err != nil || string(data) != "snapshot content" {
		t.Errorf("round-trip read mismatch: data=%q err=%v", data, err)
	}

	// 5) Session close. The cleanup callback is what sessMgr.OnSessionClose
	// invokes on cache eviction / daemon shutdown.
	cloudSessionTmpCleanup(cwd)()
	if _, err := os.Stat(cwd); !os.IsNotExist(err) {
		t.Fatalf("scratch dir still present after cleanup: %v", err)
	}

	// 6) A subsequent resume of the same session must re-allocate cleanly —
	// the scratch is deliberately NOT persisted to sess.CWD, so the resume
	// path re-creates the directory rather than looking up a dead value.
	// This invariant is the PR's fix for the reviewer-flagged lifecycle bug
	// (persisting a now-deleted path would break ValidateCWD on resume).
	reallocated, err := ensureCloudSessionTmpDir(shannonDir, sessionID, "slack")
	if err != nil || reallocated != cwd {
		t.Errorf("resume re-alloc: got (%q, %v), want (%q, nil)", reallocated, err, cwd)
	}
	if _, err := os.Stat(reallocated); err != nil {
		t.Errorf("re-allocated dir not usable: %v", err)
	}
}

// Negative scenario: a non-cloud source (e.g. Desktop, CLI) that arrives
// with no CWD keeps the "no filesystem scope" contract — the allocator
// returns empty and filesystem tools enforce ErrNoSessionCWD. Regressing
// this would silently invent a working directory for every local request
// and poison the scope-checking behavior the filesystem tools rely on.
func TestScenario_NonCloudSource_KeepsNoScopeContract(t *testing.T) {
	shannonDir := t.TempDir()
	cwd, err := ensureCloudSessionTmpDir(shannonDir, "some-desktop-session", "desktop")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cwd != "" {
		t.Fatalf("desktop source got an allocated scratch: %q", cwd)
	}

	// With no session CWD on ctx, relative-path filesystem resolution must
	// fail loudly — no silent fallback to $HOME or the daemon process cwd.
	ctx := context.Background()
	if _, err := cwdctx.ResolveFilesystemPath(ctx, "anything.txt"); err == nil {
		t.Error("ResolveFilesystemPath should refuse relative path without session CWD")
	}
}
