package claudecode

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestScanCommands_FlatMarkdown(t *testing.T) {
	src := filepath.Join("testdata", "claude_home_basic")
	got, _, err := scanCommands(src)
	if err != nil {
		t.Fatalf("scanCommands: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2, got %d: %+v", len(got), got)
	}
	names := []string{got[0].Name, got[1].Name}
	sort.Strings(names)
	if names[0] != "deploy" || names[1] != "review" {
		t.Errorf("names = %v", names)
	}
}

func TestScanCommands_SymlinkEntryRejected(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "commands"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("CONFIDENTIAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "commands", "leak.md")); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}

	got, warns, err := scanCommands(home)
	if err != nil {
		t.Fatalf("scanCommands: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("symlinked command should be skipped, got %+v", got)
	}
	gotEscape := false
	for _, w := range warns {
		if w.Kind == "symlink_escape" && w.Path == "~/.claude/commands/leak.md" {
			gotEscape = true
		}
	}
	if !gotEscape {
		t.Errorf("expected symlink_escape warning, got %+v", warns)
	}
}

func TestScanCommands_InvalidSkillSlugWarnsAndSkips(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "commands"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "commands", "review.md"), []byte("# Review\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "commands", "my_cmd.md"), []byte("# Bad\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	longName := strings.Repeat("a", 50)
	if err := os.WriteFile(filepath.Join(home, "commands", longName+".md"), []byte("# Too long\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, warns, err := scanCommands(home)
	if err != nil {
		t.Fatalf("scanCommands: %v", err)
	}
	if len(got) != 1 || got[0].Name != "review" {
		t.Fatalf("scanned commands = %+v, want only review", got)
	}
	invalid := 0
	for _, w := range warns {
		if w.Kind == "invalid_name" {
			invalid++
		}
	}
	if invalid != 2 {
		t.Fatalf("invalid_name warnings = %d, want 2: %+v", invalid, warns)
	}
}
