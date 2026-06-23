package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestRenderInputBox_BordersContent: the composer is wrapped in a rounded border
// of the requested total width, containing the composer view.
func TestRenderInputBox_BordersContent(t *testing.T) {
	out := renderInputBox("› hello", 30)
	if !strings.Contains(out, "hello") {
		t.Error("box should contain the composer content")
	}
	if !strings.Contains(out, "╭") || !strings.Contains(out, "╰") {
		t.Error("box should have rounded top/bottom borders")
	}
	first := strings.SplitN(out, "\n", 2)[0]
	if w := lipgloss.Width(first); w != 30 {
		t.Errorf("box top border width = %d, want 30", w)
	}
}

// TestRenderInputBox_NarrowFallback: too-narrow widths return the content
// unboxed rather than producing a broken/overflowing frame.
func TestRenderInputBox_NarrowFallback(t *testing.T) {
	if got := renderInputBox("x", 2); got != "x" {
		t.Errorf("narrow width should pass content through, got %q", got)
	}
}
