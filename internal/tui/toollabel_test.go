package tui

import "testing"

// TestFriendlyToolLabel: known tools get a plain-language label; unknown tools
// pass through as their raw name (so the call form is preserved for them).
func TestFriendlyToolLabel(t *testing.T) {
	if got := friendlyToolLabel("web_search"); got != "Searching the web" {
		t.Errorf("web_search → %q, want 'Searching the web'", got)
	}
	if got := friendlyToolLabel("bash"); got != "Running a command" {
		t.Errorf("bash → %q", got)
	}
	if got := friendlyToolLabel("some_unknown_tool"); got != "some_unknown_tool" {
		t.Errorf("unknown tool should pass through, got %q", got)
	}
}
