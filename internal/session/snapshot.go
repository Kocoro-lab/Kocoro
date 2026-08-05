package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// compactionSnapshotDirName holds per-session pre-compaction history copies
// under the sessions directory (hidden, like .in-progress). A compaction
// replaces Session.Messages irreversibly — once the summary lands and the
// session checkpoints, the dropped middle is gone. These snapshots are the
// rollback material for junk summaries, lost identifiers, and content the
// summary-quality audit could not see (already-microcompacted tool results).
const compactionSnapshotDirName = ".compaction-snapshots"

// CompactionSnapshot is the on-disk shape of one pre-compaction history copy.
type CompactionSnapshot struct {
	SchemaVersion int              `json:"schema_version"`
	SessionID     string           `json:"session_id"`
	Phase         string           `json:"phase"`
	CreatedAt     time.Time        `json:"created_at"`
	Messages      []client.Message `json:"messages"`
}

// SaveCompactionSnapshot persists the full pre-compaction message history for
// session id, then prunes the session's snapshot dir to the maxPerSession
// newest files. maxPerSession <= 0 disables snapshotting (silent no-op).
// Write is temp+rename so a crash never leaves a truncated snapshot behind
// with a valid name; a stale .tmp file is inert and swept with its session dir.
func (s *Store) SaveCompactionSnapshot(id, phase string, messages []client.Message, maxPerSession int) error {
	if maxPerSession <= 0 {
		return nil
	}
	// Same traversal rules as safeSessionPath — the id may be derived rather
	// than handler-validated on internal call paths.
	if _, err := s.safeSessionPath(id); err != nil {
		return err
	}
	if phase == "" {
		phase = "unknown"
	}

	dir := filepath.Join(s.dir, compactionSnapshotDirName, id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create snapshot dir: %w", err)
	}

	// nextUpdatedAt is strictly monotonic per Store, so filenames sort by
	// creation order even on low-resolution clocks.
	now := s.nextUpdatedAt()
	snap := CompactionSnapshot{
		SchemaVersion: 1,
		SessionID:     id,
		Phase:         phase,
		CreatedAt:     now,
		Messages:      messages,
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal snapshot: %w", err)
	}

	name := fmt.Sprintf("%020d-%s.json", now.UnixNano(), phase)
	tmp := filepath.Join(dir, name+".tmp")
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("write snapshot: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("finalize snapshot: %w", err)
	}

	pruneCompactionSnapshots(dir, maxPerSession)
	return nil
}

// pruneCompactionSnapshots removes the oldest snapshot files beyond keep.
// Best-effort: prune failures must never surface into the compaction path.
func pruneCompactionSnapshots(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) <= keep {
		return
	}
	sort.Strings(names) // zero-padded UnixNano prefix → lexicographic == chronological
	for _, n := range names[:len(names)-keep] {
		os.Remove(filepath.Join(dir, n))
	}
}

// SaveCompactionSnapshot on the Manager delegates to the underlying store.
func (m *Manager) SaveCompactionSnapshot(id, phase string, messages []client.Message, maxPerSession int) error {
	return m.store.SaveCompactionSnapshot(id, phase, messages, maxPerSession)
}
