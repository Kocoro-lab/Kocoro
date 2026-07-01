package daemon

import (
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// The model authors <spoken_summary> at the END of its reply (result-last), so
// the parser takes the LAST well-formed block. Head placement still resolves
// (last == only occurrence), which keeps older replies working.

func TestSplitSpokenSummary_TailPlacement(t *testing.T) {
	reply := "Here is the detail:\n- Acme\n- GitHub\n<spoken_summary>You have three new emails.</spoken_summary>"
	spoken, rest, ok := splitSpokenSummary(reply)
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if spoken != "You have three new emails." {
		t.Fatalf("spoken = %q", spoken)
	}
	if strings.Contains(rest, "spoken_summary") || !strings.Contains(rest, "Here is the detail") {
		t.Fatalf("rest = %q", rest)
	}
}

func TestSplitSpokenSummary_HeadPlacementStillWorks(t *testing.T) {
	reply := "<spoken_summary>Done.</spoken_summary>\nbody text"
	spoken, rest, ok := splitSpokenSummary(reply)
	if !ok || spoken != "Done." || rest != "body text" {
		t.Fatalf("spoken=%q rest=%q ok=%v", spoken, rest, ok)
	}
}

func TestSplitSpokenSummary_NoTag(t *testing.T) {
	reply := "Just a normal reply with no tag."
	spoken, rest, ok := splitSpokenSummary(reply)
	if ok || spoken != "" || rest != reply {
		t.Fatalf("spoken=%q rest=%q ok=%v", spoken, rest, ok)
	}
}

func TestSplitSpokenSummary_UnclosedTag(t *testing.T) {
	reply := "detail here\n<spoken_summary>partial with no closing tag"
	spoken, rest, ok := splitSpokenSummary(reply)
	if ok {
		t.Fatalf("unclosed tag must not parse as ok; spoken=%q", spoken)
	}
	if rest != reply {
		t.Fatalf("unclosed tag should leave reply intact for the fallback to scrub; rest=%q", rest)
	}
}

func TestSplitSpokenSummary_EmptyInner(t *testing.T) {
	reply := "body\n<spoken_summary></spoken_summary>"
	spoken, rest, ok := splitSpokenSummary(reply)
	if ok {
		t.Fatalf("empty inner must be ok=false; spoken=%q", spoken)
	}
	if strings.Contains(rest, "spoken_summary") {
		t.Fatalf("empty tags should be stripped from rest; rest=%q", rest)
	}
	if !strings.Contains(rest, "body") {
		t.Fatalf("rest lost the body; rest=%q", rest)
	}
}

func TestStripStraySpokenTags(t *testing.T) {
	cases := map[string]string{
		"progress <spoken_summary>foo": "progress foo",
		"a</spoken_summary>b":          "ab",
		"<spoken_summary>x</spoken_summary>": "x",
		"no tags here":                       "no tags here",
	}
	for in, want := range cases {
		if got := stripStraySpokenTags(in); got != want {
			t.Fatalf("stripStraySpokenTags(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProjectKoeVoice_AuthoredTailBlock(t *testing.T) {
	reply := "| ticker | pct |\n|---|---|\n| NVDA | +3% |\n<spoken_summary>Your portfolio is up two percent.</spoken_summary>"
	spoken, cleaned, authored := projectKoeVoice(reply)
	if !authored {
		t.Fatal("authored = false, want true")
	}
	if spoken != "Your portfolio is up two percent." {
		t.Fatalf("spoken = %q", spoken)
	}
	if strings.Contains(cleaned, "spoken_summary") {
		t.Fatalf("cleaned still has tag: %q", cleaned)
	}
	if !strings.Contains(cleaned, "NVDA") {
		t.Fatalf("cleaned lost the detail: %q", cleaned)
	}
}

func TestProjectKoeVoice_TagOnlyKeepsSpokenAsReply(t *testing.T) {
	// Gap #3: a trivial task where the model writes only the block, no detail —
	// the cleaned reply must not be blank (Desktop would show an empty message).
	reply := "<spoken_summary>Done, your 9am task is set.</spoken_summary>"
	spoken, cleaned, authored := projectKoeVoice(reply)
	if !authored || spoken != "Done, your 9am task is set." {
		t.Fatalf("spoken=%q authored=%v", spoken, authored)
	}
	if strings.Contains(cleaned, "spoken_summary") {
		t.Fatalf("cleaned still has tag: %q", cleaned)
	}
	if cleaned != "Done, your 9am task is set." {
		t.Fatalf("tag-only reply must keep the spoken line as the reply, got %q", cleaned)
	}
}

func TestProjectKoeVoice_FallbackScrubsStrayTag(t *testing.T) {
	// Advisor blind spot: an unclosed tag must not be read aloud verbatim.
	reply := "<spoken_summary>NVDA is up today but the rest is missing"
	spoken, cleaned, authored := projectKoeVoice(reply)
	if authored {
		t.Fatal("authored = true, want false for a malformed block")
	}
	if strings.Contains(spoken, "spoken_summary") {
		t.Fatalf("fallback spoken must not contain the tag: %q", spoken)
	}
	if strings.Contains(cleaned, "spoken_summary") {
		t.Fatalf("fallback cleaned must not contain the tag: %q", cleaned)
	}
}

func TestProjectKoeVoice_FallbackPrefersTailLine(t *testing.T) {
	// Gap #1: with no authored tag, the mechanical fallback must take the RESULT
	// at the tail, not the progress line at the head.
	reply := "I'm searching the market data now.\nChecking your positions.\nYour tech holdings are up two percent today."
	spoken, _, authored := projectKoeVoice(reply)
	if authored {
		t.Fatal("authored = true, want false")
	}
	if strings.Contains(spoken, "searching") || strings.Contains(spoken, "Checking") {
		t.Fatalf("fallback grabbed a head progress line: %q", spoken)
	}
	if !strings.Contains(spoken, "up two percent") {
		t.Fatalf("fallback should surface the tail result line: %q", spoken)
	}
}

func TestStripSpokenSummaryFromContent_TailTextOnly(t *testing.T) {
	mc := client.NewTextContent("full reply body\n<spoken_summary>hi</spoken_summary>")
	out := stripSpokenSummaryFromContent(mc)
	if strings.Contains(out.Text(), "spoken_summary") {
		t.Fatalf("text still has tag: %q", out.Text())
	}
	if !strings.Contains(out.Text(), "full reply body") {
		t.Fatalf("text lost body: %q", out.Text())
	}
}

func TestStripSpokenSummaryFromContent_TagOnlyKeepsSpoken(t *testing.T) {
	// Gap #3 at the transcript site: a tag-only message must not become blank.
	mc := client.NewTextContent("<spoken_summary>Done.</spoken_summary>")
	out := stripSpokenSummaryFromContent(mc)
	if strings.TrimSpace(out.Text()) != "Done." {
		t.Fatalf("tag-only transcript should keep the spoken line, got %q", out.Text())
	}
}

func TestStripSpokenSummaryFromContent_PreservesThinkingBlock(t *testing.T) {
	mc := client.NewBlockContent([]client.ContentBlock{
		{Type: "thinking", Text: "private reasoning"},
		{Type: "text", Text: "The long answer follows.\n<spoken_summary>Short answer.</spoken_summary>"},
	})
	out := stripSpokenSummaryFromContent(mc)
	blocks := out.Blocks()
	if len(blocks) != 2 {
		t.Fatalf("block count = %d, want 2", len(blocks))
	}
	if blocks[0].Type != "thinking" || blocks[0].Text != "private reasoning" {
		t.Fatalf("thinking block was mutated: %+v", blocks[0])
	}
	if strings.Contains(blocks[1].Text, "spoken_summary") {
		t.Fatalf("text block still has tag: %q", blocks[1].Text)
	}
	if !strings.Contains(blocks[1].Text, "The long answer follows") {
		t.Fatalf("text block lost body: %q", blocks[1].Text)
	}
}

func TestStripSpokenSummaryFromLastAssistant(t *testing.T) {
	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("what's up")},
		{Role: "assistant", Content: client.NewTextContent("details\n<spoken_summary>All good.</spoken_summary>")},
	}
	stripSpokenSummaryFromLastAssistant(msgs)
	if strings.Contains(msgs[1].Content.Text(), "spoken_summary") {
		t.Fatalf("last assistant still has tag: %q", msgs[1].Content.Text())
	}
	if msgs[0].Content.Text() != "what's up" {
		t.Fatalf("user message was touched: %q", msgs[0].Content.Text())
	}
}
