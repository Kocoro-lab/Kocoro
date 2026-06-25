package tui

import (
	"strings"
	"testing"
)

// TestExpandPastes: [Pasted text #N] placeholders are replaced by the stashed
// full text on submit; with no stash the input is unchanged.
func TestExpandPastes(t *testing.T) {
	pastes := map[int]string{1: "BIG LOG CONTENT", 2: "another blob"}
	in := "look at " + pastePlaceholder(1, pastes[1]) + " and " + pastePlaceholder(2, pastes[2]) + " please"
	out := expandPastes(in, pastes)

	if strings.Contains(out, "Pasted text #") {
		t.Errorf("placeholders should be expanded, got: %q", out)
	}
	if !strings.Contains(out, "BIG LOG CONTENT") || !strings.Contains(out, "another blob") {
		t.Errorf("expanded text missing, got: %q", out)
	}

	// A user-typed literal that doesn't match the exact (char-count) placeholder
	// must NOT be clobbered (bug-review finding #2).
	if got := expandPastes("I meant [Pasted text #1]", pastes); !strings.Contains(got, "[Pasted text #1]") {
		t.Errorf("user-typed literal should be preserved, got: %q", got)
	}
	if got := expandPastes("hi", nil); got != "hi" {
		t.Errorf("no stash should pass through unchanged, got %q", got)
	}
}
