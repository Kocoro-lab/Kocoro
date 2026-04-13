package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
)

func TestGrep_FindsMatches(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "a.txt"), []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &GrepTool{}
	result, err := tool.Run(context.Background(), fmt.Sprintf(`{"pattern":"hello","path":%q}`, tmp))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}
	if !strings.Contains(result.Content, "hello world") {
		t.Errorf("expected match line, got: %s", result.Content)
	}
}

func TestGrep_NoMatchesReturnsSuccess(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &GrepTool{}
	result, err := tool.Run(context.Background(), fmt.Sprintf(`{"pattern":"goodbye","path":%q}`, tmp))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success for no matches, got error: %s", result.Content)
	}
	if !strings.Contains(strings.ToLower(result.Content), "no matches") {
		t.Errorf("expected 'no matches' message, got: %s", result.Content)
	}
}

// TestGrep_GlobalLineCap: when a single search produces more than max_results
// total lines across many files, the result must be globally capped (not
// per-file, which is what rg's --max-count does). Creates a directory of 50
// files each containing 10 matches (500 total) and asks for max_results=20.
func TestGrep_GlobalLineCap(t *testing.T) {
	tmp := t.TempDir()
	for i := 0; i < 50; i++ {
		var sb strings.Builder
		for j := 0; j < 10; j++ {
			sb.WriteString(fmt.Sprintf("needle line %d in file %d\n", j, i))
		}
		path := filepath.Join(tmp, fmt.Sprintf("file%02d.txt", i))
		if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tool := &GrepTool{}
	result, err := tool.Run(context.Background(),
		fmt.Sprintf(`{"pattern":"needle","path":%q,"max_results":20}`, tmp))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}

	lines := strings.Split(strings.TrimSpace(result.Content), "\n")
	matchLines := 0
	hasTruncation := false
	for _, l := range lines {
		if strings.Contains(strings.ToLower(l), "truncated") {
			hasTruncation = true
			continue
		}
		if strings.Contains(l, "needle") {
			matchLines++
		}
	}
	if matchLines > 20 {
		t.Errorf("expected <= 20 match lines, got %d (cap is not global)", matchLines)
	}
	if !hasTruncation {
		t.Errorf("expected truncation notice, got: %s", result.Content)
	}
}

func TestGrep_RelativePathRefusedWithoutSessionCWD(t *testing.T) {
	tool := &GrepTool{}
	result, err := tool.Run(context.Background(), `{"pattern":"anything"}`)
	if err != nil {
		t.Fatalf("Run should not return a transport error, got %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected error result when session CWD unset, got: %s", result.Content)
	}
	if !strings.Contains(strings.ToLower(result.Content), "session working directory") &&
		!strings.Contains(strings.ToLower(result.Content), "absolute path") {
		t.Errorf("expected guard message, got: %s", result.Content)
	}
}

func TestGrep_RelativePathWorksWithSessionCWD(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "a.txt"), []byte("findme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := cwdctx.WithSessionCWD(context.Background(), tmp)
	tool := &GrepTool{}
	result, err := tool.Run(ctx, `{"pattern":"findme"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}
	if !strings.Contains(result.Content, "findme") {
		t.Errorf("expected match, got: %s", result.Content)
	}
}

func TestGrep_ContextCancellation(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tool := &GrepTool{}
	_, _ = tool.Run(ctx, fmt.Sprintf(`{"pattern":"x","path":%q}`, tmp))
	// Must not hang.
}
