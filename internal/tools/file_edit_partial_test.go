package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// TestFileEdit_FuzzyReplaceAll_NoPartialWrite reproduces the reviewer's
// correctness finding: a fuzzy replace_all where the matches have DIFFERENT
// raw bytes replaces only the bytes equal to the first match, yet reports
// "N occurrences" — a partial write reported as full success. Two "key\nval"
// blocks (CRLF) with different trailing whitespace force the fuzzy rstrip path
// and give the two matches different raw bytes.
func TestFileEdit_FuzzyReplaceAll_NoPartialWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	original := "key\r\nval  \r\nkey\r\nval\t\r\n" // two blocks, different trailing ws
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	tracker := agent.NewReadTracker()
	tracker.MarkRead(path)
	ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)

	tool := &FileEditTool{}
	args, _ := json.Marshal(fileEditArgs{
		Path: path, OldString: "key\nval", NewString: "DONE", ReplaceAll: true, Description: "x",
	})
	r, err := tool.Run(ctx, string(args))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if r.IsError {
		t.Fatalf("expected span-based replace_all to succeed, got: %s", r.Content)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "DONE\r\nDONE\r\n"; string(after) != want {
		t.Errorf("fuzzy replace_all must replace every matched span:\n  want: %q\n  got:  %q", want, string(after))
	}
	if !contains(r.Content, "2 occurrences") {
		t.Errorf("success result should report both replacements, got: %s", r.Content)
	}
}

// TestFileEdit_FuzzyLineMatch_UsesMatchedSpan covers an earlier byte-identical
// substring embedded inside a larger line. The line matcher selects only the
// standalone second line; replacing by raw substring would incorrectly edit the
// earlier embedded text (and replace_all would edit both).
func TestFileEdit_FuzzyLineMatch_UsesMatchedSpan(t *testing.T) {
	for _, replaceAll := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "replace_all"}[replaceAll], func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "f.txt")
			original := "prefix\t\treturn 1 suffix\n\t\treturn 1\n"
			if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
				t.Fatal(err)
			}
			tracker := agent.NewReadTracker()
			tracker.MarkRead(path)
			ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)

			args, err := json.Marshal(fileEditArgs{
				Path: path, OldString: "        return 1", NewString: "\t\treturn 2",
				ReplaceAll: replaceAll, Description: "update the standalone return",
			})
			if err != nil {
				t.Fatal(err)
			}
			r, err := (&FileEditTool{}).Run(ctx, string(args))
			if err != nil {
				t.Fatalf("transport error: %v", err)
			}
			if r.IsError {
				t.Fatalf("expected fuzzy edit to succeed, got: %s", r.Content)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want := "prefix\t\treturn 1 suffix\n\t\treturn 2\n"
			if string(after) != want {
				t.Errorf("edit must use the matched line span:\n  want: %q\n  got:  %q", want, string(after))
			}
		})
	}
}

// TestFileEdit_NotFound_SkipsDiagnosticsOnOverlongLine reproduces the reviewer's
// unbounded-cost finding: diagnoseNoMatch runs an O(len(oldFirstLine) * lineLen)
// longest-common-substring scan per file line. With a 600-rune (over-cap) first
// line that is highly similar to the file, pre-fix this ran the expensive LCS
// and produced a diagnostic; the cost is unbounded for minified / multi-KB
// lines. The fix must skip diagnostics when the first line exceeds the cap.
func TestFileEdit_NotFound_SkipsDiagnosticsOnOverlongLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	fileLine := strings.Repeat("a", 600)
	if err := os.WriteFile(path, []byte(fileLine+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tracker := agent.NewReadTracker()
	tracker.MarkRead(path)
	ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)

	tool := &FileEditTool{}
	oldStr := strings.Repeat("a", 599) + "Z" // 600 runes, differs at end → not found
	args, _ := json.Marshal(fileEditArgs{Path: path, OldString: oldStr, NewString: "y", Description: "x"})
	r, err := tool.Run(ctx, string(args))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !r.IsError {
		t.Fatalf("expected not-found error, got: %s", r.Content)
	}
	if contains(r.Content, "Closest match") {
		t.Errorf("diagnostics must be skipped for an over-long old_string first line (unbounded LCS cost)")
	}
}

func TestDiagnoseNoMatch_EnforcesTotalWorkBudget(t *testing.T) {
	line := strings.Repeat("a", diagMaxLineRunes-1) + "x\n"
	content := strings.Repeat(line, 10)
	old := strings.Repeat("a", diagMaxLineRunes-1) + "z"
	if hint := diagnoseNoMatch(content, old); hint != "" {
		t.Fatalf("diagnostic should skip work exceeding its total comparison budget, got: %s", hint)
	}
}

func TestFileEdit_NotFound_TruncatesWhitespacePaddedDiagnostic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	fileLine := strings.Repeat(" ", 10_000) + "targetValue = 42"
	if err := os.WriteFile(path, []byte(fileLine+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tracker := agent.NewReadTracker()
	tracker.MarkRead(path)
	ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)

	args, err := json.Marshal(fileEditArgs{
		Path: path, OldString: "targetValue = 99", NewString: "x", Description: "update target value",
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := (&FileEditTool{}).Run(ctx, string(args))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !r.IsError {
		t.Fatalf("expected not-found error, got: %s", r.Content)
	}
	if !contains(r.Content, "[truncated]") {
		t.Fatalf("expected bounded diagnostic output, got: %s", r.Content)
	}
	if len(r.Content) > 2000 {
		t.Fatalf("diagnostic output is unexpectedly large: %d bytes", len(r.Content))
	}
}
