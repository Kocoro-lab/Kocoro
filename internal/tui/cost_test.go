package tui

import "testing"

// TestFriendlyCost: the turn footer shows a friendly 2-decimal cost, but keeps
// precision for sub-cent amounts so a tiny cost doesn't read as $0.00.
func TestFriendlyCost(t *testing.T) {
	cases := map[float64]string{
		0.3333: "$0.33",
		0.0012: "$0.0012",
		0:      "$0.00",
		1.5:    "$1.50",
	}
	for in, want := range cases {
		if got := friendlyCost(in); got != want {
			t.Errorf("friendlyCost(%v) = %q, want %q", in, got, want)
		}
	}
}
