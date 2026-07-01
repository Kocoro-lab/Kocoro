package daemon

import (
	"strings"
	"testing"
)

func TestMakeSpokenSummaryKeepsShortReply(t *testing.T) {
	reply := "Done. I saved the report."
	if got := makeSpokenSummary(reply); got != reply {
		t.Fatalf("summary = %q, want %q", got, reply)
	}
}

func TestMakeSpokenSummaryPrefersSentenceBoundary(t *testing.T) {
	reply := strings.Repeat("First sentence has enough context. ", 20)
	got := makeSpokenSummary(reply)
	if len([]rune(got)) > spokenSummaryMaxRunes+3 {
		t.Fatalf("summary too long: %d runes", len([]rune(got)))
	}
	if !strings.HasSuffix(got, ".") {
		t.Fatalf("summary should end on a sentence boundary, got %q", got)
	}
}

func TestMakeSpokenSummaryStripsMarkdownAndListNoise(t *testing.T) {
	// Result-last convention: the fallback surfaces the concluding tail line, with
	// markdown/URL/list noise stripped — never a head progress line.
	reply := "I checked your inbox:\n\n" +
		"1. **Acme** — [Receipt](https://example.com/r)\n" +
		"2. `GitHub` — Build failed https://example.com/build\n\n" +
		"You have **three** new emails."

	got := makeSpokenSummary(reply)
	for _, bad := range []string{"**", "`", "https://", "[Receipt]"} {
		if strings.Contains(got, bad) {
			t.Fatalf("summary contains voice-hostile marker %q: %q", bad, got)
		}
	}
	if !strings.Contains(got, "three new emails") {
		t.Fatalf("summary missing the tail conclusion: %q", got)
	}
}

func TestMakeSpokenSummarySkipsCodeBlocks(t *testing.T) {
	reply := "I saved the report.\n\n```json\n{\"secret\":\"do not read\"}\n```\n\nYou can open it from Kocoro Desktop."
	got := makeSpokenSummary(reply)
	if strings.Contains(got, "secret") || strings.Contains(got, "json") {
		t.Fatalf("summary should skip fenced code blocks, got %q", got)
	}
	if !strings.Contains(got, "open it") {
		t.Fatalf("summary missing the tail line, got %q", got)
	}
}
