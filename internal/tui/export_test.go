package tui

import (
	"strings"
	"testing"
)

// TestFormatTranscript: the exported transcript has a title heading and one
// labeled section per non-empty turn (You / Kocoro), blanks skipped.
func TestFormatTranscript(t *testing.T) {
	out := formatTranscript("My Chat", []transcriptEntry{
		{role: "user", text: "hello"},
		{role: "assistant", text: "hi there"},
		{role: "user", text: "   "}, // blank → skipped
	})
	if !strings.Contains(out, "# My Chat") {
		t.Error("missing title heading")
	}
	if !strings.Contains(out, "## You") || !strings.Contains(out, "hello") {
		t.Error("user turn should be labeled 'You' with its text")
	}
	if !strings.Contains(out, "## Kocoro") || !strings.Contains(out, "hi there") {
		t.Error("assistant turn should be labeled 'Kocoro'")
	}
	if n := strings.Count(out, "## "); n != 2 {
		t.Errorf("blank message must be skipped; got %d turn headings", n)
	}
}

// TestExportSlug: titles become filesystem-safe slugs; empty falls back.
func TestExportSlug(t *testing.T) {
	cases := map[string]string{
		"My Chat: Hello!": "my-chat-hello",
		"   ":             "session",
		"go-expert run":   "go-expert-run",
	}
	for in, want := range cases {
		if got := exportSlug(in); got != want {
			t.Errorf("exportSlug(%q) = %q, want %q", in, got, want)
		}
	}
}
