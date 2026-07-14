package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// TestFileEdit_FuzzyMatch_SmartQuotes: the file uses Unicode smart quotes
// (U+201C/U+201D) but old_string is typed with ASCII straight quotes — the
// single most common file_edit failure. The edit must still locate the target
// by normalizing typographic punctuation, and replace the real file bytes.
func TestFileEdit_FuzzyMatch_SmartQuotes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.md")
	original := "intro\n“value”: “hello”\noutro\n" // smart quotes in file
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	tracker := agent.NewReadTracker()
	tracker.MarkRead(path)
	ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)

	tool := &FileEditTool{}
	args, _ := json.Marshal(fileEditArgs{
		Path:        path,
		OldString:   `"value": "hello"`, // ASCII straight quotes
		NewString:   `"value": "world"`,
		Description: "change hello to world",
	})

	result, err := tool.Run(ctx, string(args))
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected fuzzy match to succeed on smart-quote mismatch, got error: %s", result.Content)
	}

	got, _ := os.ReadFile(path)
	want := "intro\n\"value\": \"world\"\noutro\n"
	if string(got) != want {
		t.Errorf("file content mismatch:\n  want: %q\n  got:  %q", want, string(got))
	}
}

// TestFileEdit_FuzzyMatch_Punctuation: dashes and exotic spaces are the other
// half of the punctuation-normalization mechanism. File contains typographic
// chars; old_string is typed in plain ASCII.
func TestFileEdit_FuzzyMatch_Punctuation(t *testing.T) {
	cases := []struct {
		name     string
		fileBody string // the middle line as it appears in the file
		oldStr   string // ASCII form the model types
	}{
		{"em_dash", "range: 1—10", "range: 1-10"}, // U+2014 em dash
		{"en_dash", "range: 1–10", "range: 1-10"}, // U+2013 en dash
		{"nbsp", "key: value", "key: value"},      // U+00A0 non-breaking space
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "f.txt")
			original := "head\n" + tc.fileBody + "\ntail\n"
			os.WriteFile(path, []byte(original), 0o644)

			tracker := agent.NewReadTracker()
			tracker.MarkRead(path)
			ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)

			tool := &FileEditTool{}
			args, _ := json.Marshal(fileEditArgs{
				Path: path, OldString: tc.oldStr, NewString: "REPLACED", Description: "replace punctuation variant",
			})
			result, err := tool.Run(ctx, string(args))
			if err != nil {
				t.Fatalf("transport error: %v", err)
			}
			if result.IsError {
				t.Fatalf("expected fuzzy match to succeed, got error: %s", result.Content)
			}
			got, _ := os.ReadFile(path)
			want := "head\nREPLACED\ntail\n"
			if string(got) != want {
				t.Errorf("want %q got %q", want, string(got))
			}
		})
	}
}

func TestFileEdit_FuzzyReplaceAll_MixedSmartQuoteBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "quotes.txt")
	original := "“x”\n„x“\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	tracker := agent.NewReadTracker()
	tracker.MarkRead(path)
	ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)
	args, err := json.Marshal(fileEditArgs{
		Path: path, OldString: `"x"`, NewString: "X", ReplaceAll: true,
		Description: "replace both smart-quote variants",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&FileEditTool{}).Run(ctx, string(args))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected mixed-byte fuzzy replace_all to succeed, got: %s", result.Content)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "X\nX\n"; string(got) != want {
		t.Errorf("fuzzy spans must replace both quote variants:\n  want: %q\n  got:  %q", want, string(got))
	}
}
