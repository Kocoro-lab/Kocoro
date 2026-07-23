package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
)

// editCtx returns a context with a read tracker that has already marked path as
// read, so file_edit's read-before-edit guard is satisfied.
func editCtx(path string) context.Context {
	tracker := agent.NewReadTracker()
	tracker.MarkRead(path)
	return context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)
}

// Fix 1a: new_string:"" is a legal deletion request, not a missing field.
func TestFileEdit_EmptyNewStringDeletes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("foo bar baz"), 0644); err != nil {
		t.Fatal(err)
	}
	tool := &FileEditTool{}
	result, err := tool.Run(editCtx(path),
		`{"path":"`+path+`","old_string":"bar ","new_string":"","description":"delete text"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("empty new_string should delete, got error: %s", result.Content)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "foo baz" {
		t.Fatalf("expected %q after deletion, got %q", "foo baz", string(data))
	}
}

// Fix 1b: whitespace-only old_string is legitimate content (collapse blank lines).
func TestFileEdit_WhitespaceOldStringAccepted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("a\n\nb"), 0644); err != nil {
		t.Fatal(err)
	}
	tool := &FileEditTool{}
	result, err := tool.Run(editCtx(path),
		`{"path":"`+path+`","old_string":"\n\n","new_string":"\n","description":"collapse blank line"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("whitespace old_string should match, got error: %s", result.Content)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "a\nb" {
		t.Fatalf("expected %q, got %q", "a\nb", string(data))
	}
}

// Fix 1c: missing old_string still returns a [validation error] (guard).
func TestFileEdit_MissingOldStringValidationError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("foo"), 0644); err != nil {
		t.Fatal(err)
	}
	tool := &FileEditTool{}
	result, _ := tool.Run(editCtx(path),
		`{"path":"`+path+`","new_string":"X","description":"d"}`)
	if !result.IsError || !strings.HasPrefix(result.Content, "[validation error]") {
		t.Fatalf("missing old_string should be [validation error], got: %s", result.Content)
	}
	if !strings.Contains(result.Content, "old_string") {
		t.Fatalf("error should name old_string, got: %s", result.Content)
	}
}

// new_string remains required even though an explicitly empty value is legal.
// This prevents an omitted model argument from becoming a destructive delete.
func TestFileEdit_NewStringRequired(t *testing.T) {
	info := (&FileEditTool{}).Info()
	if !containsString(info.Required, "new_string") {
		t.Fatalf("new_string must be in Required, got %v", info.Required)
	}
}

func TestFileEdit_MissingNewStringRejectedWithoutMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("keep me"), 0644); err != nil {
		t.Fatal(err)
	}
	result, err := (&FileEditTool{}).Run(
		editCtx(path),
		`{"path":"`+path+`","old_string":"keep me","description":"replace text"}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.HasPrefix(result.Content, "[validation error]") {
		t.Fatalf("missing new_string must be a validation error, got %q", result.Content)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "keep me" {
		t.Fatalf("missing new_string mutated file to %q", string(data))
	}
}

// Fix 2: whitespace-only content is a legitimate write (e.g. a trailing newline).
func TestFileWrite_WhitespaceContentAccepted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	tool := &FileWriteTool{}
	result, err := tool.Run(context.Background(),
		`{"path":"`+path+`","content":"\n","description":"write newline"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("whitespace content should write, got error: %s", result.Content)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "\n" {
		t.Fatalf("expected newline written, got %q", string(data))
	}
}

// Fix 3: wait_for rejects whitespace-only condition, consistent with the sweep.
func TestWaitFor_WhitespaceConditionRejected(t *testing.T) {
	tool := &WaitTool{}
	result, _ := tool.Run(context.Background(), `{"condition":"   "}`)
	if !result.IsError || !strings.HasPrefix(result.Content, "[validation error]") {
		t.Fatalf("whitespace condition should be [validation error], got: %s", result.Content)
	}
	if !strings.Contains(result.Content, "condition") {
		t.Fatalf("error should name condition, got: %s", result.Content)
	}
}

// Fix 4: edit_image missing image_urls keeps the actionable publish_to_web hint.
func TestEditImage_MissingImageURLsGuidance(t *testing.T) {
	tool := &EditImageTool{}
	result, _ := tool.Run(context.Background(), `{"prompt":"make it blue","description":"d"}`)
	if !result.IsError || !strings.HasPrefix(result.Content, "[validation error]") {
		t.Fatalf("missing image_urls should be [validation error], got: %s", result.Content)
	}
	if !strings.Contains(result.Content, "publish_to_web") ||
		!strings.Contains(result.Content, "static.kocoro.ai") {
		t.Fatalf("missing image_urls lost its actionable hint: %s", result.Content)
	}
}

// Fix 4: retract missing id keeps the "use the id from list_my_published_files" hint.
func TestRetract_MissingIDGuidance(t *testing.T) {
	tool := &RetractPublishedFileTool{}
	result, _ := tool.Run(context.Background(), `{"description":"d"}`)
	if !result.IsError || !strings.HasPrefix(result.Content, "[validation error]") {
		t.Fatalf("missing id should be [validation error], got: %s", result.Content)
	}
	if !strings.Contains(result.Content, "list_my_published_files") {
		t.Fatalf("missing id lost its actionable hint: %s", result.Content)
	}
}

// A read can still require approval for sensitive or out-of-scope paths, so a
// missing description must fail before the approval card is rendered.
func TestFileRead_NoDescriptionRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("hello\nworld\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ctx := cwdctx.WithSessionCWD(context.Background(), dir)
	tool := &FileReadTool{}
	result, err := tool.Run(ctx, `{"path":"`+path+`"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError || !strings.HasPrefix(result.Content, "[validation error]") {
		t.Fatalf("file_read without description should fail validation, got: %s", result.Content)
	}
}

func TestReadOnlyTools_DescriptionRequiredForPossibleApproval(t *testing.T) {
	for _, tool := range []agent.Tool{
		&FileReadTool{}, &GrepTool{}, &GlobTool{}, &DirectoryListTool{},
	} {
		info := tool.Info()
		if !containsString(info.Required, "description") {
			t.Errorf("%s: description must be Required, got %v", info.Name, info.Required)
		}
	}
}
