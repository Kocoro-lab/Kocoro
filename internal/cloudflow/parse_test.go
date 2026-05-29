package cloudflow

import "testing"

func TestParseSlash(t *testing.T) {
	cases := []struct {
		in   string
		want *SlashCommand
	}{
		{"/research foo bar", &SlashCommand{Type: "research", Strategy: "standard", Query: "foo bar"}},
		{"/research deep what's new in agents", &SlashCommand{Type: "research", Strategy: "deep", Query: "what's new in agents"}},
		{"/research quick weather today", &SlashCommand{Type: "research", Strategy: "quick", Query: "weather today"}},
		{"/swarm build a launch plan", &SlashCommand{Type: "swarm", Query: "build a launch plan"}},
		{"/dag run 3 reviews in parallel", &SlashCommand{Type: "auto", Query: "run 3 reviews in parallel"}},
		{"plain user message", nil},
		{"/research", nil},      // empty query
		{"/research deep", nil}, // strategy with empty query
		{"/swarm", nil},         // empty query
		{"/dag", nil},           // empty query
		{"/unknown command", nil},
		{" /research foo", nil}, // leading whitespace not allowed (avoid quoting traps)
	}
	for _, tc := range cases {
		got := ParseSlash(tc.in)
		if (got == nil) != (tc.want == nil) {
			t.Fatalf("ParseSlash(%q): got %#v, want %#v", tc.in, got, tc.want)
		}
		if got != nil && *got != *tc.want {
			t.Fatalf("ParseSlash(%q): got %#v, want %#v", tc.in, *got, *tc.want)
		}
	}
}
