package daemon

import (
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agenttypes"
)

// MailboxOutcome describes the result of EnqueueMessage. It is wire-distinct
// from the legacy InjectResult: EnqueueMessage is the ack-on-persist path
// (Phase 1) used by cmd/daemon.go and the HTTP /message endpoint; InjectMessage
// remains the in-memory mid-run drain channel path.
type MailboxOutcome int

const (
	// MailboxQueued means the message was persisted (if a store is configured)
	// AND inserted into the in-memory mailbox. Callers MAY ack the source.
	MailboxQueued MailboxOutcome = iota
	// MailboxDeduped means a row with the same (cloud_msg_id, route_key)
	// already exists. Treated as success — caller MAY ack. The mailbox state
	// is unchanged (no double-insert).
	MailboxDeduped
	// MailboxQueueFull means the per-route in-memory capacity was reached.
	// Caller MUST NOT ack; Cloud will replay; HTTP returns 503.
	MailboxQueueFull
	// MailboxPersistFailed means the underlying SQLite append failed.
	// Caller MUST NOT ack. The in-memory mailbox is NOT mutated.
	MailboxPersistFailed
	// MailboxRouteMismatch means the route has an active run with a different
	// CWD; the message would change the run's working directory mid-flight.
	// Caller MUST NOT ack; existing /message handler already maps this to 409.
	MailboxRouteMismatch
)

func (o MailboxOutcome) String() string {
	switch o {
	case MailboxQueued:
		return "queued"
	case MailboxDeduped:
		return "deduped"
	case MailboxQueueFull:
		return "queue_full"
	case MailboxPersistFailed:
		return "persist_failed"
	case MailboxRouteMismatch:
		return "route_mismatch"
	}
	return "unknown"
}

// ShouldAck returns true iff the source transport may ack this outcome.
// Callers MUST honor this — incorrectly acking on a persist-failure or
// queue-full outcome would let Cloud's replay buffer drop a message that
// the daemon never durably committed.
func (o MailboxOutcome) ShouldAck() bool {
	return o == MailboxQueued || o == MailboxDeduped
}

// EnsureMailbox returns the route's mailbox, creating it under sc.mu if
// absent. The route entry itself is created if missing (so the route can
// host queued items even before any run starts — this is the recovery path).
func (sc *SessionCache) EnsureMailbox(key string) *agenttypes.Mailbox {
	if key == "" {
		return nil
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	entry, ok := sc.routes[key]
	if !ok || entry == nil {
		entry = &routeEntry{lastAccess: time.Now()}
		sc.routes[key] = entry
	}
	if entry.mailbox == nil {
		entry.mailbox = agenttypes.NewMailbox(sc.mailboxCap)
	}
	return entry.mailbox
}

// EnqueueMessage is the persist-first enqueue path used by /message and WS
// onMsg. It is the daemon's durable-by-default ack precondition: SQLite append
// must succeed before the in-memory mailbox is touched, and the in-memory
// insert is rolled back from SQLite if the cap is exceeded.
//
// Flow:
//  1. Active-run CWD check (if msg.CWD set + active CWD differs → mismatch).
//  2. mailboxStore.Append (skipped if store == nil; dedup may return inserted=false).
//  3. mailbox.Enqueue with capacity cap.
//  4. If Enqueue fails (cap), delete the SQLite row to keep storage consistent.
//
// Returns (outcome, error). error is non-nil only for genuinely-unexpected
// failures (SQLite IO error). For typed business outcomes (queue full,
// dedup, cwd mismatch), err is nil and the outcome speaks.
func (sc *SessionCache) EnqueueMessage(key string, msg agenttypes.QueuedMessage) (MailboxOutcome, error) {
	if key == "" {
		return MailboxPersistFailed, errors.New("route key required")
	}
	msg.RouteKey = key

	// (1) CWD guard. Reuse the existing normalizer so /tmp ↔ /private/tmp
	// equivalence is honored.
	sc.mu.Lock()
	entry, ok := sc.routes[key]
	if !ok || entry == nil {
		entry = &routeEntry{lastAccess: time.Now()}
		sc.routes[key] = entry
	}
	activeCWD := entry.activeCWD
	if entry.mailbox == nil {
		entry.mailbox = agenttypes.NewMailbox(sc.mailboxCap)
	}
	mb := entry.mailbox
	sc.mu.Unlock()

	if msg.CWD != "" && activeCWD != "" {
		if normalizeCWDForCompare(msg.CWD) != normalizeCWDForCompare(activeCWD) {
			return MailboxRouteMismatch, nil
		}
	}

	// (2) Persist.
	inserted := true
	if sc.mailboxStore != nil {
		ok, err := sc.mailboxStore.Append(msg)
		if err != nil {
			return MailboxPersistFailed, err
		}
		inserted = ok
		if !inserted {
			// Cloud msg_id dedup hit — safe to ack again. The original row
			// is still pending or already consumed; either way, do not
			// re-enqueue.
			return MailboxDeduped, nil
		}
	}

	// (3) In-memory enqueue with cap.
	if ok, err := mb.Enqueue(msg); !ok {
		// (4) Roll back SQLite row to keep durable + in-memory state aligned.
		if sc.mailboxStore != nil && inserted {
			if delErr := sc.mailboxStore.Delete(msg.ID); delErr != nil {
				return MailboxPersistFailed, fmt.Errorf("cap exceeded, rollback failed: %w", delErr)
			}
		}
		if errors.Is(err, agenttypes.ErrMailboxFull) {
			return MailboxQueueFull, nil
		}
		return MailboxPersistFailed, err
	}

	// (5) Also notify the active run (if any) via its injectCh so the
	// running agent loop's existing mid-turn drain (loop.go drain block)
	// consumes this message in the next iteration boundary — without
	// having to wait for the next RunAgent invocation to drain mailbox
	// from scratch. The mailbox row is the durability backbone; injectCh
	// is the "live notify" so users see their queued message merged into
	// the current turn rather than the next.
	//
	// Non-blocking: if injectCh is full or absent, the message stays in
	// the mailbox and will be drained by the next RunAgent start.
	sc.mu.Lock()
	ch := entry.injectCh
	sc.mu.Unlock()
	if ch != nil {
		select {
		case ch <- agent.InjectedMessage{Text: msg.Text, CWD: msg.CWD}:
			log.Printf("daemon: mailbox→injectCh wrote %q (route=%s id=%s)", truncForLog(msg.Text, 60), key, msg.ID)
		default:
			// injectCh saturated — fine, mailbox still owns durability.
			log.Printf("daemon: mailbox→injectCh full, leaving on disk (route=%s id=%s)", key, msg.ID)
		}
	} else {
		log.Printf("daemon: mailbox→injectCh nil (no active run) (route=%s id=%s)", key, msg.ID)
	}
	return MailboxQueued, nil
}

func truncForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// DrainMailbox dequeues up to limit messages for the route. Returns nil for
// an empty mailbox or unknown route. Callers MUST invoke MarkMailboxConsumed
// AFTER appending the messages to the session AND session.Save returns
// successfully — see Task 1.7 / P0-4 contract.
func (sc *SessionCache) DrainMailbox(key string, limit int) []agenttypes.QueuedMessage {
	if key == "" {
		return nil
	}
	sc.mu.Lock()
	entry := sc.routes[key]
	sc.mu.Unlock()
	if entry == nil || entry.mailbox == nil {
		return nil
	}
	return entry.mailbox.DequeueBatch(limit)
}

// MarkMailboxConsumed durably records that the given mailbox IDs have been
// successfully persisted to a session. Safe to call with nil store; no-op
// when ids is empty.
func (sc *SessionCache) MarkMailboxConsumed(ids []string) error {
	if sc.mailboxStore == nil {
		return nil
	}
	return sc.mailboxStore.MarkConsumed(ids)
}

// SeedMailbox prepends recovered messages to the route's mailbox. Used by
// daemon startup recovery (see internal/daemon/server.go).
func (sc *SessionCache) SeedMailbox(key string, msgs []agenttypes.QueuedMessage) (loaded, dropped int) {
	if key == "" || len(msgs) == 0 {
		return 0, 0
	}
	mb := sc.EnsureMailbox(key)
	if mb == nil {
		return 0, len(msgs)
	}
	return mb.SeedFromStore(msgs)
}

// MailboxSnapshot returns a defensive copy of the route's pending mailbox.
// Returns nil for unknown routes.
func (sc *SessionCache) MailboxSnapshot(key string) []agenttypes.QueuedMessage {
	if key == "" {
		return nil
	}
	sc.mu.Lock()
	entry := sc.routes[key]
	sc.mu.Unlock()
	if entry == nil || entry.mailbox == nil {
		return nil
	}
	return entry.mailbox.Snapshot()
}

// MailboxRetract removes the message with the given ID from both the
// in-memory mailbox AND the SQLite store. Returns true iff the in-memory
// retract succeeded (the store delete is best-effort; an orphan store row
// without an in-memory counterpart is harmless and gets purged by the daily
// sweep).
func (sc *SessionCache) MailboxRetract(key, id string) bool {
	if key == "" || id == "" {
		return false
	}
	sc.mu.Lock()
	entry := sc.routes[key]
	sc.mu.Unlock()
	if entry == nil || entry.mailbox == nil {
		return false
	}
	ok := entry.mailbox.Retract(id)
	if ok && sc.mailboxStore != nil {
		_ = sc.mailboxStore.Delete(id)
	}
	return ok
}

// MailboxLen returns the route's mailbox length, or 0 for unknown routes.
func (sc *SessionCache) MailboxLen(key string) int {
	if key == "" {
		return 0
	}
	sc.mu.Lock()
	entry := sc.routes[key]
	sc.mu.Unlock()
	if entry == nil || entry.mailbox == nil {
		return 0
	}
	return entry.mailbox.Len()
}

// MailboxStore returns the underlying store, or nil if persistence is
// disabled. Exposed so the daemon-startup recovery path in server.go can
// call LoadAllPending without re-plumbing through SessionCache.
func (sc *SessionCache) MailboxStoreHandle() *MailboxStore {
	return sc.mailboxStore
}

// RouteKeyForSession returns the route key currently bound to the given
// session ID, or "" when no route is mapped (or sessionID is empty).
// Used by GET /queue's ?session_id= shortcut lookup.
func (sc *SessionCache) RouteKeyForSession(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for key, entry := range sc.routes {
		if entry == nil {
			continue
		}
		if entry.loadSessionID() == sessionID {
			return key
		}
	}
	return ""
}
