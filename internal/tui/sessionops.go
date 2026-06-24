package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// searchHistory returns the most recent past input containing query
// (case-insensitive), searched newest-first. ok=false on empty query / no match.
// Backs Ctrl+R: type a fragment, then Ctrl+R jumps the composer to the match.
func searchHistory(history []string, query string) (string, bool) {
	if strings.TrimSpace(query) == "" {
		return "", false
	}
	q := strings.ToLower(query)
	for i := len(history) - 1; i >= 0; i-- {
		if strings.Contains(strings.ToLower(history[i]), q) {
			return history[i], true
		}
	}
	return "", false
}

// forkMessages copies a conversation's messages into an INDEPENDENT slice so a
// forked session continues without mutating the original.
func forkMessages(src []client.Message) []client.Message {
	return append([]client.Message(nil), src...)
}

// forkSession branches the session with id into a fresh session carrying a copy
// of its messages, then makes the fork current — so the user can "try something
// else from here" without disturbing the original. Invoked by 'f' in the picker.
func (m *Model) forkSession(id string) tea.Cmd {
	src, err := m.sessions.Resume(id)
	if err != nil {
		m.appendOutput(fmt.Sprintf("  Fork failed: %v", err))
		return m.flushPrints()
	}
	srcTitle := src.Title

	fork := m.sessions.NewSession()
	fork.Messages = forkMessages(src.Messages)
	if srcTitle != "" && srcTitle != "New session" {
		fork.Title = srcTitle + " (fork)"
		fork.TitleAuto = false
	}
	m.resumedSession = true
	m.sessionAllowed = make(map[string]bool)
	m.applyRuntimeContext(fork)
	// Render the forked conversation to scrollback — mirrors the resume path
	// (loadSessionHistory renders async, hence no flushPrints). The "(fork)"
	// title tells the user this is a branch of the original.
	m.loadSessionHistory(fork)
	return nil
}
