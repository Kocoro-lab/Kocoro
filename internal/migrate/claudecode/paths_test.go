package claudecode

import "testing"

func TestSymbolicForm(t *testing.T) {
	cases := []struct {
		abs  string
		home string
		want string
	}{
		{"/Users/wayland/.claude", "/Users/wayland", "~/.claude"},
		{"/Users/wayland/.claude.json", "/Users/wayland", "~/.claude.json"},
		{"/Users/wayland/.shannon", "/Users/wayland", "~/.shannon"},
		{"/opt/claude", "/Users/wayland", "/opt/claude"},
	}
	for _, tc := range cases {
		got := SymbolicForm(tc.abs, tc.home)
		if got != tc.want {
			t.Errorf("SymbolicForm(%q, %q) = %q, want %q", tc.abs, tc.home, got, tc.want)
		}
	}
}

func TestDefaultSources(t *testing.T) {
	got := DefaultSources("/Users/wayland")
	if got.ClaudeHome != "/Users/wayland/.claude" {
		t.Errorf("ClaudeHome = %q", got.ClaudeHome)
	}
	if got.ClaudeUserConfig != "/Users/wayland/.claude.json" {
		t.Errorf("ClaudeUserConfig = %q", got.ClaudeUserConfig)
	}
}
