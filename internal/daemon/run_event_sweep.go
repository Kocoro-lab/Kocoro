package daemon

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

func (s *Server) startRunEventSweep(days int) {
	if s == nil || s.deps == nil || days <= 0 {
		return
	}
	go func() {
		if removed, err := sweepRunEvents(s.deps.ShannonDir, time.Duration(days)*24*time.Hour); err != nil {
			log.Printf("daemon: run event sweep failed: %v", err)
		} else if removed > 0 {
			log.Printf("daemon: run event sweep removed %d stale files (older than %dd)", removed, days)
		}
	}()
}

// sweepRunEvents applies the observation-log age bound to the default session
// scope and every named-agent scope. Session managers are lazy, so startup
// must walk the on-disk roots rather than only currently active routes.
func sweepRunEvents(shannonDir string, maxAge time.Duration) (int, error) {
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
		count, err := session.SweepStaleRunEvents(root, maxAge)
		if err != nil {
			return removed, fmt.Errorf("sweep %s: %w", root, err)
		}
		removed += count
	}
	return removed, nil
}
