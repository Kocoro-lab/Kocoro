package tui

import (
	"reflect"
	"strings"
	"testing"
)

func TestFuzzySubsequence(t *testing.T) {
	cases := []struct {
		pattern, target string
		wantPos         []int
		wantOK          bool
	}{
		{"/rsch", "/research", []int{0, 1, 3, 7, 8}, true}, // / r s c h
		{"/re", "/research", []int{0, 1, 2}, true},         // prefix is also a subsequence
		{"/xyz", "/research", nil, false},
		{"/MODEL", "/model", []int{0, 1, 2, 3, 4, 5}, true}, // case-insensitive
		{"", "/anything", nil, true},
	}
	for _, c := range cases {
		pos, ok := fuzzySubsequence(c.pattern, c.target)
		if ok != c.wantOK {
			t.Errorf("fuzzySubsequence(%q,%q) ok=%v want %v", c.pattern, c.target, ok, c.wantOK)
			continue
		}
		if c.wantOK && c.pattern != "" && !reflect.DeepEqual(pos, c.wantPos) {
			t.Errorf("fuzzySubsequence(%q,%q) pos=%v want %v", c.pattern, c.target, pos, c.wantPos)
		}
	}
}

func TestHighlightChars(t *testing.T) {
	base := styleDim()
	hi := styleAccent()
	out := highlightChars("/research", []int{0, 1}, base, hi)
	// All original runes must survive (styling only adds ANSI around them).
	for _, r := range "/research" {
		if !strings.ContainsRune(out, r) {
			t.Errorf("rune %q missing from highlighted output %q", r, out)
		}
	}
	// No highlight positions -> plain base render, still contains the text.
	if got := highlightChars("/help", nil, base, hi); !strings.Contains(got, "help") {
		t.Errorf("highlightChars(nil pos) dropped text: %q", got)
	}
}
