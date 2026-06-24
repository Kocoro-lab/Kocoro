package tui

import "testing"

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
