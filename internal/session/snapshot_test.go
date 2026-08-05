package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func snapshotFiles(t *testing.T, dir, id string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, compactionSnapshotDirName, id))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read snapshot dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestStore_SaveCompactionSnapshot_WritesAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	msgs := []client.Message{
		{Role: "user", Content: client.NewTextContent("original request with COMMIT_abc123def456abc9")},
		{Role: "assistant", Content: client.NewTextContent("middle turn about to be dropped")},
	}
	if err := s.SaveCompactionSnapshot("sess-1", "proactive", msgs, 3); err != nil {
		t.Fatalf("SaveCompactionSnapshot: %v", err)
	}

	files := snapshotFiles(t, dir, "sess-1")
	if len(files) != 1 {
		t.Fatalf("expected 1 snapshot file, got %d: %v", len(files), files)
	}
	raw, err := os.ReadFile(filepath.Join(dir, compactionSnapshotDirName, "sess-1", files[0]))
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var snap CompactionSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	if snap.SessionID != "sess-1" || snap.Phase != "proactive" {
		t.Errorf("metadata mismatch: %+v", snap)
	}
	if len(snap.Messages) != 2 || snap.Messages[0].Content.Text() != msgs[0].Content.Text() {
		t.Errorf("messages did not round-trip: %+v", snap.Messages)
	}
	if snap.CreatedAt.IsZero() {
		t.Error("CreatedAt must be stamped")
	}
}

func TestStore_SaveCompactionSnapshot_PrunesOldest(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	for i, phase := range []string{"proactive", "preflight", "reactive", "force_stop"} {
		msgs := []client.Message{{Role: "user", Content: client.NewTextContent(phase)}}
		if err := s.SaveCompactionSnapshot("sess-2", phase, msgs, 3); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}

	files := snapshotFiles(t, dir, "sess-2")
	if len(files) != 3 {
		t.Fatalf("expected retention to keep 3 snapshots, got %d: %v", len(files), files)
	}
	// The oldest ("proactive") must be gone; the newest ("force_stop") kept.
	joined := ""
	for _, f := range files {
		joined += f + " "
	}
	if !contains(files, "force_stop") {
		t.Errorf("newest snapshot missing: %s", joined)
	}
	if contains(files, "proactive") {
		t.Errorf("oldest snapshot must be pruned: %s", joined)
	}
}

func contains(names []string, phase string) bool {
	for _, n := range names {
		if len(n) > len(phase) && n[len(n)-len(phase)-5:len(n)-5] == phase {
			return true
		}
	}
	return false
}

func TestStore_SaveCompactionSnapshot_RejectsBadID(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	for _, id := range []string{"", ".", "..", "../evil", "a/b", `a\b`} {
		if err := s.SaveCompactionSnapshot(id, "proactive", nil, 3); err == nil {
			t.Errorf("id %q must be rejected", id)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, compactionSnapshotDirName)); !os.IsNotExist(err) {
		t.Error("no snapshot dir should be created for rejected ids")
	}
}

func TestStore_SaveCompactionSnapshot_ZeroRetentionDisables(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	msgs := []client.Message{{Role: "user", Content: client.NewTextContent("x")}}
	if err := s.SaveCompactionSnapshot("sess-3", "proactive", msgs, 0); err != nil {
		t.Fatalf("zero retention must be a silent no-op: %v", err)
	}
	if files := snapshotFiles(t, dir, "sess-3"); len(files) != 0 {
		t.Errorf("expected no snapshot with retention 0, got %v", files)
	}
}
