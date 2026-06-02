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
	// Captions wider than the bar must degrade to a plain separator that still
	// fits the width budget — never overflow (which would wrap the input line).
	if got := composeBar(3, "leftcaption", "rightcaption"); lipgloss.Width(got) > 3 {
		t.Errorf("composeBar(narrow) width = %d, want <= 3 (got %q)", lipgloss.Width(got), got)
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
