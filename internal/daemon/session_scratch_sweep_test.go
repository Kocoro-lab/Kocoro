package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Interactive-session scratch dirs are kept across session switches (their
// artifacts must outlive OnSessionClose), so disk reclaim happens by age at
// daemon startup instead.
func TestSweepSessionScratchRemovesOnlyOldDirs(t *testing.T) {
	shannonDir := t.TempDir()
	root := filepath.Join(shannonDir, "tmp", "sessions")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}

	oldDir := filepath.Join(root, "session-old")
	newDir := filepath.Join(root, "session-new")
	looseFile := filepath.Join(root, "stray-file.txt")
	for _, d := range []string{oldDir, newDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(oldDir, "screenshot.png"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(looseFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldDir, past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(looseFile, past, past); err != nil {
		t.Fatal(err)
	}

	removed, err := sweepSessionScratch(shannonDir, 24*time.Hour)
	if err != nil {
		t.Fatalf("sweep failed: %v", err)
	}
	if removed != 1 {
		t.Fatalf("expected 1 removal, got %d", removed)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatal("old scratch dir should have been removed")
	}
	if _, err := os.Stat(newDir); err != nil {
		t.Fatal("fresh scratch dir must be kept")
	}
	// Only session DIRECTORIES are swept; unexpected regular files are left
	// alone rather than guessed at.
	if _, err := os.Stat(looseFile); err != nil {
		t.Fatal("non-directory entries must not be touched")
	}
}

func TestSweepSessionScratchMissingRootIsNoop(t *testing.T) {
	removed, err := sweepSessionScratch(t.TempDir(), 24*time.Hour)
	if err != nil {
		t.Fatalf("missing root must be a no-op, got %v", err)
	}
	if removed != 0 {
		t.Fatalf("expected 0 removals, got %d", removed)
	}
}

// sessionScratchDirPath computes and validates the per-session scratch path
// WITHOUT creating it — creation is lazy (first artifact injection) so
// sessions that never produce files leave no empty directories behind.
func TestSessionScratchDirPathDoesNotCreate(t *testing.T) {
	shannonDir := t.TempDir()
	dir, err := sessionScratchDirPath(shannonDir, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if dir == "" {
		t.Fatal("expected a path")
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatal("path-only helper must not create the directory")
	}
	// Traversal-shaped session IDs are rejected, mirroring the mkdir variant.
	if _, err := sessionScratchDirPath(shannonDir, "../escape"); err == nil {
		t.Fatal("expected traversal rejection")
	}
}
