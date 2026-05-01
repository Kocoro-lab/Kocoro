package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
