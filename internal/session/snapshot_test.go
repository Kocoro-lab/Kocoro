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

func TestStore_SaveCompactionSnapshot_PinsOldestAndPrunesRest(t *testing.T) {
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
	joined := ""
	for _, f := range files {
		joined += f + " "
	}
	// The OLDEST snapshot is pinned — it is the only one whose tool results
	// predate every micro-compact pass. Rotation evicts from the second-
	// oldest up, so "preflight" goes and "proactive"+newest two stay.
	if !contains(files, "proactive") {
		t.Errorf("oldest (pre-first-compaction) snapshot must be pinned: %s", joined)
	}
	if !contains(files, "force_stop") {
		t.Errorf("newest snapshot missing: %s", joined)
	}
	if contains(files, "preflight") {
		t.Errorf("second-oldest snapshot must be evicted: %s", joined)
	}
}

func TestStore_SaveCompactionSnapshot_SanitizesPhaseAndSweepsTmp(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	msgs := []client.Message{{Role: "user", Content: client.NewTextContent("x")}}

	if err := s.SaveCompactionSnapshot("sess-p", "../Evil Phase", msgs, 3); err != nil {
		t.Fatalf("save: %v", err)
	}
	files := snapshotFiles(t, dir, "sess-p")
	if len(files) != 1 || !contains(files, "unknown") {
		t.Fatalf("unsafe phase must collapse to 'unknown' in the filename, got %v", files)
	}

	// A stray .tmp (crash between write and rename) is removed by the next
	// save's prune pass — nothing else ever sweeps this directory.
	stray := filepath.Join(dir, compactionSnapshotDirName, "sess-p", "stale.json.tmp")
	if err := os.WriteFile(stray, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCompactionSnapshot("sess-p", "proactive", msgs, 3); err != nil {
		t.Fatalf("save 2: %v", err)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Error("stray .tmp must be swept by the prune pass")
	}
}

func TestStore_Delete_RemovesCompactionSnapshots(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	sess := &Session{ID: "sess-del", Title: "t"}
	if err := s.Save(sess); err != nil {
		t.Fatalf("save session: %v", err)
	}
	msgs := []client.Message{{Role: "user", Content: client.NewTextContent("full pre-drop history")}}
	if err := s.SaveCompactionSnapshot("sess-del", "proactive", msgs, 3); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	if err := s.Delete("sess-del"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	snapDir := filepath.Join(dir, compactionSnapshotDirName, "sess-del")
	if _, err := os.Stat(snapDir); !os.IsNotExist(err) {
		t.Error("deleting a session must remove its snapshot directory — it is the most content-rich copy of the history")
	}
}

func TestNewStore_SweepsOrphanCompactionSnapshots(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	live := &Session{ID: "sess-live", Title: "t"}
	if err := s.Save(live); err != nil {
		t.Fatal(err)
	}
	msgs := []client.Message{{Role: "user", Content: client.NewTextContent("x")}}
	if err := s.SaveCompactionSnapshot("sess-live", "proactive", msgs, 3); err != nil {
		t.Fatal(err)
	}
	// Orphan: snapshot dir with no session JSON (deleted out of band).
	orphan := filepath.Join(dir, compactionSnapshotDirName, "sess-gone")
	if err := os.MkdirAll(orphan, 0700); err != nil {
		t.Fatal(err)
	}

	NewStore(dir) // fresh store: startup sweep runs

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("orphan snapshot dir must be swept at store startup")
	}
	if files := snapshotFiles(t, dir, "sess-live"); len(files) != 1 {
		t.Errorf("live session's snapshots must survive the sweep, got %v", files)
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
