package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

func TestFileRead_Run(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("line1\nline2\nline3\n"), 0644)

	tool := &FileReadTool{}
	result, err := tool.Run(context.Background(), `{"path": "`+path+`"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}
	if !contains(result.Content, "1") || !contains(result.Content, "line1") {
		t.Errorf("expected line-numbered output, got: %s", result.Content)
	}
}

func TestFileReadTool_LargeFileRequiresRange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.txt")
	body := strings.Repeat("0123456789abcdef\n", 20000)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := &FileReadTool{}
	result, err := tool.Run(context.Background(), `{"path":"`+path+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Content, "file is too large") || !strings.Contains(result.Content, "Use offset+limit") {
		t.Fatalf("expected range guidance error, got: %#v", result)
	}
}

func TestFileReadTool_LargeFileRangeSucceeds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.txt")
	var sb strings.Builder
	for i := 0; i < 10000; i++ {
		fmt.Fprintf(&sb, "line-%05d\n", i)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := &FileReadTool{}
	result, err := tool.Run(context.Background(), `{"path":"`+path+`","offset":100,"limit":2}`)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("range read failed: %s", result.Content)
	}
	if !strings.Contains(result.Content, " 101 | line-00100") || !strings.Contains(result.Content, " 102 | line-00101") {
		t.Fatalf("unexpected range output: %s", result.Content)
	}
	if strings.Contains(result.Content, "line-09999") {
		t.Fatalf("range read leaked far-away content")
	}
}

func TestFileRead_ImageReturnsVisionBlock(t *testing.T) {
	dir := t.TempDir()
	// Create a minimal valid PNG (1x1 pixel, red).
	pngData := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, // PNG signature
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52, // IHDR chunk
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
		0xDE, 0x00, 0x00, 0x00, 0x0C, 0x49, 0x44, 0x41,
		0x54, 0x08, 0xD7, 0x63, 0xF8, 0xCF, 0xC0, 0x00,
		0x00, 0x00, 0x03, 0x00, 0x01, 0x36, 0x28, 0x19,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E,
		0x44, 0xAE, 0x42, 0x60, 0x82, // IEND chunk
	}
	path := filepath.Join(dir, "test.png")
	os.WriteFile(path, pngData, 0644)

	tool := &FileReadTool{}
	result, err := tool.Run(context.Background(), `{"path": "`+path+`"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}
	if len(result.Images) != 1 {
		t.Fatalf("expected 1 image block, got %d", len(result.Images))
	}
	if result.Images[0].MediaType != "image/png" {
		t.Errorf("expected image/png, got %s", result.Images[0].MediaType)
	}
	if result.Images[0].Data == "" {
		t.Error("expected non-empty base64 data")
	}
	if !contains(result.Content, "test.png") {
		t.Errorf("expected content to reference filename, got: %s", result.Content)
	}
}

func TestFileRead_ImageTooLarge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.png")
	// Create file just over the limit
	f, _ := os.Create(path)
	f.Truncate(maxImageReadSize + 1)
	f.Close()

	tool := &FileReadTool{}
	result, err := tool.Run(context.Background(), `{"path": "`+path+`"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for oversized image")
	}
	if !contains(result.Content, "too large") {
		t.Errorf("expected 'too large' message, got: %s", result.Content)
	}
}

func TestFileRead_NotFound(t *testing.T) {
	tool := &FileReadTool{}
	result, err := tool.Run(context.Background(), `{"path": "/nonexistent/file.txt"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for missing file")
	}
}

func contains(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && indexOf(s, substr) >= 0
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// TestFileRead_RelativePathRefusedWithoutSessionCWD ensures file_read no
// longer silently falls back to os.Getwd() when no session CWD is set.
func TestFileRead_RelativePathRefusedWithoutSessionCWD(t *testing.T) {
	tool := &FileReadTool{}
	result, err := tool.Run(context.Background(), `{"path":"relative.txt"}`)
	if err != nil {
		t.Fatalf("Run should not return a transport error, got %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected error result when session CWD unset and path is relative, got: %s", result.Content)
	}
	if !contains(result.Content, "session working directory") && !contains(result.Content, "absolute path") {
		t.Errorf("expected guard message, got: %s", result.Content)
	}
}

// TestFileRead_OversizeThrows: a file whose content exceeds fileReadMaxTokens
// must return an IsError result with offset+limit guidance, NOT silently
// truncate or fall through to the loop's spill path.
func TestFileRead_OversizeThrows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	// 10K lines × 30 chars = 300K chars ≈ 100K tokens (well above 25K cap)
	var sb strings.Builder
	for i := 0; i < 10000; i++ {
		sb.WriteString("0123456789012345678901234567890\n")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &FileReadTool{}
	args, _ := json.Marshal(fileReadArgs{Path: path})
	result, err := tool.Run(context.Background(), string(args))
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected IsError on oversized read, got success with %d bytes", len(result.Content))
	}
	if !strings.Contains(result.Content, "too large") {
		t.Errorf("error must mention 'too large', got: %s", result.Content)
	}
	if !strings.Contains(result.Content, "offset") || !strings.Contains(result.Content, "limit") {
		t.Errorf("error must guide to offset+limit, got: %s", result.Content)
	}
	// Sanity: error is short (~100B target), not the full file content.
	if len(result.Content) > 1000 {
		t.Errorf("error message should be short (~100B), got %d bytes", len(result.Content))
	}
}

// TestFileRead_OversizeRespectsLimit: same big file, but with a reasonable
// limit slice — must succeed (the cap is on the SLICE, not the file).
func TestFileRead_OversizeRespectsLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	var sb strings.Builder
	for i := 0; i < 10000; i++ {
		sb.WriteString("0123456789012345678901234567890\n")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &FileReadTool{}
	args, _ := json.Marshal(fileReadArgs{Path: path, Limit: 100})
	result, err := tool.Run(context.Background(), string(args))
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("100-line slice of big file should succeed, got error: %s", result.Content)
	}
	// 100 lines × ~33 chars = ~3300 chars ~ 1100 tokens — well below 25K cap.
	// Verify content has the line-number prefix and reasonable length.
	if !strings.Contains(result.Content, "   1 |") {
		t.Errorf("expected line number prefix in slice content, got first 200 bytes: %s", result.Content[:min(200, len(result.Content))])
	}
}

// TestFileRead_DedupSameFile_SameRange verifies that two reads of the same
// (path, offset, limit) tuple within one session — with no file modification
// in between — return a short "unchanged" stub on the second call.
func TestFileRead_DedupSameFile_SameRange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("line1\nline2\nline3\n"), 0o644)

	tracker := agent.NewReadTracker()
	ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)

	tool := &FileReadTool{}

	// First read: full content
	args, _ := json.Marshal(fileReadArgs{Path: path})
	r1, err := tool.Run(ctx, string(args))
	if err != nil {
		t.Fatalf("first read transport error: %v", err)
	}
	if r1.IsError {
		t.Fatalf("first read error: %s", r1.Content)
	}
	if !strings.Contains(r1.Content, "line1") {
		t.Errorf("first read should contain content, got: %s", r1.Content)
	}

	// Second read with SAME args: should dedup → stub
	r2, err := tool.Run(ctx, string(args))
	if err != nil {
		t.Fatalf("second read transport error: %v", err)
	}
	if r2.IsError {
		t.Fatalf("dedup hit should not be IsError: %s", r2.Content)
	}
	if !strings.Contains(r2.Content, "unchanged since last read") {
		t.Errorf("expected dedup stub, got: %s", r2.Content)
	}
	if strings.Contains(r2.Content, "line1") {
		t.Errorf("dedup stub should NOT contain file content, got: %s", r2.Content)
	}
	if len(r2.Content) > 200 {
		t.Errorf("dedup stub should be short (~120B), got %d bytes", len(r2.Content))
	}
}

// TestFileRead_DedupSameFile_DifferentRange: a second read with different
// offset+limit must NOT dedup — model is asking for a different slice.
func TestFileRead_DedupSameFile_DifferentRange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("line1\nline2\nline3\nline4\nline5\n"), 0o644)

	tracker := agent.NewReadTracker()
	ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)

	tool := &FileReadTool{}

	// First read: limit=2 (lines 1-2)
	args1, _ := json.Marshal(fileReadArgs{Path: path, Limit: 2})
	r1, _ := tool.Run(ctx, string(args1))
	if r1.IsError {
		t.Fatalf("first read error: %s", r1.Content)
	}

	// Second read with different limit=4 — must return real content, NOT stub
	args2, _ := json.Marshal(fileReadArgs{Path: path, Limit: 4})
	r2, err := tool.Run(ctx, string(args2))
	if err != nil {
		t.Fatalf("second read transport error: %v", err)
	}
	if r2.IsError {
		t.Fatalf("second read should succeed: %s", r2.Content)
	}
	if strings.Contains(r2.Content, "unchanged since last read") {
		t.Errorf("different range must NOT dedup, got stub: %s", r2.Content)
	}
	if !strings.Contains(r2.Content, "line4") {
		t.Errorf("expected line4 in expanded read, got: %s", r2.Content)
	}
}

// TestFileRead_DedupSameFile_FileModified: when the file is modified between
// reads, dedup must NOT fire (mtime check catches it).
func TestFileRead_DedupSameFile_FileModified(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("v1\n"), 0o644)

	tracker := agent.NewReadTracker()
	ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)

	tool := &FileReadTool{}
	args, _ := json.Marshal(fileReadArgs{Path: path})
	tool.Run(ctx, string(args)) // first read

	// Modify file (sleep 10ms first to ensure mtime ticks on filesystems
	// with sub-second mtime resolution; macOS APFS has nanosecond mtime
	// but kernel timer ticks may coalesce).
	time.Sleep(15 * time.Millisecond)
	os.WriteFile(path, []byte("v2 changed\n"), 0o644)

	r2, err := tool.Run(ctx, string(args))
	if err != nil {
		t.Fatalf("second read transport error: %v", err)
	}
	if r2.IsError {
		t.Fatalf("second read error: %s", r2.Content)
	}
	if strings.Contains(r2.Content, "unchanged since last read") {
		t.Errorf("modified file must NOT dedup, got stub: %s", r2.Content)
	}
	if !strings.Contains(r2.Content, "v2 changed") {
		t.Errorf("expected new content, got: %s", r2.Content)
	}
}

// TestFileRead_DedupSameFile_NoTracker: without a tracker in context, dedup
// is a no-op (always returns full content).
func TestFileRead_DedupSameFile_NoTracker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("line1\n"), 0o644)

	tool := &FileReadTool{}
	args, _ := json.Marshal(fileReadArgs{Path: path})

	// Two reads in plain context — both return full content.
	r1, _ := tool.Run(context.Background(), string(args))
	r2, _ := tool.Run(context.Background(), string(args))
	for i, r := range []agent.ToolResult{r1, r2} {
		if r.IsError {
			t.Fatalf("read %d error: %s", i, r.Content)
		}
		if !strings.Contains(r.Content, "line1") {
			t.Errorf("read %d should contain content, got: %s", i, r.Content)
		}
		if strings.Contains(r.Content, "unchanged") {
			t.Errorf("read %d should not dedup without tracker, got: %s", i, r.Content)
		}
	}
}
