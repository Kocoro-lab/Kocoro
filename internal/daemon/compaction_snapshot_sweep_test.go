package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSweepCompactionSnapshotsCoversDefaultAndNamedAgentScopes(t *testing.T) {
	shannonDir := t.TempDir()
	defaultOld := filepath.Join(shannonDir, "sessions", ".compaction-snapshots", "sess-default", "old.json")
	agentOld := filepath.Join(shannonDir, "agents", "researcher", "sessions", ".compaction-snapshots", "sess-agent", "old.json")
	agentFresh := filepath.Join(shannonDir, "agents", "researcher", "sessions", ".compaction-snapshots", "sess-agent", "fresh.json")
	for _, path := range []string{defaultOld, agentOld, agentFresh} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-48 * time.Hour)
	for _, path := range []string{defaultOld, agentOld} {
		if err := os.Chtimes(path, past, past); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := sweepCompactionSnapshots(shannonDir, 24*time.Hour)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	for _, path := range []string{defaultOld, agentOld} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expired snapshot still exists: %s", path)
		}
	}
	if _, err := os.Stat(agentFresh); err != nil {
		t.Fatalf("fresh named-agent snapshot was removed: %v", err)
	}
}

func TestSweepCompactionSnapshotsDisabledIsNoop(t *testing.T) {
	removed, err := sweepCompactionSnapshots(t.TempDir(), 0)
	if err != nil || removed != 0 {
		t.Fatalf("removed=%d err=%v, want disabled no-op", removed, err)
	}
}
