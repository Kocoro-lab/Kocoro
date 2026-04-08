package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGlob_BasicPattern(t *testing.T) {
	tmp := t.TempDir()
	for _, name := range []string{"a.go", "b.go", "c.md"} {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tool := &GlobTool{}
	result, err := tool.Run(context.Background(), fmt.Sprintf(`{"pattern":"*.go","path":%q}`, tmp))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}
	lines := strings.Split(strings.TrimSpace(result.Content), "\n")
	if len(lines) != 2 {
		t.Errorf("expected 2 matches, got %d: %s", len(lines), result.Content)
	}
}

func TestGlob_RecursivePattern(t *testing.T) {
	tmp := t.TempDir()
	sub := filepath.Join(tmp, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(tmp, "top.go"), filepath.Join(sub, "nested.go")} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tool := &GlobTool{}
	result, err := tool.Run(context.Background(), fmt.Sprintf(`{"pattern":"**/*.go","path":%q}`, tmp))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}
	lines := strings.Split(strings.TrimSpace(result.Content), "\n")
	if len(lines) != 2 {
		t.Errorf("expected 2 matches, got %d: %s", len(lines), result.Content)
	}
}

func TestGlob_NoMatches(t *testing.T) {
	tmp := t.TempDir()

	tool := &GlobTool{}
	result, err := tool.Run(context.Background(), fmt.Sprintf(`{"pattern":"*.xyz","path":%q}`, tmp))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}
	if result.Content != "no files matched" {
		t.Errorf("expected 'no files matched', got: %s", result.Content)
	}
}

func TestGlob_MaxResults(t *testing.T) {
	tmp := t.TempDir()
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("file%02d.txt", i)
		if err := os.WriteFile(filepath.Join(tmp, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tool := &GlobTool{}
	result, err := tool.Run(context.Background(), fmt.Sprintf(`{"pattern":"*.txt","path":%q,"max_results":3}`, tmp))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}
	// Should have 3 file lines + 1 truncation notice line
	lines := strings.Split(strings.TrimSpace(result.Content), "\n")
	fileLines := 0
	hasTruncation := false
	for _, l := range lines {
		if strings.Contains(l, "results truncated") || strings.Contains(l, "more") {
			hasTruncation = true
		} else if l != "" {
			fileLines++
		}
	}
	if fileLines != 3 {
		t.Errorf("expected 3 file lines, got %d: %s", fileLines, result.Content)
	}
	if !hasTruncation {
		t.Errorf("expected truncation notice, got: %s", result.Content)
	}
}

func TestGlob_ContextCancellation(t *testing.T) {
	tmp := t.TempDir()
	// Create some files so there's something to potentially match
	for i := 0; i < 5; i++ {
		if err := os.WriteFile(filepath.Join(tmp, fmt.Sprintf("f%d.go", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	tool := &GlobTool{}
	result, err := tool.Run(ctx, fmt.Sprintf(`{"pattern":"**/*.go","path":%q}`, tmp))
	// Must not hang; either returns error result or an error
	if err != nil {
		return // acceptable
	}
	if !result.IsError {
		// Also acceptable if it completed fast enough before cancel was noticed
		// Just ensure it didn't hang (test will timeout if it does)
		return
	}
}

func TestGlob_GitignoreRespected(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not available; gitignore test requires rg")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	tmp := t.TempDir()

	// git init
	if err := exec.Command("git", "init", tmp).Run(); err != nil {
		t.Fatalf("git init failed: %v", err)
	}

	// Create .gitignore
	if err := os.WriteFile(filepath.Join(tmp, ".gitignore"), []byte("ignored/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create visible file
	if err := os.WriteFile(filepath.Join(tmp, "visible.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create ignored dir + file
	ignoredDir := filepath.Join(tmp, "ignored")
	if err := os.MkdirAll(ignoredDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ignoredDir, "secret.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &GlobTool{}
	result, err := tool.Run(context.Background(), fmt.Sprintf(`{"pattern":"**/*.go","path":%q}`, tmp))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}
	if strings.Contains(result.Content, "secret.go") {
		t.Errorf("gitignored file should not appear in results, got: %s", result.Content)
	}
	if !strings.Contains(result.Content, "visible.go") {
		t.Errorf("visible.go should appear in results, got: %s", result.Content)
	}
}

func TestSplitAbsPattern(t *testing.T) {
	tests := []struct {
		pattern  string
		wantRoot string
		wantRel  string
	}{
		{"/a/b/c/{*.md,*.go}", "/a/b/c", "{*.md,*.go}"},
		{"/a/b/*/README.md", "/a/b", "*/README.md"},
		{"/a/b/**/*.go", "/a/b", "**/*.go"},
		{"/a/b/c/file.txt", "/a/b/c", "file.txt"},
		{"/a/b/c/d[0-9].txt", "/a/b/c", "d[0-9].txt"},
		{"/a/b/c/d?.txt", "/a/b/c", "d?.txt"},
	}
	for _, tt := range tests {
		root, rel := splitAbsPattern(tt.pattern)
		if root != tt.wantRoot || rel != tt.wantRel {
			t.Errorf("splitAbsPattern(%q) = (%q, %q), want (%q, %q)",
				tt.pattern, root, rel, tt.wantRoot, tt.wantRel)
		}
	}
}

func TestGlob_AbsolutePathPattern(t *testing.T) {
	tmp := t.TempDir()
	sub := filepath.Join(tmp, "repo")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(sub, "README.md"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(sub, "main.go"), []byte("package main"), 0o644)

	// Pattern with absolute path embedded — should still find files
	pattern := filepath.Join(sub, "*.md")
	tool := &GlobTool{}
	argsJSON := fmt.Sprintf(`{"pattern":%q}`, pattern)
	result, err := tool.Run(context.Background(), argsJSON)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}
	if !strings.Contains(result.Content, "README.md") {
		t.Errorf("expected README.md in results, got: %s", result.Content)
	}
	if strings.Contains(result.Content, "main.go") {
		t.Errorf("main.go should not match *.md pattern, got: %s", result.Content)
	}
}

func TestGlob_AbsolutePathWithBraces(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not available")
	}
	tmp := t.TempDir()
	sub := filepath.Join(tmp, "project")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(sub, "README.md"), []byte("readme"), 0o644)
	os.WriteFile(filepath.Join(sub, "go.mod"), []byte("module test"), 0o644)
	os.WriteFile(filepath.Join(sub, "secret.key"), []byte("x"), 0o644)

	// Brace expansion with absolute path — the exact pattern that fails without the fix
	pattern := filepath.Join(sub, "{README*,*.mod}")
	tool := &GlobTool{}
	argsJSON := fmt.Sprintf(`{"pattern":%q}`, pattern)
	result, err := tool.Run(context.Background(), argsJSON)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}
	if !strings.Contains(result.Content, "README.md") {
		t.Errorf("expected README.md, got: %s", result.Content)
	}
	if !strings.Contains(result.Content, "go.mod") {
		t.Errorf("expected go.mod, got: %s", result.Content)
	}
	if strings.Contains(result.Content, "secret.key") {
		t.Errorf("secret.key should not match, got: %s", result.Content)
	}
}
