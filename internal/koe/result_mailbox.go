//go:build darwin && cgo

package koe

import (
	"strings"
	"sync"
)

// ResultMailbox owns completed do_task speech independently of a Realtime
// connection. A warm-session teardown may kill an eventHandler while its detached
// do_task goroutine is still running; keeping the result here lets the next
// handler finish delivery instead of sending into the dead data channel.
//
// Entries stay in the mailbox until the result response reaches response.done.
// response.created is only an acceptance acknowledgement: removing an entry there
// would lose it if playback is cancelled or the connection closes mid-response.
type ResultMailbox struct {
	mu      sync.Mutex
	nextID  uint64
	entries []resultMailboxEntry
	notify  chan struct{}
}

type resultMailboxEntry struct {
	id         uint64
	taskID     string
	text       string
	resumptive bool
	revision   bool
	owner      string
}

type resultAnnouncement struct {
	id         uint64
	taskID     string
	text       string
	resumptive bool
	revision   bool
}

func NewResultMailbox() *ResultMailbox {
	return &ResultMailbox{notify: make(chan struct{}, 1)}
}

// Enqueue records a result before waking a sender. The wake is deliberately
// edge-triggered and lossy; the result is not. Queue saturation therefore cannot
// discard completed work.
func (m *ResultMailbox) Enqueue(taskID, text string, resumptive, revision bool) uint64 {
	if m == nil {
		return 0
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}
	m.mu.Lock()
	m.nextID++
	id := m.nextID
	m.entries = append(m.entries, resultMailboxEntry{
		id: id, taskID: taskID, text: text, resumptive: resumptive, revision: revision,
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
	if m == nil || owner == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []resultAnnouncement
	for i := range m.entries {
		entry := &m.entries[i]
		if entry.owner != "" {
			continue
		}
		entry.owner = owner
		out = append(out, resultAnnouncement{
			id: entry.id, taskID: entry.taskID, text: entry.text,
			resumptive: entry.resumptive, revision: entry.revision,
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
