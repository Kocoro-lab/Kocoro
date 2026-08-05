package session

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestStore_SaveCompactionSnapshot_StripsAllImagesWithoutMutatingHistory(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	const payload = "snapshot-must-not-persist-this-base64"
	imageBlock := client.ContentBlock{
		Type:   "image",
		Source: &client.ImageSource{Type: "base64", MediaType: "image/png", Data: payload},
	}
	msgs := []client.Message{
		{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "text", Text: "look at this"},
			imageBlock,
		})},
		{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolResultBlockWithImages("tool-1", "screenshot", []client.ContentBlock{imageBlock}, false),
		})},
	}

	if err := s.SaveCompactionSnapshot("sess-images", "proactive", msgs, 1); err != nil {
		t.Fatalf("SaveCompactionSnapshot: %v", err)
	}
	// Snapshot sanitization owns replacement slices; the live history remains
	// available to the active vision turn.
	if got := countSnapshotImages(msgs); got != 2 {
		t.Fatalf("input history mutated: image count = %d, want 2", got)
	}

	files := snapshotFiles(t, dir, "sess-images")
	raw, err := os.ReadFile(filepath.Join(dir, compactionSnapshotDirName, "sess-images", files[0]))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(payload)) {
		t.Fatal("snapshot retained inline image payload")
	}
	var snap CompactionSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	if got := countSnapshotImages(snap.Messages); got != 0 {
		t.Fatalf("snapshot image count = %d, want 0", got)
	}
	if !bytes.Contains(raw, []byte(snapshotImagePlaceholder)) {
		t.Fatal("snapshot should retain a text marker where image evidence was omitted")
	}
}

func countSnapshotImages(messages []client.Message) int {
	count := 0
	var countBlocks func([]client.ContentBlock)
	countBlocks = func(blocks []client.ContentBlock) {
		for _, block := range blocks {
			if block.Type == "image" {
				count++
			}
			if nested, ok := block.ToolContent.([]client.ContentBlock); ok {
				countBlocks(nested)
			}
		}
	}
	for _, message := range messages {
		countBlocks(message.Content.Blocks())
	}
	return count
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

	// NewStore's sweep is gated to once per process per directory (this dir's
	// slot was consumed by the NewStore above), so exercise the sweep
	// directly — the same code path the gate invokes.
	s.SweepOrphanCompactionSnapshots()

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("orphan snapshot dir must be swept")
	}
	if files := snapshotFiles(t, dir, "sess-live"); len(files) != 1 {
		t.Errorf("live session's snapshots must survive the sweep, got %v", files)
	}
}

func TestNewStore_OrphanSweepRunsOncePerDir(t *testing.T) {
	dir := t.TempDir()
	NewStore(dir) // consumes this dir's once-per-process sweep slot

	// An orphan planted AFTER the first construction must survive a second
	// NewStore over the same dir — the per-route Store churn that motivated
	// the gate must not re-run the sweep concurrently with live sessions.
	orphan := filepath.Join(dir, compactionSnapshotDirName, "sess-late-orphan")
	if err := os.MkdirAll(orphan, 0700); err != nil {
		t.Fatal(err)
	}
	NewStore(dir)
	if _, err := os.Stat(orphan); err != nil {
		t.Error("second NewStore over the same dir must NOT re-run the orphan sweep")
	}
}

func TestSweepStaleCompactionSnapshotsRemovesOnlyExpiredSnapshotFiles(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, compactionSnapshotDirName)
	oldOnlyDir := filepath.Join(root, "sess-old-only")
	mixedDir := filepath.Join(root, "sess-mixed")
	for _, snapshotDir := range []string{oldOnlyDir, mixedDir} {
		if err := os.MkdirAll(snapshotDir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	oldJSON := filepath.Join(oldOnlyDir, "0001-proactive.json")
	oldTmp := filepath.Join(mixedDir, "0002-preflight.json.tmp")
	freshJSON := filepath.Join(mixedDir, "0003-reactive.json")
	unexpected := filepath.Join(mixedDir, "keep.txt")
	for _, path := range []string{oldJSON, oldTmp, freshJSON, unexpected} {
		if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-48 * time.Hour)
	for _, path := range []string{oldJSON, oldTmp, unexpected} {
		if err := os.Chtimes(path, past, past); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := SweepStaleCompactionSnapshots(dir, 24*time.Hour)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	if _, err := os.Stat(oldOnlyDir); err != nil {
		t.Fatalf("empty snapshot directory must be retained to avoid racing writers: %v", err)
	}
	for _, path := range []string{freshJSON, unexpected} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("sweep removed retained file %s: %v", path, err)
		}
	}
}

func TestSweepStaleCompactionSnapshotsDisabledOrMissingIsNoop(t *testing.T) {
	dir := t.TempDir()
	for _, age := range []time.Duration{0, 24 * time.Hour} {
		removed, err := SweepStaleCompactionSnapshots(dir, age)
		if err != nil || removed != 0 {
			t.Fatalf("age %v: removed=%d err=%v, want no-op", age, removed, err)
		}
	}
}

func TestStore_SaveCompactionSnapshot_RetentionOneKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	for _, phase := range []string{"proactive", "preflight", "reactive"} {
		msgs := []client.Message{{Role: "user", Content: client.NewTextContent(phase)}}
		if err := s.SaveCompactionSnapshot("sess-one", phase, msgs, 1); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	files := snapshotFiles(t, dir, "sess-one")
	if len(files) != 1 || !contains(files, "reactive") {
		t.Errorf("retention 1 must keep exactly the NEWEST snapshot (no pin), got %v", files)
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
