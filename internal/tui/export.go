package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// transcriptEntry is one turn in an exported conversation.
type transcriptEntry struct {
	role string
	text string
}

// formatTranscript renders a conversation to a plain Markdown transcript: a
// title heading followed by one labeled section per non-empty turn. Roles are
// humanized (user → You, assistant → Kocoro) for non-technical readers.
func formatTranscript(title string, entries []transcriptEntry) string {
	var sb strings.Builder
	sb.WriteString("# " + title + "\n\n")
	for _, e := range entries {
		if strings.TrimSpace(e.text) == "" {
			continue
		}
		label := e.role
		switch e.role {
		case "user":
			label = "You"
		case "assistant":
			label = "Kocoro"
		}
		sb.WriteString("## " + label + "\n\n" + e.text + "\n\n")
	}
	return sb.String()
}

// exportSlug turns a session title into a filesystem-safe slug (lowercase,
// alphanumerics, single dashes), falling back to "session" when empty.
func exportSlug(title string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "session"
	}
	return s
}

// exportTranscript writes the current conversation to a Markdown file under
// ~/.shannon/exports/ and reports the path. Invoked by /export.
func (m *Model) exportTranscript() {
	sess := m.sessions.Current()
	if sess == nil || len(sess.Messages) == 0 {
		m.appendOutput("  No conversation to export")
		return
	}
	entries := make([]transcriptEntry, 0, len(sess.Messages))
	for _, msg := range sess.Messages {
		entries = append(entries, transcriptEntry{role: msg.Role, text: msg.Content.Text()})
	}
	title := sess.Title
	if title == "" {
		title = "Kocoro session"
	}
	content := formatTranscript(title, entries)

	dir := filepath.Join(m.shannonDir, "exports")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		m.appendOutput(fmt.Sprintf("  Export failed: %v", err))
		return
	}
	path := filepath.Join(dir, exportSlug(title)+"-"+time.Now().Format("20060102-150405")+".md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		m.appendOutput(fmt.Sprintf("  Export failed: %v", err))
		return
	}
	m.appendOutput("  Exported transcript to " + path)
}
