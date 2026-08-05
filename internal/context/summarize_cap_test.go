package context

import (
	"math"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// densestMeasuredBytesPerToken is the worst ratio observed when the live
// small tier was measured on 2026-08-05 (a JSON fixture; chinese 2.71, go
// source 3.43, english prose 4.78 were all looser). The safety floor used by
// the production code must sit at or below it.
const densestMeasuredBytesPerToken = 2.60

// summarizeMaxTokens mirrors the MaxTokens on GenerateSummary's request. The
// model counts prompt + reserved output against one window, so the budget has
// to leave room for it.
const summarizeMaxTokens = 2000

// TestSummarizeInputCapFitsSmallTierWindow is the regression guard for the
// 2026-08-05 production incident: summarizeInputCapBytes was 540_000 bytes
// documented as "≈180K tokens", but dense tool-result transcripts bill ~2.5
// bytes/token, so the call arrived at 213,719 tokens against a 200K window.
// Every compaction 400'd, which meant the session could never shrink and grew
// ~2K tokens per failed turn instead.
//
// This test fails if anyone raises the budget (or loosens the floor) back past
// what the small tier can actually accept.
func TestSummarizeInputCapFitsSmallTierWindow(t *testing.T) {
	// The small tier routes to a 200K-window model family.
	const smallTierWindow = 200_000

	if conservativeBytesPerToken > densestMeasuredBytesPerToken {
		t.Fatalf("safety floor %.2f is looser than the densest measured content %.2f — "+
			"the estimate would run low on exactly the transcripts that overflow",
			conservativeBytesPerToken, densestMeasuredBytesPerToken)
	}

	worstCaseTokens := int(math.Ceil(float64(summarizeInputCapBytes)/densestMeasuredBytesPerToken)) +
		int(math.Ceil(float64(len(summarizePrompt))/densestMeasuredBytesPerToken)) +
		summarizeMaxTokens

	if worstCaseTokens >= smallTierWindow {
		t.Fatalf("a maximally-sized summarize call bills ~%d tokens against a %d window — "+
			"lower summarizeInputCapTokens (currently %d)",
			worstCaseTokens, smallTierWindow, summarizeInputCapTokens)
	}
	t.Logf("worst case ~%d tokens, %d headroom under the %d window",
		worstCaseTokens, smallTierWindow-worstCaseTokens, smallTierWindow)
}

// TestCapTranscriptForSummarize_StaysUnderBudget covers both the byte-aligned
// and multi-byte paths: CJK is the case where a byte-oriented cap can slice
// mid-rune, and it is also near the dense end of the ratio range.
func TestCapTranscriptForSummarize_StaysUnderBudget(t *testing.T) {
	cases := map[string]string{
		"ascii": strings.Repeat("a", summarizeInputCapBytes*2),
		"cjk":   strings.Repeat("摘要压缩", summarizeInputCapBytes/2),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			got := capTranscriptForSummarize(input)
			if len(got) > summarizeInputCapBytes {
				t.Errorf("capped transcript is %d bytes, over the %d ceiling", len(got), summarizeInputCapBytes)
			}
			if estimateSummarizeTokens(got) > summarizeInputCapTokens {
				t.Errorf("capped transcript estimates %d tokens, over the %d budget",
					estimateSummarizeTokens(got), summarizeInputCapTokens)
			}
			if !utf8.ValidString(got) {
				t.Error("capping produced invalid UTF-8 — a rune was sliced in half")
			}
		})
	}
}

// A transcript that already fits must come back untouched: the cap is a safety
// bound, not a rewrite.
func TestCapTranscriptForSummarize_PassesThroughWhenFitting(t *testing.T) {
	input := strings.Repeat("x", summarizeInputCapBytes-1)
	if got := capTranscriptForSummarize(input); got != input {
		t.Errorf("fitting transcript was modified: %d bytes in, %d out", len(input), len(got))
	}
}

// Every fold chunk goes to the same small tier as the single-shot path, so a
// chunk must respect the same budget once capped — including the pathological
// case of one oversized message, which splitTranscriptChunks deliberately
// emits as a chunk of its own.
func TestFoldChunksRespectTheSummarizeBudget(t *testing.T) {
	messages := []client.Message{
		{Role: "user", Content: client.NewTextContent(strings.Repeat("a", summarizeInputCapBytes/2))},
		{Role: "assistant", Content: client.NewTextContent(strings.Repeat("b", summarizeInputCapBytes/2))},
		// One unit far larger than the whole budget.
		{Role: "user", Content: client.NewTextContent(strings.Repeat("c", summarizeInputCapBytes*3))},
		{Role: "assistant", Content: client.NewTextContent(strings.Repeat("d", summarizeInputCapBytes/2))},
	}
	chunks := splitTranscriptChunks(messages, summarizeInputCapBytes)
	if len(chunks) < 2 {
		t.Fatalf("expected the oversized unit to be split out, got %d chunk(s)", len(chunks))
	}
	for i, ch := range chunks {
		capped := capTranscriptForSummarize(buildTranscript(ch))
		if estimateSummarizeTokens(capped) > summarizeInputCapTokens {
			t.Errorf("chunk %d estimates %d tokens after capping, over the %d budget",
				i, estimateSummarizeTokens(capped), summarizeInputCapTokens)
		}
	}
}

// Fold coverage is chunk count × per-chunk budget, so lowering the budget
// silently shrinks how far back a summary can reach — the exact regression
// that shipping the 2026-08-05 cap fix alone would have caused (6 × 540K =
// 3.24MB became 6 × 375K = 2.25MB). Coverage must not fall below what the
// path was built to handle.
func TestFoldCoverageDoesNotRegress(t *testing.T) {
	// Coverage before the budget was re-derived from measurement.
	const priorCoverageBytes = 6 * 540_000

	coverage := maxSummaryFoldChunks * summarizeInputCapBytes
	if coverage < priorCoverageBytes {
		t.Fatalf("fold now covers %d bytes, down from %d — raise maxSummaryFoldChunks "+
			"(currently %d) to hold coverage when the per-chunk budget drops",
			coverage, priorCoverageBytes, maxSummaryFoldChunks)
	}
	t.Logf("fold covers %d bytes across %d chunks of %d", coverage, maxSummaryFoldChunks, summarizeInputCapBytes)
}
