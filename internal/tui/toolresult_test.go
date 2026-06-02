package tui

import (
	"fmt"
	"strings"
	"testing"
)

func TestTruncateHeadTail(t *testing.T) {
	// Short content (<= head+tail) is returned unchanged.
	in := "l1\nl2\nl3"
	if got := truncateHeadTail(in, 8, 4); got != in {
		t.Errorf("short content changed: %q", got)
	}

	// Long content: middle elided, head/tail preserved, structure intact
	// (the old strings.Fields path collapsed everything into one line).
	var b strings.Builder
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&b, "line%d\n", i)
	}
	got := truncateHeadTail(b.String(), 3, 2)
	if !strings.Contains(got, "… +15 lines") {
		t.Errorf("missing elision marker: %q", got)
	}
	if !strings.Contains(got, "line1\n") || !strings.Contains(got, "line20") {
		t.Errorf("head/tail not preserved: %q", got)
	}
	if n := strings.Count(got, "\n"); n < 5 {
		t.Errorf("expected multi-line output, got %d newlines: %q", n, got)
	}
}
