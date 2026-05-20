package agenttypes

import (
	"errors"
	"sync"
)

// ErrMailboxFull is returned by Mailbox.Enqueue when the per-route capacity is
// reached. Callers should map this to a transport-appropriate "queue full"
// status (HTTP 503, WS no-ack-so-Cloud-replays).
var ErrMailboxFull = errors.New("mailbox full")

// Mailbox is an in-memory priority deque with a fixed capacity cap. Safe for
// concurrent use. Persistence is owned externally (internal/daemon stores
// rows to SQLite); a Mailbox itself is pure in-memory state and is rebuilt
// from storage on recovery.
//
// Ordering: items dequeue in (priority ASC, enqueue-order ASC) order, where
// enqueue-order is preserved within a priority bucket by insertion-sort on
// Enqueue. The capacity cap is hard — exceeding it returns ErrMailboxFull
// without partial mutation.
type Mailbox struct {
	mu       sync.Mutex
	items    []QueuedMessage
	capacity int
}

// NewMailbox returns a Mailbox with the given capacity cap. A non-positive
// capacity falls back to 100, which is the daemon's default
// (daemon.mailbox_max_per_route).
func NewMailbox(capacity int) *Mailbox {
	if capacity <= 0 {
		capacity = 100
	}
	return &Mailbox{
		items:    make([]QueuedMessage, 0, 4),
		capacity: capacity,
	}
}

// Enqueue inserts msg respecting priority + FIFO ordering.
// Returns (false, ErrMailboxFull) when the capacity cap would be exceeded;
// the mailbox is left unchanged in that case.
func (m *Mailbox) Enqueue(msg QueuedMessage) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.items) >= m.capacity {
		return false, ErrMailboxFull
	}
	idx := len(m.items)
	for i, it := range m.items {
		if it.Priority > msg.Priority {
			idx = i
			break
		}
	}
	m.items = append(m.items, QueuedMessage{})
	copy(m.items[idx+1:], m.items[idx:])
	m.items[idx] = msg
	return true, nil
}

// DequeueBatch removes and returns up to limit messages in priority/FIFO order.
// Returns an empty slice when the mailbox is empty.
func (m *Mailbox) DequeueBatch(limit int) []QueuedMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit > len(m.items) {
		limit = len(m.items)
	}
	if limit <= 0 {
		return nil
	}
	batch := make([]QueuedMessage, limit)
	copy(batch, m.items[:limit])
	m.items = m.items[limit:]
	return batch
}

// Retract removes the message with the given ID. Returns true if found.
func (m *Mailbox) Retract(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, it := range m.items {
		if it.ID == id {
			m.items = append(m.items[:i], m.items[i+1:]...)
			return true
		}
	}
	return false
}

// Snapshot returns a defensive copy of the current queue for UI display.
// Mutations to the returned slice do not affect the mailbox.
func (m *Mailbox) Snapshot() []QueuedMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]QueuedMessage, len(m.items))
	copy(out, m.items)
	return out
}

// Len returns the current queue length.
func (m *Mailbox) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.items)
}

// SeedFromStore prepends the given persisted messages to the mailbox. Used by
// daemon startup recovery to restore in-flight rows from SQLite. Existing
// in-memory items (typically none, since recovery runs before any live
// inject) are pushed after the seed.
//
// Capacity is honored: messages beyond the cap are dropped on the floor and
// the dropped count is returned so the caller can log it.
func (m *Mailbox) SeedFromStore(msgs []QueuedMessage) (loaded int, dropped int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing := m.items
	m.items = make([]QueuedMessage, 0, len(msgs)+len(existing))
	for _, msg := range msgs {
		if len(m.items) >= m.capacity {
			dropped++
			continue
		}
		m.items = append(m.items, msg)
		loaded++
	}
	for _, it := range existing {
		if len(m.items) >= m.capacity {
			dropped++
			continue
		}
		m.items = append(m.items, it)
	}
	return loaded, dropped
}
