package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textarea"

	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

// TestView_StateInput_StatusBarShowsAgentAndModel: in the input state the
// bottom status bar must render the active agent and model tier. Reproduces
// the report that the bar (and the agent on it) was missing.
func TestView_StateInput_StatusBarShowsAgentAndModel(t *testing.T) {
	m := &Model{
		state: stateInput,
		width: 80,
		cfg:   &config.Config{ModelTier: "medium"},
	}
	m.textarea = textarea.New()
	m.textarea.SetWidth(78)

	out := m.View()

	if !strings.Contains(out, "/ commands") {
		t.Errorf("status bar '/ commands' hint missing; View()=\n%q", out)
	}
	if !strings.Contains(out, "default") {
		t.Errorf("status bar must show active agent 'default'; View()=\n%q", out)
	}
	if !strings.Contains(out, "medium") {
		t.Errorf("status bar must show model 'medium'; View()=\n%q", out)
	}
}
