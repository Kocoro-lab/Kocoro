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

func TestFileRead_ResolvesRelativePathFromSessionCWD(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte("hello"), 0644)

	ctx := cwdctx.WithSessionCWD(context.Background(), dir)
	tool := &FileReadTool{}
	result, err := tool.Run(ctx, `{"path":"test.txt","description":"test relative read"}`)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
	if result.Content == "" {
		t.Fatal("expected file content")
	}
}

func TestGlob_ResolvesRelativeRootFromSessionCWD(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0644)

	ctx := cwdctx.WithSessionCWD(context.Background(), dir)
	tool := &GlobTool{}
	result, err := tool.Run(ctx, `{"pattern":"*.go","description":"test relative glob"}`)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "main.go" {
		t.Fatalf("expected main.go, got %q", result.Content)
	}
}

func TestDirectoryList_ResolvesRelativePathFromSessionCWD(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0644)

	ctx := cwdctx.WithSessionCWD(context.Background(), dir)
	tool := &DirectoryListTool{}
	result, err := tool.Run(ctx, `{"description":"test relative list"}`)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || result.Content == "" {
		t.Fatalf("unexpected: error=%v content=%q", result.IsError, result.Content)
	}
}

func TestGrep_ResolvesRelativePathFromSessionCWD(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("findme here\n"), 0644)

	ctx := cwdctx.WithSessionCWD(context.Background(), dir)
	tool := &GrepTool{}
	result, err := tool.Run(ctx, `{"pattern":"findme","description":"test relative grep"}`)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
}

func TestFileRead_IsSafeArgsWithContext_UsesSessionCWD(t *testing.T) {
	dir := t.TempDir()
	ctx := cwdctx.WithSessionCWD(context.Background(), dir)

	tool := &FileReadTool{}
	// File under session CWD — should be safe
	args := fmt.Sprintf(`{"path":"%s/test.txt"}`, dir)
	if !tool.IsSafeArgsWithContext(ctx, args) {
		t.Error("file under session CWD should be safe")
	}
	// File outside session CWD — should NOT be safe
	if tool.IsSafeArgsWithContext(ctx, `{"path":"/etc/passwd"}`) {
		t.Error("file outside session CWD should not be safe")
	}
	// Relative path under session CWD — should be safe
	if !tool.IsSafeArgsWithContext(ctx, `{"path":"subdir/file.txt"}`) {
		t.Error("relative path under session CWD should be safe")
	}
}

func TestBash_UsesSessionCWDWhenNoCWDField(t *testing.T) {
	dir := t.TempDir()

	ctx := cwdctx.WithSessionCWD(context.Background(), dir)
	tool := &BashTool{}
	result, err := tool.Run(ctx, `{"command":"pwd","description":"test relative bash"}`)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
	got := strings.TrimSpace(result.Content)
	// On macOS, /tmp may resolve to /private/tmp
	resolved, _ := filepath.EvalSymlinks(dir)
	if got != dir && got != resolved {
		t.Fatalf("expected bash to run in %q, got %q", dir, got)
	}
}
