package tui

import (
	"os"
	"strings"
	"testing"
)

// TestNoRawSemanticColors guards the theme.go migration: the listed semantic
// 256-color indices must not reappear as raw lipgloss.Color("N") in TUI source.
// New code should reference the adaptive tokens in theme.go so light terminals
// stay readable. Intentional non-semantic palettes (spinColors gradient, frog
// pixel art, the shimmer sweep) use other indices and are unaffected.
func TestNoRawSemanticColors(t *testing.T) {
	banned := []string{"243", "196", "42", "214", "237", "39", "252", "111", "146"}
	files := []string{"app.go", "header.go", "toolformat.go", "doctor.go", "compact.go"}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		text := string(src)
		for _, code := range banned {
			needle := `lipgloss.Color("` + code + `")`
			if strings.Contains(text, needle) {
				t.Errorf("%s contains raw %s — use an adaptive token from theme.go", f, needle)
			}
		}
	}
}
