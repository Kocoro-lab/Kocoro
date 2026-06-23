package tui

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRedirectLogs_WritesToFileNotStderr is the regression guard for the
// input-bar corruption bug: MCP/Chrome subsystem log.Printf calls must land in
// a file, not on stderr (which would tear the Bubbletea alt-screen render).
func TestRedirectLogs_WritesToFileNotStderr(t *testing.T) {
	dir := t.TempDir()

	restore, err := RedirectLogs(dir)
	if err != nil {
		t.Fatalf("RedirectLogs: %v", err)
	}
	defer restore()

	log.Printf("Playwright MCP connected — marker_%d", 42)

	data, err := os.ReadFile(filepath.Join(dir, "logs", "tui.log"))
	if err != nil {
		t.Fatalf("read tui.log: %v", err)
	}
	if !strings.Contains(string(data), "marker_42") {
		t.Errorf("log line not captured in tui.log; file contents:\n%s", data)
	}
}

// TestRedirectLogs_RestoreRevertsWriter confirms the returned restore func puts
// the log writer back (so post-TUI errors still reach stderr).
func TestRedirectLogs_RestoreRevertsWriter(t *testing.T) {
	dir := t.TempDir()
	orig := log.Writer()

	restore, err := RedirectLogs(dir)
	if err != nil {
		t.Fatalf("RedirectLogs: %v", err)
	}
	if log.Writer() == orig {
		t.Fatal("RedirectLogs did not change the log writer")
	}

	restore()
	if log.Writer() != orig {
		t.Error("restore did not revert the log writer to its original")
	}
}
