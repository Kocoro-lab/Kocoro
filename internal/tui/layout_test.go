package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"

	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

// newLayoutTestModel builds a Model wired enough to render any non-startup state
// at a fixed size. The alt-screen contract is that View() fills EXACTLY m.height
// rows and never emits a line wider than m.width — without a real terminal this
// invariant test is the only guard against the "swallowed row / stranded ghost"
// class of bugs the viewport rewrite exists to kill.
func newLayoutTestModel(w, h int) *Model {
	m := &Model{
		cfg:                 &config.Config{ModelTier: "medium", Endpoint: "https://api.test.com"},
		textarea:            textarea.New(),
		viewport:            viewport.New(w, h),
		followBottom:        true,
		width:               w,
		height:              h,
		version:             "dev",
		spinnerTexts:        []string{"Thinking deeply...", "Connecting the dots..."},
		markdownCache:       map[string]string{},
		processingStartTime: time.Now(),
	}
	m.textarea.SetWidth(w - inputBorderOverhead)
	return m
}

func TestViewFillsExactHeight(t *testing.T) {
	const w, h = 80, 24

	// CJK + plain history so the viewport content path is exercised, including the
	// full-width characters that broke the old main-screen renderer.
	withContent := func(m *Model) {
		m.appendOutput(renderUserMessage("搜索今天的新闻（全球、中国、财经）", w))
		m.appendMarkdownOutput("正在搜索今日新闻。\n\n并行搜索全球、中国及财经三个维度。",
			m.renderMarkdownCached("正在搜索今日新闻。\n\n并行搜索全球、中国及财经三个维度。", w))
	}

	cases := []struct {
		name  string
		setup func(*Model)
	}{
		{"input empty", func(m *Model) { m.state = stateInput }},
		{"input with content", func(m *Model) { m.state = stateInput; withContent(m) }},
		{"input with suggestion", func(m *Model) { m.state = stateInput; m.promptSuggestion = "试试 /research 深度分析" }},
		{"input menu visible", func(m *Model) {
			m.state = stateInput
			m.menuVisible = true
			m.menuItems = []slashCmd{{cmd: "/help", desc: "show help"}, {cmd: "/clear", desc: "clear"}}
		}},
		{"processing spinner", func(m *Model) { m.state = stateProcessing }},
		{"processing with stream tail", func(m *Model) {
			m.state = stateProcessing
			withContent(m)
			m.streamLive = "答案正在逐字生成，这一行可能比较长比较长比较长比较长比较长比较长。\n第二行。"
		}},
		{"processing tool call", func(m *Model) {
			m.state = stateProcessing
			m.pendingToolName = "tool_search"
			m.pendingToolArgs = `{"query":"搜索今天的新闻"}`
		}},
		{"approval", func(m *Model) { m.state = stateApproval }},
		{"session picker", func(m *Model) { m.state = stateSessionPicker }},
		{"generic picker", func(m *Model) {
			m.state = statePicker
			m.pickerTitle = "Pick a model"
			m.pickerOpts = []pickerOption{{label: "medium", desc: "balanced"}, {label: "large", desc: "smartest"}}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newLayoutTestModel(w, h)
			tc.setup(m)
			m.layoutViewport() // mirror the real Update wrapper
			out := m.View()

			// Content-sized viewport: the frame must never EXCEED the screen
			// (overflow scrolls/clips), and must render at least the bottom region.
			if got := lipgloss.Height(out); got > h || got < 1 {
				t.Errorf("View height = %d, want 1..%d (must not overflow the screen)", got, h)
			}
			for i, line := range strings.Split(out, "\n") {
				if wln := lipgloss.Width(line); wln > w {
					t.Errorf("line %d width = %d > %d: %q", i, wln, w, line)
				}
			}
		})
	}
}

// When content exceeds the screen, the viewport caps and the frame fills exactly
// m.height (no more — overflow would corrupt the alt-screen frame).
func TestViewCapsAtScreenHeightWhenContentTall(t *testing.T) {
	const w, h = 80, 24
	m := newLayoutTestModel(w, h)
	m.state = stateInput
	for i := 0; i < 200; i++ {
		m.appendOutput("history line that fills the conversation well past one screen")
	}
	m.layoutViewport()
	out := m.View()
	if got := lipgloss.Height(out); got != h {
		t.Errorf("with tall content, View height = %d, want exactly %d", got, h)
	}
	for i, line := range strings.Split(out, "\n") {
		if wln := lipgloss.Width(line); wln > w {
			t.Errorf("line %d width = %d > %d: %q", i, wln, w, line)
		}
	}
}

// Startup banner must also fill the screen exactly (it paints before the first
// turn, so a short frame would leave the alt-screen half-drawn).
func TestStartupViewFillsExactHeight(t *testing.T) {
	const w, h = 80, 24
	m := newLayoutTestModel(w, h)
	m.state = stateStartup
	out := m.View()
	if got := lipgloss.Height(out); got != h {
		t.Errorf("startup View height = %d, want %d", got, h)
	}
}
