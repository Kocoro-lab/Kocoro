package tui

import (
	"log"
	"os"
	"path/filepath"
)

// RedirectLogs routes the standard log package away from stderr to
// <shannonDir>/logs/tui.log for the lifetime of a TUI session.
//
// Why: the TUI takes over the terminal via Bubbletea. MCP / Chrome / Playwright
// subsystems call the global log.Printf during an active session (e.g. on-demand
// MCP (re)connects), and any write to stderr tears the alt-screen render — the
// symptom is a garbled input bar with log lines bleeding into it. The daemon
// path already redirects logs (cmd/daemon.go); the interactive TUI path did not.
//
// The returned func reverts the log writer to its prior destination and closes
// the file, so errors printed after the TUI exits still reach stderr.
func RedirectLogs(shannonDir string) (func(), error) {
	logPath := filepath.Join(shannonDir, "logs", "tui.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	prev := log.Writer()
	log.SetOutput(f)
	return func() {
		log.SetOutput(prev)
		_ = f.Close()
	}, nil
}
