package cmd

import (
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/koe"
)

func TestParseActivityTier(t *testing.T) {
	for _, tt := range []struct {
		raw  string
		want koe.ActivityTier
	}{
		{"", koe.ActivityStandard},
		{"standard", koe.ActivityStandard},
		{" STANDARD ", koe.ActivityStandard},
		{"quiet", koe.ActivityQuiet},
		{"lively", koe.ActivityLively},
	} {
		got, err := parseActivityTier(tt.raw)
		if err != nil {
			t.Fatalf("parseActivityTier(%q): %v", tt.raw, err)
		}
		if got != tt.want {
			t.Fatalf("parseActivityTier(%q) = %v, want %v", tt.raw, got, tt.want)
		}
	}
	if _, err := parseActivityTier("hyper"); err == nil {
		t.Fatal("unknown activity tier must fail loud")
	}
}
