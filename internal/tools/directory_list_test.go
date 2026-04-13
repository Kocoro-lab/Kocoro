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

func TestDirectoryList_Absolute(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &DirectoryListTool{}
	result, err := tool.Run(context.Background(), fmt.Sprintf(`{"path":%q}`, tmp))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}
	if !strings.Contains(result.Content, "a.txt") {
		t.Errorf("expected a.txt in listing, got: %s", result.Content)
	}
}

func TestDirectoryList_RelativeRefusedWithoutSessionCWD(t *testing.T) {
	tool := &DirectoryListTool{}
	result, err := tool.Run(context.Background(), `{"path":"somedir"}`)
	if err != nil {
		t.Fatalf("Run should not return a transport error, got %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected error result when session CWD unset and path is relative, got: %s", result.Content)
	}
	if !strings.Contains(strings.ToLower(result.Content), "session working directory") &&
		!strings.Contains(strings.ToLower(result.Content), "absolute path") {
		t.Errorf("expected guard message, got: %s", result.Content)
	}
}

func TestDirectoryList_EmptyPathRefusedWithoutSessionCWD(t *testing.T) {
	tool := &DirectoryListTool{}
	result, err := tool.Run(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("Run should not return a transport error, got %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected error result when session CWD unset and path is empty, got: %s", result.Content)
	}
}

func TestDirectoryList_RelativeWorksWithSessionCWD(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "b.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := cwdctx.WithSessionCWD(context.Background(), tmp)
	tool := &DirectoryListTool{}
	result, err := tool.Run(ctx, `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}
	if !strings.Contains(result.Content, "b.txt") {
		t.Errorf("expected b.txt in listing, got: %s", result.Content)
	}
}
