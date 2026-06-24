package tui

import (
	"strings"
	"testing"
)

// TestExpandPastes: [Pasted text #N] placeholders are replaced by the stashed
// full text on submit; with no stash the input is unchanged.
func TestExpandPastes(t *testing.T) {
	pastes := map[int]string{1: "BIG LOG CONTENT", 2: "another blob"}
	in := "look at [Pasted text #1] and [Pasted text #2] please"
	out := expandPastes(in, pastes)

	if strings.Contains(out, "[Pasted text #1]") || strings.Contains(out, "[Pasted text #2]") {
		t.Errorf("placeholders should be expanded, got: %q", out)
	}
	if !strings.Contains(out, "BIG LOG CONTENT") || !strings.Contains(out, "another blob") {
		t.Errorf("expanded text missing, got: %q", out)
	}

	if got := expandPastes("hi", nil); got != "hi" {
		t.Errorf("no stash should pass through unchanged, got %q", got)
	}
}
