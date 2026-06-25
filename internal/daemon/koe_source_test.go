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

func TestKoeMessagingClassification(t *testing.T) {
	for _, src := range []string{"koe", "koe-reachy", " KOE "} {
		if !IsMessagingPlatform(src) {
			t.Errorf("IsMessagingPlatform(%q) = false, want true", src)
		}
		if got := kindOf(src); got != SessionKindIM {
			t.Errorf("kindOf(%q) = %q, want %q", src, got, SessionKindIM)
		}
		if isInteractiveSource(src) {
			t.Errorf("isInteractiveSource(%q) = true, want false (koe is a burst session, not the user's interactive chat)", src)
		}
	}
}
