package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// TestFileEdit_FuzzyMatch_Whitespace covers line-level whitespace tolerance
// where old_string is NOT a substring of the file (so exact/punct matching
// genuinely fails): CRLF-vs-LF and tab-vs-space indentation, absorbed by the
// rstrip/trim passes. Matching is
// line-oriented, so the matched line block is replaced by new_string and the
// block's own line terminators are preserved around it.
func TestFileEdit_FuzzyMatch_Whitespace(t *testing.T) {
	cases := []struct {
		name     string
		original string // exact file bytes on disk
		oldStr   string // what the model types — deliberately NOT a substring
		newStr   string
		want     string // exact file bytes after the edit
	}{
		{
			// File uses CRLF; model types a two-line block with LF. "alpha\nbeta"
			// is not a substring of "...alpha\r\nbeta..." → exact + punct fail,
			// rstrip pass matches. (rstrip)
			name:     "crlf_vs_lf",
			original: "head\r\nalpha\r\nbeta\r\ntail\r\n",
			oldStr:   "alpha\nbeta",
			newStr:   "MERGED",
			want:     "head\r\nMERGED\r\ntail\r\n",
		},
		{
			// File indents with tabs; model types spaces. "        return 1"
			// (8 spaces) is not a substring of "\t\treturn 1" → rstrip fails,
			// trim (leading whitespace) pass matches. (trim)
			name:     "indent_tab_vs_spaces",
			original: "func f() {\n\t\treturn 1\n}\n",
			oldStr:   "        return 1",
			newStr:   "\t\treturn 2",
			want:     "func f() {\n\t\treturn 2\n}\n",
		},
		{
			// A trailing newline is part of old_string. strings.Split would create
			// a phantom empty line and prevent the CRLF fallback from matching.
			name:     "crlf_vs_lf_with_trailing_newline",
			original: "head\r\nalpha\r\nbeta\r\ntail\r\n",
			oldStr:   "alpha\nbeta\n",
			newStr:   "MERGED\r\n",
			want:     "head\r\nMERGED\r\ntail\r\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "f.txt")
			if err := os.WriteFile(path, []byte(tc.original), 0o644); err != nil {
				t.Fatal(err)
			}

			tracker := agent.NewReadTracker()
			tracker.MarkRead(path)
			ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)

			tool := &FileEditTool{}
			args, _ := json.Marshal(fileEditArgs{
				Path: path, OldString: tc.oldStr, NewString: tc.newStr, Description: "replace fuzzy block",
			})
			result, err := tool.Run(ctx, string(args))
			if err != nil {
				t.Fatalf("transport error: %v", err)
			}
			if result.IsError {
				t.Fatalf("expected whitespace-tolerant match to succeed, got error: %s", result.Content)
			}
			got, _ := os.ReadFile(path)
			if string(got) != tc.want {
				t.Errorf("file bytes mismatch:\n  want: %q\n  got:  %q", tc.want, string(got))
			}
		})
	}
}

func TestFileEdit_FuzzyMatch_TrailingNewlineRequiresTerminator(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	original := "alpha"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	tracker := agent.NewReadTracker()
	tracker.MarkRead(path)
	ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)
	args, err := json.Marshal(fileEditArgs{
		Path: path, OldString: "alpha\n", NewString: "beta\n", Description: "replace terminated line",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&FileEditTool{}).Run(ctx, string(args))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("old_string with a trailing newline must not match an unterminated file line")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("failed edit changed file: want %q, got %q", original, string(got))
	}
}
