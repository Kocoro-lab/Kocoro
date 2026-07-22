//go:build darwin && cgo

package koe

import (
	"sync"
)

// ResultMailbox owns completed do_task speech independently of a Realtime
// connection but never independently of its originating call. A replacement
// handler may recover delivery only for the same burst; ending the call retires
// that burst so a later Option-key activation starts with a clean voice state.
//
// Entries stay in the mailbox until the result response reaches response.done.
// response.created is only an acceptance acknowledgement: removing an entry there
// would lose it if playback is cancelled or the connection closes mid-response.
type ResultMailbox struct {
	mu           sync.Mutex
	nextID       uint64
	entries      []resultMailboxEntry
	activeBursts map[string]struct{}
	notify       chan struct{}
}

type resultMailboxEntry struct {
	id         uint64
	burstID    string
	result     SayResult
	resumptive bool
	owner      string
}

type resultAnnouncement struct {
	id         uint64
	result     SayResult
	resumptive bool
}

func NewResultMailbox() *ResultMailbox {
	return &ResultMailbox{
		activeBursts: make(map[string]struct{}),
		notify:       make(chan struct{}, 1),
	}
}

// BeginBurst opens voice delivery for one call. A burst is the hard boundary
// created by an Option-key activation: completed work may persist elsewhere,
// but only this exact call may claim its spoken result.
func (m *ResultMailbox) BeginBurst(burstID string) {
	if m == nil || burstID == "" {
		return
	}
	m.mu.Lock()
	m.activeBursts[burstID] = struct{}{}
	m.mu.Unlock()
}

// RetireBurst closes a call's voice-delivery scope and drops any speech that was
// still queued for it. Later results from detached do_task goroutines are also
// rejected because the burst is no longer active.
func (m *ResultMailbox) RetireBurst(burstID string) int {
	if m == nil || burstID == "" {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.activeBursts, burstID)
	kept := m.entries[:0]
	removed := 0
	for _, entry := range m.entries {
		if entry.burstID == burstID {
			removed++
			continue
		}
		kept = append(kept, entry)
	}
	m.entries = kept
	return removed
}

// Enqueue records a result before waking a sender. The wake is deliberately
// edge-triggered and lossy; the result is not. Queue saturation therefore cannot
// discard completed work.
func (m *ResultMailbox) Enqueue(result SayResult, resumptive bool) uint64 {
	return m.enqueue("", result, resumptive)
}

// EnqueueForBurst records speech only while its originating call still owns a
// live voice scope. The task result itself remains persisted by the daemon even
// when this returns zero after a hang-up.
func (m *ResultMailbox) EnqueueForBurst(burstID string, result SayResult, resumptive bool) uint64 {
	return m.enqueue(burstID, result, resumptive)
}

func (m *ResultMailbox) enqueue(burstID string, result SayResult, resumptive bool) uint64 {
	if m == nil {
		return 0
	}
	if result.Reply == "" && result.Say == "" && len(result.Deliverables) == 0 {
		return 0
	}
	result.Deliverables = append([]Deliverable(nil), result.Deliverables...)
	m.mu.Lock()
	if burstID != "" {
		if _, active := m.activeBursts[burstID]; !active {
			m.mu.Unlock()
			return 0
		}
	}
	m.nextID++
	id := m.nextID
	m.entries = append(m.entries, resultMailboxEntry{
		id: id, burstID: burstID, result: result, resumptive: resumptive,
	})
	m.mu.Unlock()
	m.Wake()
	return id
}

// Wake asks the active handler to inspect the mailbox. It is safe to call when no
// handler is active; every newly attached handler also calls Wake once.
func (m *ResultMailbox) Wake() {
	if m == nil {
		return
	}
	select {
	case m.notify <- struct{}{}:
	default:
	}
}

func (m *ResultMailbox) notifications() <-chan struct{} {
	if m == nil {
		return nil
	}
	return m.notify
}

// claim transfers every currently-pending entry to one connection owner. Only
// one task-result response is in flight per handler, so owner is sufficient as a
// lease key; a connection teardown releases all of its entries atomically.
func (m *ResultMailbox) claim(owner string) []resultAnnouncement {
	return m.claimForBurst(owner, "")
}

func (m *ResultMailbox) claimForBurst(owner, burstID string) []resultAnnouncement {
	if m == nil || owner == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []resultAnnouncement
	for i := range m.entries {
		entry := &m.entries[i]
		// Empty burst IDs are retained only for standalone/test compatibility.
		// Production do_task results are always scoped and must match exactly.
		if entry.owner != "" || (entry.burstID != "" && entry.burstID != burstID) {
			continue
		}
		entry.owner = owner
		result := entry.result
		result.Deliverables = append([]Deliverable(nil), entry.result.Deliverables...)
		out = append(out, resultAnnouncement{
			id: entry.id, result: result, resumptive: entry.resumptive,
		})
	}
	return out
}

// complete removes only entries held by owner. It is called after a completed
// response.done, which is the delivery acknowledgement for this in-memory plane.
func (m *ResultMailbox) complete(owner string) int {
	if m == nil || owner == "" {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.entries[:0]
	removed := 0
	for _, entry := range m.entries {
		if entry.owner == owner {
			removed++
			continue
		}
		kept = append(kept, entry)
	}
	m.entries = kept
	return removed
}

// release returns an owner's in-flight entries to pending. The caller decides
// whether to Wake immediately: connection teardown relies on the next handler's
// startup wake, while a cancelled response can retry after the user yields.
func (m *ResultMailbox) release(owner string) int {
	if m == nil || owner == "" {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	released := 0
	for i := range m.entries {
		if m.entries[i].owner == owner {
			m.entries[i].owner = ""
			released++
		}
	}
	return released
}

func (m *ResultMailbox) pending() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}
