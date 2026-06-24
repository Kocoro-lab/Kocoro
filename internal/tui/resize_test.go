package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

func newResizeTestModel(t *testing.T, width int) *Model {
	t.Helper()

	sessions := session.NewManager(t.TempDir())
	sessions.NewSession()

	m := &Model{
		cfg: &config.Config{
			ModelTier: "medium",
			Endpoint:  "https://api.test.com",
		},
		sessions:      sessions,
		textarea:      textarea.New(),
		width:         width,
		version:       "dev",
		headerCWD:     "/tmp/project",
		markdownCache: map[string]string{},
	}
	m.finishHeaderAnimation()
	m.pendingPrints = nil
	return m
}

func TestFinishHeaderAnimation_CommitsHeaderOnce(t *testing.T) {
	m := &Model{
		cfg: &config.Config{
			ModelTier: "medium",
			Endpoint:  "https://api.test.com",
		},
		width:         120,
		version:       "dev",
		headerCWD:     "/tmp/project",
		markdownCache: map[string]string{},
	}
	if cmd := m.finishHeaderAnimation(); cmd == nil {
		t.Fatal("expected startup finish to return a command (clear + flush)")
	}
	if len(m.output) == 0 {
		t.Fatal("expected startup header committed to output")
	}
	if !strings.Contains(m.output[0].rendered, "Kocoro CLI") {
		t.Fatal("expected the committed first block to be the startup header")
	}
}

// Write-once scrollback: rerenderOutput must NOT re-render committed blocks at a
// new width (resize keeps original wrap width, as in Codex/Claude Code).
func TestRerenderOutput_WriteOnce_DoesNotRerenderScrollback(t *testing.T) {
	m := newResizeTestModel(t, 120)
	before := m.output[0].rendered

	m.width = 60
	m.rerenderOutput()

	if m.output[0].rendered != before {
		t.Fatal("write-once: committed scrollback must not be re-rendered on resize")
	}
}

// When the caller wiped the conversation (/clear, /reset, Ctrl+L), rerenderOutput
// returns a clear command; otherwise it only flushes newly-appended blocks.
func TestRerenderOutput_ClearsWhenOutputEmpty(t *testing.T) {
	m := newResizeTestModel(t, 120)
	m.output = nil
	if cmd := m.rerenderOutput(); cmd == nil {
		t.Fatal("expected a clear command when output was wiped")
	}
}

func TestUpdate_WindowResize_UpdatesWidthWithoutReflowing(t *testing.T) {
	m := newResizeTestModel(t, 120)
	m.height = 40
	before := m.output[0].rendered

	m.update(tea.WindowSizeMsg{Width: 60, Height: 40})

	if m.width != 60 {
		t.Fatalf("resize should update width to 60, got %d", m.width)
	}
	if m.output[0].rendered != before {
		t.Fatal("resize must not re-flow committed scrollback (write-once)")
	}
}
