package tui

import (
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

func TestFinishHeaderAnimation_HeaderBlockRerendersOnResize(t *testing.T) {
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
		t.Fatal("expected startup finish to trigger a repaint")
	}
	if len(m.output) == 0 {
		t.Fatal("expected startup header in output")
	}
	if m.output[0].rerender == nil {
		t.Fatal("expected startup header to store a rerender function")
	}

	m.width = 60
	cmd := m.rerenderOutput()
	if cmd == nil {
		t.Fatal("expected rerender command when not processing")
	}

	want := renderStartupHeader(headerTotalFrames-1, 60, "dev", "medium", "https://api.test.com", "/tmp/project", nil, 0, "default")
	if got := m.output[0].rendered; got != want {
		t.Fatal("expected startup header to rerender at the new width")
	}
}

func TestRerenderOutput_RepaintsWhileProcessing(t *testing.T) {
	m := newResizeTestModel(t, 120)

	m.state = stateProcessing
	m.width = 60
	cmd := m.rerenderOutput()
	if cmd == nil {
		t.Fatal("expected rerender command while processing")
	}

	want := renderStartupHeader(headerTotalFrames-1, 60, "dev", "medium", "https://api.test.com", "/tmp/project", nil, 0, "default")
	if got := m.output[0].rendered; got != want {
		t.Fatal("expected stored header rendering to update immediately")
	}
}

func TestUpdate_WindowResizeWhileProcessingTriggersRepaint(t *testing.T) {
	m := newResizeTestModel(t, 120)
	m.state = stateProcessing
	m.height = 40
	_, cmd := m.update(tea.WindowSizeMsg{Width: 60, Height: 40})
	if cmd == nil {
		t.Fatal("expected resize during processing to trigger a repaint")
	}
}
