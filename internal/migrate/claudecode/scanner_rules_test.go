package claudecode

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanRules_Present(t *testing.T) {
	src := filepath.Join("testdata", "claude_home_basic")
	got, _, err := scanRules(src)
	if err != nil {
		t.Fatalf("scanRules: %v", err)
	}
	if got == nil {
		t.Fatal("expected rules, got nil")
	}
	if got.Status != "ok" {
		t.Errorf("status = %q", got.Status)
	}
}

func TestScanRules_Missing(t *testing.T) {
	got, _, err := scanRules(filepath.Join("testdata", "claude_home_no_rules"))
	if err != nil {
		t.Fatalf("scanRules: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestScanRules_SymlinkFileRejected(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "CLAUDE.md")
	if err := os.WriteFile(outside, []byte("CONFIDENTIAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "CLAUDE.md")); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}

	got, warns, err := scanRules(home)
	if err != nil {
		t.Fatalf("scanRules: %v", err)
	}
	if got != nil {
		t.Errorf("symlinked rules should be skipped, got %+v", got)
	}
	gotEscape := false
	for _, w := range warns {
		if w.Kind == "symlink_escape" && w.Path == "~/.claude/CLAUDE.md" {
			gotEscape = true
		}
	}
	if !gotEscape {
		t.Errorf("expected symlink_escape warning, got %+v", warns)
	}
}
