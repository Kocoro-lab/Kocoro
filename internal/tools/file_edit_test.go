package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

func TestFileEdit_RejectsUnreadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("hello world"), 0644)

	// Create a context with a ReadTracker that has NOT read this file
	tracker := agent.NewReadTracker()
	ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)

	tool := &FileEditTool{}
	args, _ := json.Marshal(fileEditArgs{
		Path:      path,
		OldString: "hello",
		NewString: "goodbye",
	})

	result, err := tool.Run(ctx, string(args))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result when file not read first")
	}
	if !contains(result.Content, "file_read") {
		t.Errorf("error message should mention file_read, got: %s", result.Content)
	}

	// Verify the file was NOT modified
	data, _ := os.ReadFile(path)
	if string(data) != "hello world" {
		t.Error("file should not have been modified")
	}
}

func TestFileEdit_AllowsReadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("hello world"), 0644)

	// Create a context with a ReadTracker that HAS read this file
	tracker := agent.NewReadTracker()
	tracker.MarkRead(path)
	ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)

	tool := &FileEditTool{}
	args, _ := json.Marshal(fileEditArgs{
		Path:      path,
		OldString: "hello",
		NewString: "goodbye",
	})

	result, err := tool.Run(ctx, string(args))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.Content)
	}

	data, _ := os.ReadFile(path)
	if string(data) != "goodbye world" {
		t.Errorf("expected 'goodbye world', got: %s", string(data))
	}
}

func TestFileEdit_NoTrackerInContext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("hello world"), 0644)

	// No tracker in context (e.g., tool called outside agent loop) - should allow
	tool := &FileEditTool{}
	args, _ := json.Marshal(fileEditArgs{
		Path:      path,
		OldString: "hello",
		NewString: "goodbye",
	})

	result, err := tool.Run(context.Background(), string(args))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success without tracker, got error: %s", result.Content)
	}
}

// TestFileEdit_ReplaceAll covers replace_all true/false × single/multiple/zero
// matches. The default (false) keeps the existing uniqueness guard; passing
// true allows rename / refactor in one call.
func TestFileEdit_ReplaceAll(t *testing.T) {
	cases := []struct {
		name       string
		content    string
		oldStr     string
		replaceAll bool
		wantErr    bool
		wantFile   string // expected file content after edit (only checked when !wantErr)
		wantInMsg  string // substring expected in result.Content
	}{
		{
			name:       "default_single_match_succeeds",
			content:    "foo bar baz",
			oldStr:     "bar",
			replaceAll: false,
			wantErr:    false,
			wantFile:   "foo X baz",
			wantInMsg:  "1 occurrence",
		},
		{
			name:       "default_multiple_matches_errors",
			content:    "foo bar foo bar",
			oldStr:     "bar",
			replaceAll: false,
			wantErr:    true,
			wantInMsg:  "2 times",
		},
		{
			name:       "default_zero_matches_errors",
			content:    "foo baz",
			oldStr:     "bar",
			replaceAll: false,
			wantErr:    true,
			wantInMsg:  "not found",
		},
		{
			name:       "replace_all_single_match_succeeds",
			content:    "foo bar baz",
			oldStr:     "bar",
			replaceAll: true,
			wantErr:    false,
			wantFile:   "foo X baz",
			wantInMsg:  "1 occurrence",
		},
		{
			name:       "replace_all_multiple_matches_succeeds",
			content:    "foo bar foo bar foo bar",
			oldStr:     "bar",
			replaceAll: true,
			wantErr:    false,
			wantFile:   "foo X foo X foo X",
			wantInMsg:  "3 occurrences",
		},
		{
			name:       "replace_all_zero_matches_errors",
			content:    "foo baz",
			oldStr:     "bar",
			replaceAll: true,
			wantErr:    true,
			wantInMsg:  "not found",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "test.txt")
			os.WriteFile(path, []byte(tc.content), 0644)

			tracker := agent.NewReadTracker()
			tracker.MarkRead(path)
			ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)

			args, _ := json.Marshal(fileEditArgs{
				Path:       path,
				OldString:  tc.oldStr,
				NewString:  "X",
				ReplaceAll: tc.replaceAll,
			})

			tool := &FileEditTool{}
			result, err := tool.Run(ctx, string(args))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr {
				if !result.IsError {
					t.Fatalf("expected error, got success: %s", result.Content)
				}
				if !contains(result.Content, tc.wantInMsg) {
					t.Errorf("error message missing %q, got: %s", tc.wantInMsg, result.Content)
				}
				// Verify file unchanged on error
				data, _ := os.ReadFile(path)
				if string(data) != tc.content {
					t.Errorf("file should be unchanged on error, got: %s", string(data))
				}
				return
			}
			if result.IsError {
				t.Fatalf("expected success, got error: %s", result.Content)
			}
			if !contains(result.Content, tc.wantInMsg) {
				t.Errorf("success message missing %q, got: %s", tc.wantInMsg, result.Content)
			}
			data, _ := os.ReadFile(path)
			if string(data) != tc.wantFile {
				t.Errorf("file content mismatch:\n  want: %q\n  got:  %q", tc.wantFile, string(data))
			}
		})
	}
}

// TestFileEdit_ReplaceAllDefaultsFalse confirms omitting replace_all in JSON
// keeps the uniqueness guard (must zero out without explicit field).
func TestFileEdit_ReplaceAllDefaultsFalse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("foo bar foo bar"), 0644)

	tracker := agent.NewReadTracker()
	tracker.MarkRead(path)
	ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)

	tool := &FileEditTool{}
	// JSON without replace_all field — should default to false (uniqueness guard active)
	result, err := tool.Run(ctx, `{"path":"`+path+`","old_string":"bar","new_string":"X"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected error (multiple matches with default), got success: %s", result.Content)
	}
}
