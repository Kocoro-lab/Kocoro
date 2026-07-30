package claudecode

import "testing"

func TestSymbolicForm(t *testing.T) {
	cases := []struct {
		abs  string
		home string
		want string
	}{
		{"/Users/alice/.claude", "/Users/alice", "~/.claude"},
		{"/Users/alice/.claude.json", "/Users/alice", "~/.claude.json"},
		{"/Users/alice/.shannon", "/Users/alice", "~/.shannon"},
		{"/opt/claude", "/Users/alice", "/opt/claude"},
	}
	for _, tc := range cases {
		got := SymbolicForm(tc.abs, tc.home)
		if got != tc.want {
			t.Errorf("SymbolicForm(%q, %q) = %q, want %q", tc.abs, tc.home, got, tc.want)
		}
	}
}

func TestDefaultSources(t *testing.T) {
	got := DefaultSources("/Users/alice")
	if got.ClaudeHome != "/Users/alice/.claude" {
		t.Errorf("ClaudeHome = %q", got.ClaudeHome)
	}
	if got.ClaudeUserConfig != "/Users/alice/.claude.json" {
		t.Errorf("ClaudeUserConfig = %q", got.ClaudeUserConfig)
	}
}
