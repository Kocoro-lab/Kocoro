package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSpillToDisk_RejectsEmptyShannonDir is the regression for the bug
// where filepath.Join("", "tmp") = "tmp" — a relative path — caused spill
// files to land in the process cwd (e.g. internal/agent/tmp/ during unit
// tests). The guard now refuses to write when shannonDir is empty.
func TestSpillToDisk_RejectsEmptyShannonDir(t *testing.T) {
	_, err := spillToDisk("", "sess1", "call1", "content")
	if err == nil {
		t.Fatal("expected spillToDisk to reject empty shannonDir")
	}
	if !strings.Contains(err.Error(), "shannonDir") {
		t.Errorf("expected error to mention shannonDir, got: %v", err)
	}
}

func TestSpillToDisk_SmallResult(t *testing.T) {
	// Results under threshold should not be spilled (caller checks threshold).
	// This test verifies spillToDisk works even for small content.
	dir := t.TempDir()
	content := "small output"
	preview, err := spillToDisk(dir, "sess1", "call1", content)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(preview, "Output saved to disk") {
		t.Fatal("expected spill preview header")
	}
	if !strings.Contains(preview, "small output") {
		t.Fatal("expected full content in preview for small output")
	}
}

func TestSpillToDisk_LargeResult(t *testing.T) {
	dir := t.TempDir()
	// Build a 60K rune string.
	content := strings.Repeat("x", 60000)
	preview, err := spillToDisk(dir, "sess1", "call2", content)
	if err != nil {
		t.Fatal(err)
	}

	// Preview should contain the file path and char count.
	if !strings.Contains(preview, "60000 chars") {
		t.Fatalf("expected char count in preview, got: %s", preview[:200])
	}

	// Preview should be truncated to ~2000 chars of content + header.
	previewContent := preview[strings.Index(preview, "Preview (first 2000 chars):\n")+len("Preview (first 2000 chars):\n"):]
	if len([]rune(previewContent)) != spillPreviewChars {
		t.Fatalf("expected preview content of %d runes, got %d", spillPreviewChars, len([]rune(previewContent)))
	}

	// Full content should be on disk.
	path := filepath.Join(dir, "tmp", "tool_result_sess1_call2.txt")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("spill file not readable: %v", err)
	}
	if len(data) != 60000 {
		t.Fatalf("expected 60000 bytes on disk, got %d", len(data))
	}
}

func TestSpillToDisk_RuneSafe(t *testing.T) {
	dir := t.TempDir()
	// Multi-byte chars: each is 3 bytes in UTF-8.
	content := strings.Repeat("あ", 60000)
	preview, err := spillToDisk(dir, "sess1", "call3", content)
	if err != nil {
		t.Fatal(err)
	}
	// Preview content should be exactly 2000 runes, not bytes.
	idx := strings.Index(preview, "Preview (first 2000 chars):\n")
	previewContent := preview[idx+len("Preview (first 2000 chars):\n"):]
	if len([]rune(previewContent)) != spillPreviewChars {
		t.Fatalf("expected %d runes in preview, got %d", spillPreviewChars, len([]rune(previewContent)))
	}
}

func TestCleanupSpills(t *testing.T) {
	dir := t.TempDir()
	// Create spill files for two sessions.
	spillToDisk(dir, "sess1", "a", "data-a")
	spillToDisk(dir, "sess1", "b", "data-b")
	spillToDisk(dir, "sess2", "c", "data-c")

	// Cleanup sess1 only.
	cleanupSpills(dir, "sess1")

	// sess1 files should be gone.
	matches, _ := filepath.Glob(filepath.Join(dir, "tmp", "tool_result_sess1_*.txt"))
	if len(matches) != 0 {
		t.Fatalf("expected 0 sess1 files after cleanup, got %d", len(matches))
	}

	// sess2 file should remain.
	matches, _ = filepath.Glob(filepath.Join(dir, "tmp", "tool_result_sess2_*.txt"))
	if len(matches) != 1 {
		t.Fatalf("expected 1 sess2 file, got %d", len(matches))
	}
}

// TestSpillFiles_SurviveBetweenRuns verifies that spill files created in one
// run are still readable before cleanup (session close). This is the regression
// test for the bug where SpillCleanupFunc was deferred per-Run, deleting files
// that subsequent turns could still reference.
func TestSpillFiles_SurviveBetweenRuns(t *testing.T) {
	dir := t.TempDir()

	// Turn 1: spill a large result
	_, err := spillToDisk(dir, "sess1", "call1", strings.Repeat("x", 60000))
	if err != nil {
		t.Fatal(err)
	}

	// Between turns: file must still exist
	path1 := filepath.Join(dir, "tmp", "tool_result_sess1_call1.txt")
	if _, err := os.Stat(path1); err != nil {
		t.Fatalf("spill file should survive between turns: %v", err)
	}

	// Turn 2: spill another result
	_, err = spillToDisk(dir, "sess1", "call2", strings.Repeat("y", 60000))
	if err != nil {
		t.Fatal(err)
	}

	// Both files should exist (cleanup hasn't run yet)
	path2 := filepath.Join(dir, "tmp", "tool_result_sess1_call2.txt")
	for _, p := range []string{path1, path2} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("spill file should exist before session close: %v", err)
		}
	}

	// Session close: cleanup
	cleanupSpills(dir, "sess1")

	// Now both should be gone
	for _, p := range []string{path1, path2} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("spill file should be gone after session close: %s", p)
		}
	}
}

func TestSpillThresholdIntegration(t *testing.T) {
	// Verify that content under spillThreshold would not trigger spill
	// (the check is in loop.go, but we verify the constant here).
	under := strings.Repeat("x", spillThreshold)
	if len([]rune(under)) > spillThreshold {
		t.Fatal("threshold constant mismatch")
	}
	over := strings.Repeat("x", spillThreshold+1)
	if len([]rune(over)) <= spillThreshold {
		t.Fatal("threshold constant mismatch")
	}
}
