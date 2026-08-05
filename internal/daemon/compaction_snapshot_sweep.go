package daemon

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

func (s *Server) startCompactionSnapshotSweep(days int) {
	if s == nil || s.deps == nil || days <= 0 {
		return
	}
	go func() {
		if removed, err := sweepCompactionSnapshots(s.deps.ShannonDir, time.Duration(days)*24*time.Hour); err != nil {
			log.Printf("daemon: compaction snapshot sweep failed: %v", err)
		} else if removed > 0 {
			log.Printf("daemon: compaction snapshot sweep removed %d stale files (older than %dd)", removed, days)
		}
	}()
}

// sweepCompactionSnapshots applies the snapshot age bound to the default
// session scope and every named-agent scope. Managers are created lazily, so a
// daemon-start sweep must walk the on-disk agent roots rather than relying on
// SessionCache entries that happen to be active.
func sweepCompactionSnapshots(shannonDir string, maxAge time.Duration) (int, error) {
	if shannonDir == "" || maxAge <= 0 {
		return 0, nil
	}
	roots := []string{filepath.Join(shannonDir, "sessions")}
	agentsRoot := filepath.Join(shannonDir, "agents")
	entries, err := os.ReadDir(agentsRoot)
	if err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("read agents directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			roots = append(roots, filepath.Join(agentsRoot, entry.Name(), "sessions"))
		}
	}

	removed := 0
	for _, root := range roots {
		count, err := session.SweepStaleCompactionSnapshots(root, maxAge)
		if err != nil {
			return removed, fmt.Errorf("sweep %s: %w", root, err)
		}
		removed += count
	}
	return removed, nil
}
