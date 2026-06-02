package tui

import (
	"strings"
	"testing"
)

// TestMarkdownRendererBothBackgrounds ensures the renderer builds and produces
// non-empty output on BOTH a dark and a light terminal background. The light
// path is the regression guard: the custom compactStyle hardcodes light-gray
// code text that is invisible on white, so light terminals fall back to
// glamour's tuned light palette.
func TestMarkdownRendererBothBackgrounds(t *testing.T) {
	const src = "# Heading\n\nSome `inline code` and a list:\n\n- alpha\n- beta\n"
	for _, dark := range []bool{true, false} {
		r, err := buildRenderer(80, dark)
		if err != nil || r == nil {
			t.Fatalf("buildRenderer(dark=%v): err=%v nil=%v", dark, err, r == nil)
		}
		out, err := r.Render(src)
		if err != nil {
			t.Fatalf("render(dark=%v): %v", dark, err)
		}
		if strings.TrimSpace(out) == "" {
			t.Fatalf("render(dark=%v) produced empty output", dark)
		}
	}
}
