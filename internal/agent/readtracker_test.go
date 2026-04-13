package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
)

func TestReadTracker_MarkAndHasRead(t *testing.T) {
	rt := NewReadTracker()

	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("hello"), 0644)

	if rt.HasRead(path) {
		t.Error("expected HasRead to return false before MarkRead")
	}

	rt.MarkRead(path)

	if !rt.HasRead(path) {
		t.Error("expected HasRead to return true after MarkRead")
	}
}

func TestReadTracker_RelativePath(t *testing.T) {
	rt := NewReadTracker()

	// Create a file in a temp dir. The tracker should treat absolute and
	// relative paths as the same file when SetCWD is called — this mirrors
	// the explicit-CWD contract the rest of the filesystem tools enforce
	// after the CWD-hardening work; process cwd is NOT consulted.
	dir := t.TempDir()
	path := filepath.Join(dir, "rel.txt")
	os.WriteFile(path, []byte("data"), 0644)

	rt.SetCWD(dir)

	// Mark with absolute, check with relative
	rt.MarkRead(path)
	if !rt.HasRead("rel.txt") {
		t.Error("expected HasRead to match relative path against absolute after SetCWD")
	}
}

// TestReadTracker_RelativePathWithoutCWD verifies that a relative path with
// no SetCWD is treated as a distinct key from its absolute form — the
// tracker must NOT silently resolve against the process cwd.
func TestReadTracker_RelativePathWithoutCWD(t *testing.T) {
	rt := NewReadTracker()

	dir := t.TempDir()
	abs := filepath.Join(dir, "rel.txt")
	os.WriteFile(abs, []byte("data"), 0644)

	rt.MarkRead(abs)
	if rt.HasRead("rel.txt") {
		t.Error("expected relative path to not match absolute when no CWD is set")
	}
}

func TestReadTracker_Symlink(t *testing.T) {
	rt := NewReadTracker()

	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	link := filepath.Join(dir, "link.txt")
	os.WriteFile(real, []byte("data"), 0644)
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks not supported")
	}

	// Mark the symlink as read, check via real path
	rt.MarkRead(link)
	if !rt.HasRead(real) {
		t.Error("expected HasRead to resolve symlinks")
	}
}

func TestReadTracker_EmptyPath(t *testing.T) {
	rt := NewReadTracker()
	rt.MarkRead("")
	if rt.HasRead("") {
		t.Error("expected empty path to not be tracked")
	}
}

func TestReadTracker_NonexistentFile(t *testing.T) {
	rt := NewReadTracker()

	dir := t.TempDir()
	path := filepath.Join(dir, "noexist.txt")

	// Should still work (normalization falls back to clean path)
	rt.MarkRead(path)
	if !rt.HasRead(path) {
		t.Error("expected HasRead to work for nonexistent files")
	}
}

func TestReadTracker_NormalizesWithSessionCWD(t *testing.T) {
	rt := NewReadTracker()
	rt.SetCWD("/projects/foo")

	rt.MarkRead("src/main.go")
	if !rt.HasRead("src/main.go") {
		t.Error("should find relative path after MarkRead")
	}
	if !rt.HasRead("/projects/foo/src/main.go") {
		t.Error("should find absolute path equivalent")
	}
	if rt.HasRead("/other/src/main.go") {
		t.Error("should not match different absolute path")
	}
}

func TestIsMemoryFile_UsesSessionCWD(t *testing.T) {
	memDir := t.TempDir()
	ctx := context.Background()
	ctx = WithMemoryDir(ctx, memDir)
	ctx = cwdctx.WithSessionCWD(ctx, "/projects/foo")

	// Absolute path to memory file
	if !IsMemoryFile(ctx, filepath.Join(memDir, "MEMORY.md")) {
		t.Error("absolute path to MEMORY.md should match")
	}
}
