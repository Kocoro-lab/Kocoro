package cmd

import (
	"strings"
	"testing"
)

func TestParseStatefulFlag_Valid(t *testing.T) {
	cases := []struct {
		in   string
		want *bool
		err  bool
	}{
		{"", nil, false}, // empty → no change (Update)
		{"true", boolPtr(true), false},
		{"false", boolPtr(false), false},
		{"True", boolPtr(true), false}, // ParseBool is case-insensitive
		{"FALSE", boolPtr(false), false},
		{"1", boolPtr(true), false},
		{"0", boolPtr(false), false},
		{"maybe", nil, true}, // critical: must NOT silently treat as false
		{"yes", nil, true},
		{"no", nil, true},
	}
	for _, c := range cases {
		got, err := parseStatefulFlag(c.in)
		if c.err {
			if err == nil {
				t.Errorf("parseStatefulFlag(%q): expected error, got nil", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseStatefulFlag(%q): unexpected error %v", c.in, err)
			continue
		}
		if (got == nil) != (c.want == nil) {
			t.Errorf("parseStatefulFlag(%q): nil-ness mismatch: got %v, want %v", c.in, got, c.want)
			continue
		}
		if got != nil && *got != *c.want {
			t.Errorf("parseStatefulFlag(%q): got %v want %v", c.in, *got, *c.want)
		}
	}
}

func TestParseStatefulFlag_ErrorMessage(t *testing.T) {
	_, err := parseStatefulFlag("maybe")
	if err == nil || !strings.Contains(err.Error(), "stateful") {
		t.Errorf("error must mention 'stateful' to help user, got %v", err)
	}
}

func boolPtr(b bool) *bool { return &b }
