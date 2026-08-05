package agent

import (
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	ctxwin "github.com/Kocoro-lab/ShanClaw/internal/context"
)

// TestRestoreCapFitsAbsoluteHysteresisBand pins the cross-package relation
// between the post-compaction restoration cap and the trigger/landing
// hysteresis band: restoration budgets against the TRIGGER line, so on the
// absolute-buffer regime the band (trigger − landing) must hold the full
// restoreTotalTokenCap plus more than one large turn pair of slack — else
// one moderate tool result after restoration immediately re-arms the
// compaction that was just paid for. window − 2×buffer failed this (band
// 60K vs cap 50K → ~10K slack); window − 3×buffer restores it.
func TestRestoreCapFitsAbsoluteHysteresisBand(t *testing.T) {
	const largeTurnPairTokens = 15_000
	for _, w := range []int{900_000, 1_000_000, 2_000_000} {
		band := ctxwin.CompactTriggerTokens(w) - ctxwin.CompactLandingTokens(w)
		if band <= restoreTotalTokenCap+largeTurnPairTokens {
			t.Errorf("window %d: hysteresis band %d must exceed restore cap %d + one large turn pair %d",
				w, band, restoreTotalTokenCap, largeTurnPairTokens)
		}
	}
	// Transitional regime (absolute trigger, fractional landing — windows in
	// (600K, 900K)): the strong invariant above does not hold there, but the
	// band must never be NARROWER than the pre-existing fractional baseline
	// (10% of window) — the absolute complement may only widen it.
	for _, w := range []int{610_000, 700_000, 800_000, 899_000} {
		band := ctxwin.CompactTriggerTokens(w) - ctxwin.CompactLandingTokens(w)
		if fractionalBand := w / 10; band < fractionalBand {
			t.Errorf("window %d: band %d narrower than the fractional baseline %d", w, band, fractionalBand)
		}
	}
}

// TestShouldPreflightCompact_AbsoluteOrderingAt1M pins the layered-gate
// ordering on the absolute regime — landing < trigger < preflight — and the
// preflight reserve floor: every pre-existing shouldPreflightCompact test
// runs at 200K where the absolute branch never fires.
func TestShouldPreflightCompact_AbsoluteOrderingAt1M(t *testing.T) {
	const w = 1_000_000
	trigger := ctxwin.CompactTriggerTokens(w)
	landing := ctxwin.CompactLandingTokens(w)
	if !(landing < trigger) {
		t.Fatalf("landing %d must stay below trigger %d", landing, trigger)
	}
	// The preflight line sits at w − max(buffer/2, defaultMaxOutputTokens)
	// = 968K: above the trigger (backstop ordering) and reserving at least
	// the fallback output ceiling below the window.
	preflightLine := w - defaultMaxOutputTokens
	if !(trigger < preflightLine) {
		t.Fatalf("preflight line %d must stay above trigger %d", preflightLine, trigger)
	}
	probe := func(target int) bool {
		msgs := []client.Message{{Role: "user", Content: client.NewTextContent("x")}}
		return shouldPreflightCompact(msgs, w, target-ctxwin.EstimateTokens(msgs))
	}
	if probe(950_000) {
		t.Error("950K estimate must NOT preflight — the absolute branch supersedes the 95% fraction on 1M")
	}
	if !probe(preflightLine + 1_000) {
		t.Error("an estimate over the absolute preflight line must trigger the backstop")
	}
}
