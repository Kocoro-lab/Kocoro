package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestStreamDeltaAccumulatesAndClears exercises the live-preview lifecycle:
// deltas accumulate while processing, and any commit-to-scrollback boundary
// clears the preview so it can never duplicate the finalized answer.
func TestStreamDeltaAccumulatesAndClears(t *testing.T) {
	m := &Model{state: stateProcessing, width: 80, spinnerTexts: []string{"."}}

	m.Update(streamDeltaMsg{delta: "Hello "})
	m.Update(streamDeltaMsg{delta: "world"})
	if m.streamLive != "Hello world" {
		t.Fatalf("streamLive = %q, want %q", m.streamLive, "Hello world")
	}

	// Committing a segment (preamble/status/cloud) clears the preview.
	m.Update(streamOutputMsg{text: "committed"})
	if m.streamLive != "" {
		t.Errorf("streamOutputMsg did not clear streamLive: %q", m.streamLive)
	}

	// A tool result clears it too.
	m.streamLive = "partial"
	m.Update(toolResultMsg{name: "bash", content: "done"})
	if m.streamLive != "" {
		t.Errorf("toolResultMsg did not clear streamLive: %q", m.streamLive)
	}
}

func TestStreamPreview(t *testing.T) {
	// Keeps only the last maxLines lines.
	out := streamPreview("a\nb\nc\nd\ne", 80, 3)
	if strings.Contains(out, "a") || strings.Contains(out, "b") {
		t.Errorf("expected only the last 3 lines, got %q", out)
	}
	if !strings.Contains(out, "c") || !strings.Contains(out, "e") {
		t.Errorf("missing tail lines: %q", out)
	}

	// Long lines are truncated to width, never overflowing the terminal.
	out = streamPreview(strings.Repeat("x", 200), 20, 8)
	for _, ln := range strings.Split(out, "\n") {
		if w := lipgloss.Width(ln); w > 20 {
			t.Errorf("preview line width %d > 20: %q", w, ln)
		}
	}

	if streamPreview("", 80, 8) != "" {
		t.Errorf("empty input should yield empty preview")
	}
}

func TestBoundStreamTail(t *testing.T) {
	// Under the cap: returned unchanged.
	if got := boundStreamTail("short\ntext"); got != "short\ntext" {
		t.Errorf("boundStreamTail(short) = %q, want unchanged", got)
	}
	// Over the cap: bounded and cut at a line boundary (no mid-line start).
	big := strings.Repeat("line of text\n", 2000) // ~24KB
	got := boundStreamTail(big)
	if len(got) > streamLiveMaxBytes {
		t.Errorf("boundStreamTail did not bound: len=%d > %d", len(got), streamLiveMaxBytes)
	}
	if strings.HasPrefix(got, "ine") || strings.HasPrefix(got, "ne of") {
		t.Errorf("boundStreamTail started mid-line: %q", got[:20])
	}
	// The newest content is retained.
	if !strings.HasSuffix(strings.TrimRight(got, "\n"), "line of text") {
		t.Errorf("boundStreamTail dropped the tail: %q", got[len(got)-20:])
	}
}
