package daemon

import "testing"

func TestIsKoeSource(t *testing.T) {
	cases := map[string]bool{
		"koe":         true,
		"KOE":         true,
		" koe ":       true,
		"koe-reachy":  true,
		"koe-bot":     true,
		" KOE-Reachy": true,
		"koer":        false, // must not match a "koe" prefix without the hyphen
		"kocoro":      false,
		"slack":       false,
		"":            false,
	}
	for input, want := range cases {
		if got := isKoeSource(input); got != want {
			t.Errorf("isKoeSource(%q) = %v, want %v", input, got, want)
		}
	}
	if ChannelKoe != "koe" {
		t.Errorf("ChannelKoe = %q, want \"koe\"", ChannelKoe)
	}
}
