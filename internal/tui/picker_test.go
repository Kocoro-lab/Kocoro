package tui

import (
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agents"
)

// TestAgentPickerOptions: the /agent picker lists the default agent first, then
// each named agent, with the entry's name as the option value.
func TestAgentPickerOptions(t *testing.T) {
	entries := []agents.AgentEntry{
		{Name: "research-bot"},
		{Name: "ops", Builtin: true},
	}
	opts := agentPickerOptions(entries)
	if len(opts) != 3 {
		t.Fatalf("got %d options, want 3 (default + 2 named)", len(opts))
	}
	if opts[0].label != "default" || opts[0].value != "" {
		t.Errorf("first option must be the default agent, got %+v", opts[0])
	}
	if opts[1].value != "research-bot" {
		t.Errorf("opts[1].value = %q, want research-bot", opts[1].value)
	}
	if opts[2].value != "ops" {
		t.Errorf("opts[2].value = %q, want ops", opts[2].value)
	}
}

// TestModelTierOptions: the model picker offers the three routing tiers, in
// order, each with a description (config.go validates small/medium/large).
func TestModelTierOptions(t *testing.T) {
	opts := modelTierOptions()
	want := []string{"small", "medium", "large"}
	if len(opts) != len(want) {
		t.Fatalf("got %d options, want %d", len(opts), len(want))
	}
	for i, w := range want {
		if opts[i].value != w {
			t.Errorf("opt[%d].value = %q, want %q", i, opts[i].value, w)
		}
		if opts[i].desc == "" {
			t.Errorf("opt[%d] (%s) should have a description", i, w)
		}
	}
}

// TestPickerWrap: arrow navigation wraps at both ends and is safe on an empty
// list.
func TestPickerWrap(t *testing.T) {
	tests := []struct{ idx, n, want int }{
		{-1, 3, 2}, // up from top → bottom
		{3, 3, 0},  // down from bottom → top
		{1, 3, 1},  // middle unchanged
		{0, 0, 0},  // empty list
	}
	for _, tt := range tests {
		if got := pickerWrap(tt.idx, tt.n); got != tt.want {
			t.Errorf("pickerWrap(%d,%d) = %d, want %d", tt.idx, tt.n, got, tt.want)
		}
	}
}
