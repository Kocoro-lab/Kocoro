package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// AuditLogger is a structured logger sink. Satisfied by *audit.Logger in
// production; tests provide a capturing stub.
type AuditLogger interface {
	Log(event string, fields map[string]any)
}

// Deps groups the runtime collaborators required by Run. Now is injectable so
// tests can pin time without freezing the whole package.
type Deps struct {
	Cfg       Config
	HomeDir   string           // ~/.shannon
	ClientVer string           // for SyncBatchRequest.ClientVersion
	Uploader  Uploader
	Loader    SessionLoader
	Audit      AuditLogger
	Now        func() time.Time
	OnSyncDone func() // optional: called when ≥1 session was accepted; wakes memory bundle puller
}

// Run executes one sync iteration: read marker, scan candidates, build batches,
// upload, apply per-session ack/reject, write marker, audit.
//
// Concurrent CLI + daemon callers are serialized via an exclusive flock on
// ~/.shannon/sync.lock. On contention timeout we emit an OutcomeNoop audit
// event and return cleanly — losing the race is not an error.
func Run(ctx context.Context, deps Deps) error {
	now := time.Now().UTC()
	if deps.Now != nil {
		now = deps.Now()
	}

	markerPath := filepath.Join(deps.HomeDir, "sync_marker.json")

	lockPath := filepath.Join(deps.HomeDir, "sync.lock")
	release, err := acquireFlock(ctx, lockPath, deps.Cfg.LockTimeout)
	if err != nil {
		audit(deps.Audit, "session_sync", map[string]any{
			"outcome":      OutcomeNoop,
			"reason":       "lock_contention",
			"skipped_dirs": 0,
		})
		return nil // not an error; another caller is running
	}
	defer release()

	marker, err := ReadMarker(markerPath)
	if err != nil {
		// ReadMarker swallows missing/corrupt/version-mismatch into emptyMarker,
		// so any error here is unrecoverable I/O — fail the run.
		return fmt.Errorf("read marker: %w", err)
	}

	if !deps.Cfg.Enabled {
		audit(deps.Audit, "session_sync", map[string]any{
			"outcome":      OutcomeNoop,
			"reason":       "sync.enabled=false",
			"skipped_dirs": 0,
		})
		return nil
	}

	cands, skipped, err := DiscoverCandidates(ctx, ScannerDeps{HomeDir: deps.HomeDir}, deps.Cfg, marker, now)
	if err != nil {
		return fmt.Errorf("discover candidates: %w", err)
	}

	if len(cands) == 0 {
		marker.LastSyncOutcome = OutcomeNoop
		marker.LastSyncCount = 0
		if err := WriteMarkerAtomic(markerPath, marker); err != nil {
			return fmt.Errorf("write marker (noop): %w", err)
		}
		audit(deps.Audit, "session_sync", map[string]any{
			"outcome":      OutcomeNoop,
			"reason":       "no candidates",
			"skipped_dirs": skipped,
		})
		return nil
	}

	batches, err := BuildBatches(ctx, cands, deps.Loader, deps.Cfg, &marker, now)
	if err != nil {
		return fmt.Errorf("build batches: %w", err)
	}

	totalAccepted := 0
	totalRejectedTransient := 0
	totalRejectedPermanent := 0
	sentCount := 0
	outcome := OutcomeOK
	transportErr := false

	// id → MarshaledSession lookup so per-id ack/reject can pull the candidate's
	// UpdatedAt for marker advance and the no-churn rule.
	byID := map[string]MarshaledSession{}
	for _, b := range batches {
		for _, s := range b.Sessions {
			byID[s.SessionID] = s
		}
	}

	for _, batch := range batches {
		req := client.SyncBatchRequest{
			ClientVersion: deps.ClientVer,
			SyncAt:        now,
			Sessions:      make([]client.SessionEnvelope, 0, len(batch.Sessions)),
		}
		for _, s := range batch.Sessions {
			req.Sessions = append(req.Sessions, client.SessionEnvelope{
				AgentName: s.AgentName,
				Session:   s.JSON,
			})
		}

		sentCount += len(req.Sessions)
		resp, err := deps.Uploader.Send(ctx, req)
		if err != nil {
			// Transport error: stop sending more batches, but keep advances from
			// already-accepted batches. Marker writes below still happen so the
			// next run resumes from where we got to.
			transportErr = true
			outcome = OutcomeTransportError
			break
		}

		for _, id := range resp.Accepted {
			totalAccepted++
			if ms, ok := byID[id]; ok {
				if ms.Candidate.UpdatedAt.After(marker.LastSyncAt) {
					marker.LastSyncAt = ms.Candidate.UpdatedAt
				}
			}
			delete(marker.Failed, id)
		}
		for _, r := range resp.Rejected {
			cat := ClassifyReason(r.Reason)
			if cat == CategoryTransient {
				totalRejectedTransient++
			} else {
				totalRejectedPermanent++
			}
			recordRejection(&marker, r.ID, r.Reason, byID, now, deps.Cfg.FailedMaxAttemptsTransient)
			if outcome == OutcomeOK {
				outcome = OutcomePartial
			}
		}
	}

	marker.LastSyncCount = totalAccepted
	marker.LastSyncOutcome = outcome
	if err := WriteMarkerAtomic(markerPath, marker); err != nil {
		return fmt.Errorf("write marker: %w", err)
	}

	audit(deps.Audit, "session_sync", map[string]any{
		"sent":               sentCount,
		"accepted":           totalAccepted,
		"rejected_transient": totalRejectedTransient,
		"rejected_permanent": totalRejectedPermanent,
		"failed_carryover":   len(marker.Failed),
		"outcome":            outcome,
		"transport_error":    transportErr,
		"skipped_dirs":       skipped,
	})
	if totalAccepted > 0 && deps.OnSyncDone != nil {
		deps.OnSyncDone()
	}
	return nil
}

// recordRejection merges a per-session reject into marker.Failed. Permanent
// reasons pin NextAttemptAt = nil (no auto-retry); transient reasons schedule
// the next attempt via NextTransientAttemptAt and drop the entry once the
// transient cap is hit. byID supplies the candidate's UpdatedAt so the
// no-churn rule on the scanner side can detect future local edits.
func recordRejection(m *Marker, id, reason string, byID map[string]MarshaledSession, now time.Time, transientCap int) {
	if m.Failed == nil {
		m.Failed = map[string]FailedEntry{}
	}
	prev, existed := m.Failed[id]
	cat := ClassifyReason(reason)
	if !existed {
		prev = FailedEntry{
			Reason:         reason,
			Category:       cat,
			Attempts:       0,
			FirstAttemptAt: now,
		}
	}
	prev.Reason = reason
	prev.Category = cat
	prev.Attempts++
	prev.LastAttemptAt = now
	if ms, ok := byID[id]; ok {
		prev.SizeBytes = ms.SizeBytes
		prev.LastObservedUpdatedAt = ms.Candidate.UpdatedAt
	}
	if cat == CategoryPermanent {
		prev.NextAttemptAt = nil
	} else {
		// Transient: drop if past cap; else schedule next attempt.
		if transientCap > 0 && prev.Attempts >= transientCap {
			delete(m.Failed, id)
			return
		}
		nxt := NextTransientAttemptAt(prev.Attempts, now)
		prev.NextAttemptAt = &nxt
	}
	m.Failed[id] = prev
}

// audit is a nil-safe wrapper so callers don't have to gate every Log call.
func audit(a AuditLogger, event string, fields map[string]any) {
	if a == nil {
		return
	}
	a.Log(event, fields)
}

// acquireFlock takes an exclusive flock on path, blocking up to timeout
// or until ctx is canceled (whichever fires first). The lock file is never
// deleted (a deletion would race with a concurrent Open on a different inode
// — same warning as internal/schedule/schedule.go).
func acquireFlock(ctx context.Context, path string, timeout time.Duration) (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}

	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("flock: %w", err)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("flock contention: %w", err)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
