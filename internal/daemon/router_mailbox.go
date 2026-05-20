package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agenttypes"
)

// MailboxOutcome describes the result of EnqueueMessage. It is wire-distinct
// from the legacy InjectResult: EnqueueMessage is the ack-on-persist path
// used by cmd/daemon.go and the HTTP /queue endpoint. InjectMessage remains
// the separate in-memory "modify active run" path for explicit /message
// injection.
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

	return MailboxQueued, nil
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

// RegisterAdHocSessionRoute attaches a transient routeEntry under the key
// "session:<sessionID>" so POST /queue and POST /cancel can locate the
// running default-agent loop (ComputeRouteKey returns "" for the desktop
// default-agent path, so the routed branch in runner.go never registers
// these runs the normal way).
//
// The entry holds the run's cancel function, done channel, injectCh, and
// activeCWD — exactly the fields handleEnqueueQueue and handleCancel need
// — but does NOT participate in entry.mu locking or sessMgr ownership.
// Those stay with the ephemeral sessMgr the runner created. Caller MUST
// invoke UnregisterAdHocSessionRoute on the returned key in a defer to
// avoid leaking dangling entries.
//
// Returns ("" , false) if a route already exists for this key — the
// caller's run can't safely overwrite an in-flight one.
func (sc *SessionCache) RegisterAdHocSessionRoute(
	sessionID string,
	cancel context.CancelFunc,
	done chan struct{},
	injectCh chan agent.InjectedMessage,
	activeCWD string,
) (string, bool) {
	if sessionID == "" {
		return "", false
	}
	key := "session:" + sanitizeRouteValue(sessionID)
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if existing, exists := sc.routes[key]; exists && existing != nil && existing.done != nil {
		// Another run already claimed this session — refuse rather than
		// stomping on the live entry's cancel handle.
		return "", false
	}
	entry, exists := sc.routes[key]
	if !exists || entry == nil {
		entry = &routeEntry{lastAccess: time.Now()}
		sc.routes[key] = entry
	}
	entry.cancel = cancel
	entry.done = done
	entry.injectCh = injectCh
	entry.activeCWD = activeCWD
	entry.lastAccess = time.Now()
	entry.storeSessionID(sessionID)
	return key, true
}

// UnregisterAdHocSessionRoute clears the run-state fields of the entry
// matching key. The entry itself is left in the map as a reusable shell
// (mailbox preservation, lastAccess for eviction policy). If the entry
// has a non-empty mailbox, we leave injectCh nil so the next RunAgent
// that picks it up via the routed branch can attach its own channels.
func (sc *SessionCache) UnregisterAdHocSessionRoute(key string) {
	if key == "" {
		return
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	entry, ok := sc.routes[key]
	if !ok || entry == nil {
		return
	}
	entry.cancel = nil
	entry.done = nil
	entry.injectCh = nil
	entry.activeCWD = ""
}
