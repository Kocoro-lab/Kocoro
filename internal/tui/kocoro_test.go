package tui

import (
	"strings"
	"testing"
)

// TestRenderKocoroGrid_Dimensions: the Kocoro swirl renders as 8 terminal lines
// (16px tall via half-blocks) containing block glyphs.
func TestRenderKocoroGrid_Dimensions(t *testing.T) {
	lines := renderKocoroGrid(0)
	if len(lines) != 8 {
		t.Fatalf("renderKocoroGrid should return 8 lines, got %d", len(lines))
	}
	hasInk := false
	for _, ln := range lines {
		if strings.ContainsAny(ln, "▀▄█") {
			hasInk = true
		}
	}
	if !hasInk {
		t.Error("rendered swirl should contain half-block glyphs")
	}
}

// TestRenderKocoroGrid_FramesShimmer: the gradient phase shifts per frame so the
// startup banner shimmers (keeps the header "frames differ" animation contract).
func TestRenderKocoroGrid_FramesShimmer(t *testing.T) {
	a := strings.Join(renderKocoroGrid(0), "\n")
	b := strings.Join(renderKocoroGrid(6), "\n")
	if a == b {
		t.Error("gradient phase should differ between frames (shimmer)")
	}
}
