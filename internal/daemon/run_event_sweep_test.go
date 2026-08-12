package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSweepRunEventsCoversDefaultAndNamedAgentScopes(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-48 * time.Hour)
	paths := []string{
		filepath.Join(root, "sessions", ".run-events", "sess-default", "run1_0123456789abcdef0123456789abcdef", "att1_0123456789abcdef0123456789abcdef.jsonl"),
		filepath.Join(root, "agents", "writer", "sessions", ".run-events", "sess-agent", "run1_1123456789abcdef0123456789abcdef", "att1_1123456789abcdef0123456789abcdef.incomplete"),
	}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("stale\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := sweepRunEvents(root, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != len(paths) {
		t.Fatalf("removed = %d, want %d", removed, len(paths))
	}
	for _, path := range paths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("stale run-event file remains %s: %v", path, err)
		}
	}
}

func TestSweepRunEventsDisabledIsNoop(t *testing.T) {
	removed, err := sweepRunEvents(t.TempDir(), 0)
	if err != nil || removed != 0 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
}
