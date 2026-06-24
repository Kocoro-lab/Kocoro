package tui

import "testing"

// TestShowCommandPalette: the palette key lists every command (not just ones
// matching typed text) and makes the menu visible for arrow-selection.
func TestShowCommandPalette(t *testing.T) {
	m := &Model{}
	m.showCommandPalette()
	if !m.menuVisible {
		t.Error("palette should make the command menu visible")
	}
	if len(m.menuItems) != len(baseSlashCommands) {
		t.Errorf("palette should list all %d commands, got %d", len(baseSlashCommands), len(m.menuItems))
	}
}

// TestIsImmediateCommand: in the slash autocomplete menu, Enter executes a
// command that needs no argument (pickers like /agent & /model, and bare no-arg
// commands) instead of merely autocompleting it; commands that need a typed
// argument keep autocompleting so the user can type it.
func TestIsImmediateCommand(t *testing.T) {
	executes := []string{"/agent", "/model", "/help", "/clear", "/reset",
		"/config", "/sessions", "/session", "/status", "/doctor", "/quit", "/exit"}
	for _, c := range executes {
		if !isImmediateCommand(c) {
			t.Errorf("%s takes no required argument; Enter should execute it", c)
		}
	}

	needsArg := []string{"/rename", "/search", "/research", "/swarm"}
	for _, c := range needsArg {
		if isImmediateCommand(c) {
			t.Errorf("%s needs a typed argument; Enter should autocomplete, not execute", c)
		}
	}
}
