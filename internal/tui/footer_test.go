package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestComposeBar(t *testing.T) {
	out := composeBar(40, " left", "right ")
	if w := lipgloss.Width(out); w != 40 {
		t.Errorf("composeBar width = %d, want 40", w)
	}
	if !strings.Contains(out, "left") || !strings.Contains(out, "right") {
		t.Errorf("captions missing from %q", out)
	}
	// Captions wider than the bar must not panic (negative repeat count).
	if got := composeBar(3, "leftcaption", "rightcaption"); got == "" {
		t.Errorf("composeBar(narrow) returned empty")
	}
}

func TestRenderWaveTextStable(t *testing.T) {
	// Must stay non-empty and panic-free across a full sweep period.
	for tick := 0; tick < 40; tick++ {
		if out := renderWaveText("Thinking…", tick); out == "" {
			t.Fatalf("empty shimmer at tick %d", tick)
		}
	}
	if renderWaveText("", 5) != "" {
		t.Errorf("empty input should yield empty output")
	}
}
