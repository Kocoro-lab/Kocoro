package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
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
		viewport:      viewport.New(width, 20),
		followBottom:  true,
		width:         width,
		height:        40,
		version:       "dev",
		headerCWD:     "/tmp/project",
		markdownCache: map[string]string{},
	}
	m.finishHeaderAnimation()
	return m
}

func TestFinishHeaderAnimation_CommitsHeaderOnce(t *testing.T) {
	m := &Model{
		cfg: &config.Config{
			ModelTier: "medium",
			Endpoint:  "https://api.test.com",
		},
		viewport:      viewport.New(120, 20),
		width:         120,
		height:        40,
		version:       "dev",
		headerCWD:     "/tmp/project",
		markdownCache: map[string]string{},
	}
	m.finishHeaderAnimation()
	if len(m.output) == 0 {
		t.Fatal("expected startup header committed to output")
	}
	if !strings.Contains(m.output[0].rendered, "Kocoro CLI") {
		t.Fatal("expected the committed first block to be the startup header")
	}
	// The viewport model marks content dirty rather than emitting a flush command.
	if !m.viewportDirty {
		t.Fatal("expected finishHeaderAnimation to mark the viewport dirty")
	}
}

// Resize updates the width and flags the viewport for a re-flow. Under the
// viewport architecture, committed history DOES re-flow at the new width (a
// strict improvement over the old write-once scrollback) — markdown blocks are
// re-rendered from their raw source via the (raw,width)-keyed cache.
func TestResize_UpdatesWidthAndMarksDirty(t *testing.T) {
	m := newResizeTestModel(t, 120)
	m.viewportDirty = false // clear the post-construction flag

	m.update(tea.WindowSizeMsg{Width: 60, Height: 40})

	if m.width != 60 {
		t.Fatalf("resize should update width to 60, got %d", m.width)
	}
	if !m.viewportDirty {
		t.Fatal("resize should mark the viewport dirty so content re-flows at the new width")
	}
}

// Markdown history re-flows on resize: a wide-rendered block re-renders narrower
// when the terminal shrinks (the win the viewport buys over write-once).
func TestResize_ReflowsMarkdownHistory(t *testing.T) {
	m := newResizeTestModel(t, 120)
	raw := "This is a reasonably long paragraph of prose that will wrap differently at 120 columns than it does at 40 columns when rendered as markdown."
	m.appendMarkdownOutput(raw, m.renderMarkdownCached(raw, 120))

	m.width = 40
	m.layoutViewport()
	narrow := m.viewport.View()

	m.width = 120
	m.layoutViewport()
	wide := m.viewport.View()

	if narrow == wide {
		t.Fatal("expected markdown history to re-flow between 40 and 120 columns")
	}
}
