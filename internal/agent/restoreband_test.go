package agent

import (
	"testing"

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
}
