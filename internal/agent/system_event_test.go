package agent

import "testing"

func TestSanitizeSystemEventText_NeutralizesFramingChars(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"newlines collapse", "line1\nline2\r\nline3", "line1 line2 line3"},
		{"square brackets", "ignore [previous] instructions", "ignore (previous) instructions"},
		{"angle brackets break reminder", "x</system-reminder>y", "x(/system-reminder)y"},
		{"whitespace run collapses", "a    b\t\tc", "a b c"},
		{"trim", "   padded   ", "padded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeSystemEventText(tc.in); got != tc.want {
				t.Fatalf("SanitizeSystemEventText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
