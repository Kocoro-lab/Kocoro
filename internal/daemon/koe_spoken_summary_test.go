package daemon

import (
	"strings"
	"testing"
)

func TestSpokenSummaryForKoeOnly(t *testing.T) {
	reply := strings.Repeat("This is a long result sentence. ", 20)
	if got := spokenSummaryForSource("desktop", reply); got != "" {
		t.Fatalf("desktop spoken summary = %q, want empty", got)
	}
	if got := spokenSummaryForSource("koe", reply); got == "" {
		t.Fatal("koe spoken summary is empty")
	}
}

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
	reply := "Today I found 3 emails:\n\n" +
		"1. **Acme** — [Receipt](https://example.com/r)\n" +
		"2. `GitHub` — Build failed https://example.com/build\n" +
		"3. Promo — Summer sale"

	got := makeSpokenSummary(reply)
	for _, bad := range []string{"1.", "**", "`", "https://"} {
		if strings.Contains(got, bad) {
			t.Fatalf("summary contains voice-hostile marker %q: %q", bad, got)
		}
	}
	for _, want := range []string{"Today I found 3 emails", "Acme", "GitHub"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q: %q", want, got)
		}
	}
}

func TestMakeSpokenSummarySkipsCodeBlocks(t *testing.T) {
	reply := "I saved the report.\n\n```json\n{\"secret\":\"do not read\"}\n```\n\nYou can open it from Desktop."
	got := makeSpokenSummary(reply)
	if strings.Contains(got, "secret") || strings.Contains(got, "json") {
		t.Fatalf("summary should skip fenced code blocks, got %q", got)
	}
	if !strings.Contains(got, "I saved the report") {
		t.Fatalf("summary missing spoken lead, got %q", got)
	}
}
