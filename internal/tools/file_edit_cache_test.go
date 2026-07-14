package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// TestFileEdit_FailureInvalidatesReadDedup reproduces the R1 deadlock's
// downstream half: after a file_edit that fails to match, the file is untouched
// (mtime/size unchanged), so a re-read with the same args would normally hit the
// file_read dedup "unchanged" stub and the agent could never inspect the real
// bytes. A failed edit must invalidate that file's dedup so the next read is
// fresh.
func TestFileEdit_FailureInvalidatesReadDedup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("line one\nline two\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tracker := agent.NewReadTracker()
	ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)
	readTool := &FileReadTool{}
	editTool := &FileEditTool{}

	readArgs, _ := json.Marshal(fileReadArgs{Path: path, Description: "read"})

	// 1. First read records a dedup entry for (path, 0, 0).
	r1, err := readTool.Run(ctx, string(readArgs))
	if err != nil || r1.IsError {
		t.Fatalf("first read failed: err=%v content=%s", err, r1.Content)
	}
	tracker.MarkRead(path) // mirrors loop.go after a successful file_read

	// 2. An edit whose old_string cannot match even fuzzily → fails, file untouched.
	editArgs, _ := json.Marshal(fileEditArgs{
		Path: path, OldString: "TOTALLY ABSENT XYZ", NewString: "x", Description: "edit",
	})
	e1, err := editTool.Run(ctx, string(editArgs))
	if err != nil {
		t.Fatalf("edit transport error: %v", err)
	}
	if !e1.IsError {
		t.Fatalf("expected edit to fail on an absent old_string, got: %s", e1.Content)
	}

	// 3. Re-read with identical args MUST return real content, not the stub.
	r2, err := readTool.Run(ctx, string(readArgs))
	if err != nil || r2.IsError {
		t.Fatalf("re-read failed: err=%v content=%s", err, r2.Content)
	}
	if contains(r2.Content, "unchanged since last read") {
		t.Errorf("dedup stub returned after a failed edit — cache not invalidated:\n%s", r2.Content)
	}
	if !contains(r2.Content, "line one") {
		t.Errorf("expected real file content on re-read, got:\n%s", r2.Content)
	}
}

// TestFileRead_DedupStubPointsToContext: the unchanged stub must point the
// model back to the earlier file_read result in context, not merely
// say "unchanged" — so it knows the current content is already available and
// doesn't spin trying to re-read.
func TestFileRead_DedupStubPointsToContext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	os.WriteFile(path, []byte("alpha\nbeta\n"), 0o644)

	tracker := agent.NewReadTracker()
	ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)
	tool := &FileReadTool{}
	args, _ := json.Marshal(fileReadArgs{Path: path, Description: "read"})

	if _, err := tool.Run(ctx, string(args)); err != nil { // first read
		t.Fatal(err)
	}
	r2, err := tool.Run(ctx, string(args)) // dedup stub
	if err != nil {
		t.Fatal(err)
	}
	if !contains(r2.Content, "unchanged since last read") {
		t.Fatalf("expected dedup stub, got: %s", r2.Content)
	}
	if !contains(r2.Content, "earlier") || !contains(r2.Content, "file_read") {
		t.Errorf("stub should point back to the earlier file_read result in context, got: %s", r2.Content)
	}
}

// TestFileEdit_NotFound_DiagnosesClosestLine: when old_string has a real content
// difference that fuzzy matching cannot bridge, the "not found" error should
// point at the closest file line and echo its actual bytes — so the model sees
// the difference (here 99 vs 42) without a re-read — the error echoes the actual
// file line, not only the string the model searched for.
func TestFileEdit_NotFound_DiagnosesClosestLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	os.WriteFile(path, []byte("alpha\nconfigValue := computeThing(42)\nbeta\n"), 0o644)

	tracker := agent.NewReadTracker()
	tracker.MarkRead(path)
	ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)

	tool := &FileEditTool{}
	args, _ := json.Marshal(fileEditArgs{
		Path: path, OldString: "configValue := computeThing(99)", NewString: "x", Description: "edit",
	})
	r, err := tool.Run(ctx, string(args))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !r.IsError {
		t.Fatalf("expected not-found error, got: %s", r.Content)
	}
	if !contains(r.Content, "line 2") {
		t.Errorf("diagnostic should point at the closest line (2), got: %s", r.Content)
	}
	if !contains(r.Content, "computeThing(42)") {
		t.Errorf("diagnostic should echo the actual file line bytes, got: %s", r.Content)
	}
}
