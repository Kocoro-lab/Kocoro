package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	// The phase lands in a filename; the exported method cannot assume its
	// callers stick to the loop's literals, so anything outside the expected
	// vocabulary collapses to "unknown" rather than reaching filepath.Join.
	if !isSafePhase(phase) {
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

// isSafePhase reports whether phase is a plain lowercase/underscore token —
// the loop's compaction phase vocabulary — and thus safe to embed in a
// snapshot filename.
func isSafePhase(phase string) bool {
	if phase == "" {
		return false
	}
	for _, r := range phase {
		if (r < 'a' || r > 'z') && r != '_' {
			return false
		}
	}
	return true
}

// pruneCompactionSnapshots bounds the snapshot dir to keep files. The OLDEST
// snapshot is pinned and rotation evicts from the second-oldest up: the
// pre-first-compaction snapshot is the only one whose tool results have not
// already been micro-compacted or tier-1'd — evicting it first would discard
// the most content-rich copy while keeping three already-degraded ones.
// Stray .tmp files (crash between write and rename) are removed here too;
// they are inert but nothing else ever sweeps this directory.
// Best-effort: prune failures must never surface into the compaction path.
func pruneCompactionSnapshots(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".tmp") {
			os.Remove(filepath.Join(dir, e.Name()))
			continue
		}
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) <= keep {
		return
	}
	sort.Strings(names) // zero-padded UnixNano prefix → lexicographic == chronological
	evictable := names[1:] // pin the oldest
	excess := len(names) - keep
	for _, n := range evictable[:excess] {
		os.Remove(filepath.Join(dir, n))
	}
}

// SweepOrphanCompactionSnapshots removes snapshot directories whose session
// JSON no longer exists (deleted before snapshot cleanup shipped, or removed
// out of band). Called from NewStore so the sweep runs once per process
// start; deterministic — no age heuristic needed, the session's absence IS
// the signal.
func (s *Store) SweepOrphanCompactionSnapshots() {
	root := filepath.Join(s.dir, compactionSnapshotDirName)
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sessPath, err := s.safeSessionPath(e.Name())
		if err != nil {
			continue // never derive a delete target from an unsafe name
		}
		if _, err := os.Stat(sessPath); os.IsNotExist(err) {
			os.RemoveAll(filepath.Join(root, e.Name()))
		}
	}
}

// SaveCompactionSnapshot on the Manager delegates to the underlying store.
func (m *Manager) SaveCompactionSnapshot(id, phase string, messages []client.Message, maxPerSession int) error {
	return m.store.SaveCompactionSnapshot(id, phase, messages, maxPerSession)
}
