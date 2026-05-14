package claudecode

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanAgents_SingleFile(t *testing.T) {
	src := filepath.Join("testdata", "claude_home_basic")
	got, _, err := scanAgents(src)
	if err != nil {
		t.Fatalf("scanAgents: %v", err)
	}
	if len(got) != 1 || got[0].Name != "code-reviewer" {
		t.Fatalf("expected 1 agent code-reviewer, got %+v", got)
	}
	if got[0].Status != "ok" {
		t.Errorf("status = %q", got[0].Status)
	}
	if got[0].ContentHash == "" {
		t.Error("content hash should be set")
	}
}

func TestScanAgents_SymlinkEntryRejected(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("CONFIDENTIAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "agents", "leak.md")); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}

	got, warns, err := scanAgents(home)
	if err != nil {
		t.Fatalf("scanAgents: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("symlinked agent should be skipped, got %+v", got)
	}
	gotEscape := false
	for _, w := range warns {
		if w.Kind == "symlink_escape" && w.Path == "~/.claude/agents/leak.md" {
			gotEscape = true
		}
	}
	if !gotEscape {
		t.Errorf("expected symlink_escape warning, got %+v", warns)
	}
}
