package tui

import (
	"testing"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// First Ctrl+C on an empty composer must ARM exit without discarding the
// conversation. (Regression guard: it previously wiped m.output + started a new
// session, silently losing context for anyone who hit Ctrl+C reflexively.)
func TestCtrlC_FirstPress_ArmsWithoutClearing(t *testing.T) {
	sessions := session.NewManager(t.TempDir())
	sessions.NewSession()
	m := &Model{
		state:    stateInput,
		width:    80,
		height:   24,
		viewport: viewport.New(80, 20),
		textarea: textarea.New(),
		cfg:      &config.Config{ModelTier: "medium"},
		sessions: sessions,
	}
	m.appendOutput("a prior message in the conversation")
	before := len(m.output)

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})

	if !m.ctrlCArmed {
		t.Error("first Ctrl+C should arm exit")
	}
	if len(m.output) != before {
		t.Errorf("first Ctrl+C must NOT clear the conversation: output went %d → %d", before, len(m.output))
	}
}

// Any other key disarms the exit prompt, so only two CONSECUTIVE Ctrl+C exit.
func TestCtrlC_DisarmedByOtherKey(t *testing.T) {
	m := &Model{
		state:    stateInput,
		width:    80,
		height:   24,
		viewport: viewport.New(80, 20),
		textarea: textarea.New(),
		cfg:      &config.Config{ModelTier: "medium"},
	}
	m.ctrlCArmed = true
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if m.ctrlCArmed {
		t.Error("a non-Ctrl+C key should disarm the exit prompt")
	}
}
