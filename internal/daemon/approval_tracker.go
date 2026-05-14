package daemon

import "sync"

// ApprovalTracker tracks which sessions are currently blocked on a user
// approval prompt. The agent loop drives this via Mark/Clear at the moment
// it calls ApprovalBroker.Request; the daemon HTTP layer reads it to expose
// an "awaiting_approval" flag on SessionSummary and through GET /approvals.
//
// A single session may have multiple parallel approval requests in flight
// (one per concurrent tool call), so entries are reference-counted instead
// of boolean — a single Clear must not lie that the session is unblocked
// while a sibling tool call is still waiting.
type ApprovalTracker struct {
	mu     sync.RWMutex
	counts map[string]int // sessionID → pending approval count
}

func NewApprovalTracker() *ApprovalTracker {
	return &ApprovalTracker{counts: make(map[string]int)}
}

// Mark records that sessionID has opened an approval prompt. No-op for empty
// sessionID so non-routed paths (e.g. /research, /swarm) silently skip.
func (t *ApprovalTracker) Mark(sessionID string) {
	if t == nil || sessionID == "" {
		return
	}
	t.mu.Lock()
	t.counts[sessionID]++
	t.mu.Unlock()
}

// Clear decrements the pending count for sessionID; when the count reaches
// zero the entry is removed so the listing returns to a stable size. Calling
// Clear without a matching Mark is a no-op (defensive — the broker timeout
// path should never under-count, but the contract stays robust either way).
func (t *ApprovalTracker) Clear(sessionID string) {
	if t == nil || sessionID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.counts[sessionID]
	if n <= 1 {
		delete(t.counts, sessionID)
		return
	}
	t.counts[sessionID] = n - 1
}

// IsAwaiting reports whether sessionID currently has any pending approval.
func (t *ApprovalTracker) IsAwaiting(sessionID string) bool {
	if t == nil || sessionID == "" {
		return false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.counts[sessionID] > 0
}

// SessionIDs returns the set of session IDs currently awaiting approval.
// Returns nil (not a zero-length slice) when nothing is pending; HTTP
// callers must normalize nil → []string{} before encoding to keep the
// daemon's `{"sessions": []}` empty-list convention (see handleApprovals).
func (t *ApprovalTracker) SessionIDs() []string {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if len(t.counts) == 0 {
		return nil
	}
	out := make([]string, 0, len(t.counts))
	for id := range t.counts {
		out = append(out, id)
	}
	return out
}

// AwaitingSet returns a map[sessionID]struct{} for O(1) membership checks
// when enriching session listings. The returned map is a snapshot copy —
// safe to retain and iterate; mutating it has no effect on the tracker.
func (t *ApprovalTracker) AwaitingSet() map[string]struct{} {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if len(t.counts) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(t.counts))
	for id := range t.counts {
		out[id] = struct{}{}
	}
	return out
}
