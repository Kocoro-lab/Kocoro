package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agenttypes"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// DrainedInflightEntry tracks a user IM message that has been pulled from
// injectCh into an LLM turn. The agent loop appends one entry per drained
// follow-up so the daemon can emit MESSAGE_LIFECYCLE "done" / "cleared"
// events to Cloud at run completion (Cloud needs the original IMStatusContext
// to map each entry back to a platform reaction).
//
// CloudMessageID is the Cloud-side envelope id (set on MessagePayload.MessageID
// in the WS frame); MessageID alone is ambiguous in a daemon that also tracks
// session messages and mailbox rows.
type DrainedInflightEntry struct {
	CloudMessageID  string
	IMStatusContext json.RawMessage
}

var ErrSessionChanged = errors.New("session changed since pre-check")
var ErrRouteActive = errors.New("route has an active run")

type routeEntry struct {
	mu            sync.Mutex
	cancel        context.CancelFunc
	cancelPending bool                    // set under sc.mu when CancelRoute fires before cancel is assigned
	pendingReason agenttypes.CancelReason // reason for the pending cancel (set with cancelPending)
	// cancelCause is the optional reason-tagged variant of cancel. Runner.go
	// registers both (cancel + cancelCause) when it owns the ctx — the legacy
	// CancelFunc value lives in cancel; the typed one in cancelCause. CancelRoute
	// prefers cancelCause when present so the loop can extract the reason via
	// agenttypes.ExtractReason(context.Cause(ctx)).
	cancelCause context.CancelCauseFunc
	done        chan struct{}
	// sessionID is atomic so CancelBySessionID can scan all routes without
	// blocking on entry.mu held by an active run. Writers in the runner
	// already hold entry.mu (per the resume invariant); the atomic only
	// adds memory-order visibility for the lock-free reader.
	sessionID  atomic.Pointer[string]
	lastAccess time.Time
	injectCh   chan agent.InjectedMessage // buffered channel for mid-run follow-up injection
	activeCWD  string
	evicting   bool
	manager    *session.Manager
	// mailbox is the persisted per-route message queue (Phase 1+).
	// Lazily created on first EnsureMailbox; nil for routes that have
	// never queued. Coexists with injectCh — injectCh is the legacy
	// in-memory mid-run path, mailbox is the durability-first path for
	// new ack-on-persist semantics.
	mailbox *agenttypes.Mailbox
	// drainedInflight is the per-route ordered list of IM messages that this
	// run has fed into an LLM turn. Each entry pairs a Cloud envelope id with
	// the message's IMStatusContext. Populated by RunLifecycleEmitter.OnUserMessage-
	// Processing (which calls SessionCache.AppendDrainedInflight). Consumed by
	// run-completion lifecycle emission. Locking: sc.mu (see AppendDrainedInflight).
	drainedInflight []DrainedInflightEntry
}

// loadSessionID returns the route's current session ID, or "" if unset.
func (e *routeEntry) loadSessionID() string {
	if p := e.sessionID.Load(); p != nil {
		return *p
	}
	return ""
}

// storeSessionID atomically updates the route's session ID.
func (e *routeEntry) storeSessionID(id string) {
	e.sessionID.Store(&id)
}

func cloneSessionSnapshot(sess *session.Session) *session.Session {
	if sess == nil {
		return nil
	}
	clone := *sess
	clone.Messages = append([]client.Message(nil), sess.Messages...)
	clone.MessageMeta = append([]session.MessageMeta(nil), sess.MessageMeta...)
	clone.RemoteTasks = append([]string(nil), sess.RemoteTasks...)
	return &clone
}

// SessionCache separates route-level locking from session storage.
//   - routes: one lock/cancel/inflight channel per routing key
//   - managers: one shared session.Manager per sessions directory for non-routed usage
//   - route manager: lazily created session.Manager per route for routed runs
//   - mailboxStore: optional SQLite-backed durability for per-route mailboxes
//     (nil disables persistence; tests typically pass nil)
//   - mailboxCap: per-route mailbox capacity cap (defaults to 100)
type SessionCache struct {
	mu           sync.Mutex
	routes       map[string]*routeEntry
	managers     map[string]*session.Manager
	shannonDir   string
	mailboxStore *MailboxStore
	mailboxCap   int
	// retractedInjects holds, per route key, the client_message_ids the
	// client cancelled AFTER the inject was already sent to injectCh. The agent
	// loop consults this at drain time (via injectRetractedChecker) and skips
	// any matching follow-up, so a cancelled steering message never reaches the
	// model — injectCh is a Go channel and cannot remove elements directly, so
	// the skip happens after drain rather than by mutating the channel.
	// Values are tombstone-creation times: tombstones survive run transitions
	// (a retract that loses the race against run teardown must still drop a
	// late inject landing on the NEXT run) and are reaped by TTL + per-route
	// cap instead of wholesale deletion at run end.
	retractedInjects map[string]map[string]time.Time
	// committedInjects holds, per route key, the client_message_ids whose
	// follow-up passed the retraction filter and entered an LLM turn (it is —
	// or is about to be — a persisted user message). The retract endpoint
	// answers "already_committed" from this ledger so the client knows its
	// retract was too late and must NOT re-send the same text as a fresh
	// message (the force-send / pop-back duplicate). Marking happens in the
	// same critical section as the retraction-filter check, so every id
	// resolves to exactly one of {dropped-by-tombstone, committed} — there is
	// no window where a retract returns "retracted" for an id the loop is
	// about to commit. Same TTL + cap reaping as retractedInjects.
	committedInjects map[string]map[string]time.Time
	// lastInjectLedgerSweep throttles the cross-route reaping of the two inject
	// ledgers (see sweepInjectLedgersLocked). The ledgers are keyed like
	// sc.routes, which never shrinks, so without a periodic sweep every route
	// that ever committed/retracted an inject would retain its entry for the
	// daemon's lifetime — the leak flagged in review. Updated under sc.mu.
	lastInjectLedgerSweep time.Time
	// systemEvents, when set, is Forgotten per route on eviction so an
	// enqueue-but-never-run route does not leak its bounded queue. Optional —
	// nil in tests / pure-local paths.
	systemEvents *SystemEventStore
}

// NewSessionCache creates a cache rooted at the given shannon directory.
// Mailbox persistence is disabled (callers that want crash recovery should use
// NewSessionCacheWithMailbox).
func NewSessionCache(shannonDir string) *SessionCache {
	return NewSessionCacheWithMailbox(shannonDir, nil, 0)
}

// NewSessionCacheWithMailbox creates a cache with the given mailbox store and
// per-route capacity cap. A non-nil store enables persistence + recovery; a
// zero capacity falls back to the agenttypes default (100).
func NewSessionCacheWithMailbox(shannonDir string, store *MailboxStore, capacity int) *SessionCache {
	if capacity <= 0 {
		capacity = 100
	}
	return &SessionCache{
		routes:           make(map[string]*routeEntry),
		managers:         make(map[string]*session.Manager),
		shannonDir:       shannonDir,
		mailboxStore:     store,
		mailboxCap:       capacity,
		retractedInjects: make(map[string]map[string]time.Time),
		committedInjects: make(map[string]map[string]time.Time),
	}
}

// injectLedgerTTL bounds how long retraction tombstones and committed-inject
// ledger entries live. Long enough to cover any realistic retract-vs-teardown
// race (network latency + run teardown + replacement run start), short enough
// that the per-route maps cannot accumulate across a long-lived daemon.
const injectLedgerTTL = 10 * time.Minute

// injectLedgerCap bounds entries per route. A client queues at most a handful
// of drafts per run; hitting this cap means something is wrong upstream, and
// dropping the OLDEST entry degrades to the pre-ledger behavior (best-effort
// retract) instead of growing without bound.
const injectLedgerCap = 256

// pruneInjectLedgerLocked removes expired entries from one route's ledger map
// and, if the map is over cap, evicts oldest-first down to the cap. Caller
// must hold sc.mu. Deletes the route key entirely when the set empties.
func pruneInjectLedgerLocked(ledger map[string]map[string]time.Time, key string, now time.Time) {
	set := ledger[key]
	if set == nil {
		return
	}
	for id, at := range set {
		if now.Sub(at) > injectLedgerTTL {
			delete(set, id)
		}
	}
	for len(set) > injectLedgerCap {
		oldestID := ""
		var oldestAt time.Time
		for id, at := range set {
			if oldestID == "" || at.Before(oldestAt) {
				oldestID, oldestAt = id, at
			}
		}
		delete(set, oldestID)
	}
	if len(set) == 0 {
		delete(ledger, key)
	}
}

// injectLedgerSweepInterval throttles the cross-route ledger sweep. The ledgers
// only grow under inject activity, and the sweep runs on the write path, so a
// once-per-interval cadence keeps the amortized cost negligible while bounding
// total retained keys to roughly those active within one TTL window.
const injectLedgerSweepInterval = time.Minute

// sweepInjectLedgersLocked prunes expired entries across ALL routes in both
// inject ledgers, throttled to one pass per injectLedgerSweepInterval. Called
// from the ledger write paths (which already hold sc.mu), so reclamation tracks
// activity without a background goroutine: an idle daemon does no work and the
// maps do not grow. This is what keeps the ledgers from shadowing sc.routes
// (which never shrinks) for the daemon's lifetime. Deleting keys mid-range is
// safe per the Go spec.
func (sc *SessionCache) sweepInjectLedgersLocked(now time.Time) {
	if !sc.lastInjectLedgerSweep.IsZero() && now.Sub(sc.lastInjectLedgerSweep) < injectLedgerSweepInterval {
		return
	}
	sc.lastInjectLedgerSweep = now
	for key := range sc.retractedInjects {
		pruneInjectLedgerLocked(sc.retractedInjects, key, now)
	}
	for key := range sc.committedInjects {
		pruneInjectLedgerLocked(sc.committedInjects, key, now)
	}
}

// GetOrCreate returns the session.Manager for the given agent, preserving
// compatibility with existing caller paths.
func (sc *SessionCache) GetOrCreate(agent string) *session.Manager {
	return sc.GetOrCreateManager(sc.sessionsDir(agent))
}

// GetOrCreateManager returns the shared session.Manager for a sessions directory.
// Multiple routes that map to the same directory reuse the same manager.
func (sc *SessionCache) GetOrCreateManager(sessionsDir string) *session.Manager {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	if mgr, ok := sc.managers[sessionsDir]; ok && mgr != nil {
		return mgr
	}

	mgr := sc.newManager(sessionsDir)
	sc.managers[sessionsDir] = mgr
	return mgr
}

// Lock acquires the route lock for a named agent.
// kept for compatibility with existing caller paths.
func (sc *SessionCache) Lock(agent string) {
	sc.LockRouteWithManager(sc.agentRouteKey(agent), sc.sessionsDir(agent))
}

// Unlock releases the route lock for a named agent.
// kept for compatibility with existing caller paths.
func (sc *SessionCache) Unlock(agent string) {
	sc.UnlockRoute(sc.agentRouteKey(agent))
}

// LockRoute acquires the per-route mutex.
// If another run is in-flight for this route, it is canceled and waited for
// before this call returns.
func (sc *SessionCache) LockRoute(key string) *routeEntry {
	// Preserve the compatibility behavior for non-routed callers.
	// The route manager is not created here because the caller may not know
	// the sessions directory.
	return sc.LockRouteWithManager(key, "")
}

// TryLockRouteWithManager acquires a route lock without canceling or waiting
// for an existing run. busy=true means the route is active and the caller
// should reject or retry, not inject into the existing run.
//
// Implementation note: we take entry.mu.TryLock() while still holding sc.mu so
// the cancelPending clear is atomic with the lock acquisition. If we cleared
// cancelPending after releasing sc.mu, a CancelRoute landing in the gap would
// be silently overwritten — see LockRouteWithManager for the same discipline.
// TryLock is non-blocking, so holding sc.mu briefly while calling it cannot
// deadlock (sc.mu is always the outer lock by convention).
func (sc *SessionCache) TryLockRouteWithManager(key, sessionsDir string) (*routeEntry, bool) {
	if key == "" {
		return nil, false
	}
	sc.mu.Lock()
	entry, ok := sc.routes[key]
	if !ok {
		entry = &routeEntry{lastAccess: time.Now()}
		sc.routes[key] = entry
	}
	if entry.manager == nil && sessionsDir != "" {
		entry.manager = sc.newManager(sessionsDir)
	}

	if !entry.mu.TryLock() {
		// Busy: leave cancelPending alone. The lock-holder may have a real
		// pending intent that we must not erase.
		sc.mu.Unlock()
		return nil, true
	}
	// We own entry.mu now. Clear stale cancelPending atomically with the
	// acquisition, mirroring LockRouteWithManager:154. A cancel arriving after
	// this sc.mu.Unlock will set cancelPending=true and SetRouteCancel will
	// catch it.
	entry.cancelPending = false
	entry.lastAccess = time.Now()
	sc.mu.Unlock()
	return entry, false
}

func (sc *SessionCache) LockRouteWithManager(key, sessionsDir string) *routeEntry {
	if key == "" {
		return nil
	}
	sc.mu.Lock()
	entry, ok := sc.routes[key]
	if !ok {
		entry = &routeEntry{
			lastAccess: time.Now(),
		}
		sc.routes[key] = entry
	}
	if entry.manager == nil && sessionsDir != "" {
		entry.manager = sc.newManager(sessionsDir)
	}
	cancel := entry.cancel
	done := entry.done
	// Clear any stale pending cancel from when the route was idle. A cancel
	// arriving after this point (during the startup window before SetRouteCancel
	// is called) will set cancelPending again and be picked up correctly.
	entry.cancelPending = false
	sc.mu.Unlock()

	if cancel != nil && done != nil {
		cancel()
		<-done
	}

	entry.mu.Lock()
	entry.lastAccess = time.Now()
	return entry
}

// UnlockRoute releases the per-route mutex acquired by LockRoute.
// IMPORTANT: entry.mu is already held by the caller (from LockRouteWithManager).
// Do NOT re-acquire it — sync.Mutex is not reentrant.
func (sc *SessionCache) UnlockRoute(key string) {
	sc.mu.Lock()
	entry, ok := sc.routes[key]
	sc.mu.Unlock()
	if !ok || entry == nil {
		return
	}

	// Check evicting flag under the already-held lock.
	var mgr *session.Manager
	entry.cancel = nil
	entry.cancelPending = false
	entry.lastAccess = time.Now()
	if entry.evicting {
		mgr = entry.manager
		entry.manager = nil
		entry.evicting = false
	}

	// Single unlock point — releases the lock from LockRouteWithManager.
	// Entry stays in the map as a reusable shell (never deleted).
	entry.mu.Unlock()

	if mgr != nil {
		if err := mgr.Close(); err != nil {
			log.Printf("daemon: failed to close session for evicted route %q: %v", key, err)
		}
	}
}

// SetRouteCancel registers the cancel function for the active run under sc.mu,
// making it immediately visible to CancelRoute. If a cancel was already
// requested (cancelPending), cancel is called before returning.
//
// Called by the runner while entry.mu is held — sc.mu may be acquired while
// entry.mu is held because all other callers release sc.mu before acquiring
// entry.mu (same pattern as UnlockRoute).
func (sc *SessionCache) SetRouteCancel(key string, cancel context.CancelFunc) {
	sc.mu.Lock()
	entry, ok := sc.routes[key]
	var pending bool
	if ok && entry != nil {
		entry.cancel = cancel
		pending = entry.cancelPending
		entry.cancelPending = false
	}
	sc.mu.Unlock()
	if pending && cancel != nil {
		cancel()
	}
}

// SetSystemEventStore wires the daemon's S0 store so evictRoute can release a
// route's queued system events. Safe to call once at startup; nil clears.
func (sc *SessionCache) SetSystemEventStore(store *SystemEventStore) {
	sc.mu.Lock()
	sc.systemEvents = store
	sc.mu.Unlock()
}

// SetRouteSessionID stores the current route session id for future resume.
func (sc *SessionCache) SetRouteSessionID(key, sessionID string) {
	sc.mu.Lock()
	entry := sc.routes[key]
	sc.mu.Unlock()
	if entry == nil {
		return
	}
	entry.mu.Lock()
	entry.storeSessionID(sessionID)
	entry.mu.Unlock()
}

// RouteSessionID returns the session id tracked by this route.
func (sc *SessionCache) RouteSessionID(key string) string {
	sc.mu.Lock()
	entry := sc.routes[key]
	sc.mu.Unlock()
	if entry == nil {
		return ""
	}
	return entry.loadSessionID()
}

// InjectResult describes the outcome of an InjectMessage call.
type InjectResult int

const (
	InjectNoActiveRun InjectResult = iota // no in-flight run; caller should start one
	InjectOK                              // message delivered to the running loop
	InjectQueueFull                       // active run exists but queue is saturated
	InjectBusy                            // run exists but is not yet ready to receive injected messages
	InjectCWDConflict                     // active run uses a different immutable cwd
	InjectRetracted                       // client retracted this id before delivery; dropped intentionally
)

// ActiveSessionIDs returns the set of session IDs whose route currently
// owns an in-flight agent run (entry.done != nil). Used by the HTTP layer
// to flag sessions as "in_progress" in the listing without scanning JSON
// from disk. Returns nil when nothing is running so JSON encoders emit
// null and not an empty object.
func (sc *SessionCache) ActiveSessionIDs() map[string]struct{} {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if len(sc.routes) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(sc.routes))
	for _, entry := range sc.routes {
		if entry == nil || entry.done == nil {
			continue
		}
		id := entry.loadSessionID()
		if id == "" {
			continue
		}
		out[id] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ActiveRouteKeysForChannel returns active route keys that reference the given
// platform and channel. BEST-EFFORT: the channelID must appear as a delimited
// segment of the route key — either an interior `:<channelID>:` segment or the
// trailing `:<channelID>` suffix — so a query for "C1" does NOT leak onto an
// unrelated "C123" route (substring match would have, violating route
// isolation). The daemon route key embeds the platform/channel for IM sources,
// but a route whose key encodes the channel without `:` delimiters is missed —
// in that case the per-run ConnectionStateCache render (Session Facts) still
// surfaces the state, so no awareness is lost, only the immediate injection.
// channelID == "" (binding/transport events) returns all routes whose key
// carries the platform as a delimited `:<platform>:` segment. ComputeRouteKey
// always embeds Source as an interior segment (preceded by `default:` or
// `agent:<name>:`, followed by channel/thread/sender), never at the head or
// tail — so the segment check below cannot miss a legitimate IM route, but a
// bare substring WOULD mis-match a different-platform route whose channel/sender
// name merely contains the platform string (e.g. a Telegram channel
// "slackmirror" matching a Slack event).
func (sc *SessionCache) ActiveRouteKeysForChannel(platform, channelID string) []string {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	var out []string
	for key := range sc.routes {
		if !strings.Contains(key, ":"+platform+":") {
			continue
		}
		if channelID != "" {
			if !strings.Contains(key, ":"+channelID+":") && !strings.HasSuffix(key, ":"+channelID) {
				continue
			}
		}
		out = append(out, key)
	}
	return out
}

// HasActiveRun reports whether the route currently owns an in-flight
// agent run. Pure lookup, no side effects — safe to call from request
// handlers that need to decide between "inject into existing run" vs
// "start a new run" without actually delivering anything.
func (sc *SessionCache) HasActiveRun(key string) bool {
	if key == "" {
		return false
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	entry, ok := sc.routes[key]
	if !ok || entry == nil {
		return false
	}
	// Mid-cancellation routes report as inactive so callers (e.g.
	// /message to InjectMessage gate) let a fresh RunAgent start instead
	// of bouncing the request with 409 while the runner.go cleanup is
	// still flushing the old run. Same race that InjectMessage handles;
	// kept in sync here.
	if entry.cancelPending {
		return false
	}
	return entry.done != nil
}

// InjectMessage sends a message into a running agent loop for this route.
// Returns:
//   - InjectOK when the follow-up was delivered to the active run
//   - InjectNoActiveRun when no run is in-flight (caller may start a new run)
//   - InjectQueueFull when the active run owns the route but its queue is saturated
//   - InjectBusy when the active run exists but is not yet ready to receive injections
//   - InjectCWDConflict when the follow-up tries to change cwd mid-run
func (sc *SessionCache) InjectMessage(key string, msg agent.InjectedMessage) InjectResult {
	if key == "" {
		return InjectNoActiveRun
	}
	// Normalize the request cwd before taking the lock — EvalSymlinks touches the
	// filesystem and must not run under sc.mu.
	requestCWD := normalizeCWDForCompare(msg.CWD)

	sc.mu.Lock()
	defer sc.mu.Unlock()
	entry, ok := sc.routes[key]
	if !ok || entry == nil {
		return InjectNoActiveRun
	}
	// Treat a route mid-cancellation as "no active run" so the caller can
	// fall through to starting a fresh RunAgent. Without this, Desktop's
	// interrupt-send (cancel to SSE close to immediate POST /message) races
	// the runner.go cleanup that clears `done`, and the new prompt lands
	// here while `done` is still non-nil but `cancelPending` is set,
	// returning InjectBusy 409 to the client and surfacing as "SSE request
	// failed" on the Desktop toolbar.
	if entry.cancelPending {
		return InjectNoActiveRun
	}
	if entry.done == nil {
		return InjectNoActiveRun
	}
	if entry.injectCh == nil {
		return InjectBusy
	}
	// entry.activeCWD is stored pre-normalized (SetRouteRunState /
	// RegisterAdHocSessionRoute normalize before the lock), so this comparison
	// runs no EvalSymlinks filesystem call under sc.mu. (P7)
	if requestCWD != "" && requestCWD != entry.activeCWD {
		return InjectCWDConflict
	}
	// Ingestion-time retraction: tombstones survive run transitions, so an
	// inject POST that arrives AFTER the client already retracted its id —
	// e.g. delayed past a Cmd+Enter retract+cancel+resend sequence and landing
	// on the REPLACEMENT run — is dropped here instead of being committed into
	// a turn that already carries the same text via the force-send payload.
	if msg.ClientMessageID != "" {
		if set := sc.retractedInjects[key]; set != nil {
			if at, ok := set[msg.ClientMessageID]; ok && time.Since(at) <= injectLedgerTTL {
				delete(set, msg.ClientMessageID)
				if len(set) == 0 {
					delete(sc.retractedInjects, key)
				}
				return InjectRetracted
			}
		}
	}
	// Enqueue under sc.mu so the send is atomic with respect to
	// DrainSurvivorsOrCloseInject: a follow-up racing run teardown either lands
	// before the drain (reclaimed as a survivor) or observes the closed window
	// above and returns InjectNoActiveRun. On a non-end_turn exit the loop
	// doesn't drain, but ReEnqueueInjectSurvivors (runner cleanup) drains and
	// re-queues any survivor to the mailbox before ClearRouteRunState nils the
	// channel — so a follow-up is never silently dropped after returning
	// InjectOK. (P5)
	//
	// DrainSurvivorsOrCloseInject also takes sc.mu, but the two never nest, and
	// the send below is a non-blocking select on a buffered channel, so holding
	// the lock here cannot deadlock against the drain.
	select {
	case entry.injectCh <- msg:
		return InjectOK
	default:
		return InjectQueueFull
	}
}

// normalizeCWDForCompare cleans and symlink-resolves a CWD path for comparison.
// This prevents false cwd_conflict on macOS where /tmp → /private/tmp.
func normalizeCWDForCompare(cwd string) string {
	cwd = filepath.Clean(strings.TrimSpace(cwd))
	if cwd == "." || cwd == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		return resolved
	}
	return cwd
}

// AppendDrainedInflight records that an IM-sourced user message has moved
// from injectCh (or first-turn primary) into an LLM turn. Consumed at run
// completion by Task 9 to emit MESSAGE_LIFECYCLE "done" / "cleared" for each
// entry — Cloud needs the original IMStatusContext to map the entry back to
// a platform reaction. No-op when key or CloudMessageID is empty (defensive:
// non-IM drains short-circuit at the call site already).
//
// Locking: sc.mu only. The agent loop runs under entry.mu (acquired by the
// runner via LockRouteWithManager), but we never touch entry.mu here. The
// slice field is guarded by sc.mu — Task 9's run-completion reader MUST
// take sc.mu the same way.
func (sc *SessionCache) AppendDrainedInflight(key string, entry DrainedInflightEntry) {
	if key == "" || entry.CloudMessageID == "" {
		return
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if e, ok := sc.routes[key]; ok && e != nil {
		e.drainedInflight = append(e.drainedInflight, entry)
	}
}

// TakeDrainedInflight returns the drained-inflight slice for routeKey and
// clears it from the route entry. Atomic under sc.mu — readers see either the
// full slice OR an empty one, never a partial. Used by the run-completion
// lifecycle emit to drain + clear in one critical section so a second call
// after completion is a silent no-op (idempotent).
func (sc *SessionCache) TakeDrainedInflight(routeKey string) []DrainedInflightEntry {
	if routeKey == "" {
		return nil
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	e, ok := sc.routes[routeKey]
	if !ok || e == nil {
		return nil
	}
	out := e.drainedInflight
	e.drainedInflight = nil
	return out
}

// SetRouteRunState updates the externally visible run state for a route.
// This is used by injection/cancel paths that must not block on entry.mu while
// the active run holds it for the duration of execution.
func (sc *SessionCache) SetRouteRunState(key string, done chan struct{}, injectCh chan agent.InjectedMessage, activeCWD string) {
	if key == "" {
		return
	}
	// Pre-normalize OUTSIDE the lock so InjectMessage can compare against
	// entry.activeCWD under sc.mu without an EvalSymlinks filesystem call. (P7)
	normalizedCWD := normalizeCWDForCompare(activeCWD)
	sc.mu.Lock()
	entry, ok := sc.routes[key]
	if ok && entry != nil {
		entry.done = done
		entry.injectCh = injectCh
		entry.activeCWD = normalizedCWD
	}
	sc.mu.Unlock()
}

// ClearRouteRunState removes the externally visible in-flight run state for a route.
func (sc *SessionCache) ClearRouteRunState(key string) {
	if key == "" {
		return
	}
	sc.mu.Lock()
	entry, ok := sc.routes[key]
	if ok && entry != nil {
		entry.done = nil
		entry.injectCh = nil
		entry.activeCWD = ""
	}
	// Tombstones and the committed ledger deliberately SURVIVE run end: a
	// retract that lost the race against teardown must still drop its target
	// when the late inject lands on the next run, and a pop-back retract must
	// still learn "already_committed" after the owning run finished. Reap by
	// TTL + cap instead of wholesale deletion.
	now := time.Now()
	pruneInjectLedgerLocked(sc.retractedInjects, key, now)
	pruneInjectLedgerLocked(sc.committedInjects, key, now)
	sc.mu.Unlock()
}

// DrainSurvivorsOrCloseInject is the atomic core of the end_turn drain-race
// guard. The agent loop calls it when it is about to return: it drains any
// follow-ups still buffered on the route (delivered via InjectMessage's
// InjectOK but not yet consumed) and filters out retracted ones, all under
// sc.mu.
//
//   - If non-retracted survivors remain, they are returned and the inject
//     window stays OPEN: the loop commits them, continues, and re-checks here
//     on its next end_turn.
//   - If nothing survives, the inject window is CLOSED in the SAME critical
//     section (done/injectCh niled). Because InjectMessage now also sends under
//     sc.mu, a follow-up racing the loop's return either landed before this
//     drain (and is returned here) or observes the closed window and returns
//     InjectNoActiveRun — so the caller starts a fresh run instead of orphaning
//     the message on a channel the loop will never read again.
//
// This closes the window that let an InjectOK message strand silently after the
// run completed (the IM-burst "last follow-up never enters the loop" bug, plus
// its sibling "the IM card spins forever"). Stateless Cloud/Slack cannot
// re-queue client-side the way Desktop does, so the guarantee must live here.
// Retracted follow-ups are reaped (tombstone consumed), not returned. Safe and
// idempotent on an unknown or already-closed route.
//
// markSurvivorsCommitted distinguishes the two callers:
//   - the end_turn drain-race guard passes TRUE — its survivors are committed
//     INLINE into the continuing run, so they enter the committed ledger here,
//     in the same critical section as the retraction filter;
//   - ReEnqueueInjectSurvivors passes FALSE — its survivors go to the durable
//     mailbox for the NEXT run, and marking them committed would make a
//     subsequent retract report "already_committed" while the retract cascade
//     deletes the mailbox row, losing the message entirely.
func (sc *SessionCache) DrainSurvivorsOrCloseInject(key string, markSurvivorsCommitted bool) []agent.InjectedMessage {
	if key == "" {
		return nil
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	entry, ok := sc.routes[key]
	if !ok || entry == nil || entry.injectCh == nil {
		return nil
	}
	var survivors []agent.InjectedMessage
	retracted := sc.retractedInjects[key]
	now := time.Now()
drainLoop:
	for {
		select {
		case m := <-entry.injectCh:
			if m.ClientMessageID != "" && retracted != nil {
				if at, hit := retracted[m.ClientMessageID]; hit && now.Sub(at) <= injectLedgerTTL {
					// Reap the tombstone (one-shot, mirrors ConsumeInjectRetracted)
					// so a cancelled follow-up is dropped, not re-dispatched.
					delete(retracted, m.ClientMessageID)
					continue
				}
			}
			if markSurvivorsCommitted && m.ClientMessageID != "" {
				sc.markInjectCommittedLocked(key, m.ClientMessageID, now)
			}
			survivors = append(survivors, m)
		default:
			break drainLoop
		}
	}
	if len(survivors) == 0 {
		// Nothing left to process — close the window atomically so a follow-up
		// racing the loop's return falls through to a fresh run instead of
		// orphaning. Same teardown as ClearRouteRunState. Tombstones survive
		// (TTL-reaped) so a late retract/inject pair still resolves correctly.
		entry.done = nil
		entry.injectCh = nil
		entry.activeCWD = ""
		pruneInjectLedgerLocked(sc.retractedInjects, key, now)
	}
	return survivors
}

// markInjectCommittedLocked records a client_message_id in the committed
// ledger. Caller must hold sc.mu.
func (sc *SessionCache) markInjectCommittedLocked(key, clientMessageID string, now time.Time) {
	if sc.committedInjects == nil {
		sc.committedInjects = make(map[string]map[string]time.Time)
	}
	set := sc.committedInjects[key]
	if set == nil {
		set = make(map[string]time.Time)
		sc.committedInjects[key] = set
	}
	set[clientMessageID] = now
	pruneInjectLedgerLocked(sc.committedInjects, key, now)
	sc.sweepInjectLedgersLocked(now)
}

// RetractInject marks a client_message_id as cancelled for a route. If the
// matching follow-up is still sitting in injectCh (not yet drained), the agent
// loop's drain-time check (ConsumeInjectRetracted via injectRetractedChecker)
// will skip it so a cancelled steering message never reaches the model. Safe to
// call for an id that was already drained or never existed — it just leaves a
// TTL-reaped tombstone.
func (sc *SessionCache) RetractInject(key, clientMessageID string) {
	if key == "" || clientMessageID == "" {
		return
	}
	sc.mu.Lock()
	sc.plantRetractTombstoneLocked(key, clientMessageID, time.Now())
	sc.mu.Unlock()
}

// plantRetractTombstoneLocked records a retraction tombstone. Caller must hold
// sc.mu.
func (sc *SessionCache) plantRetractTombstoneLocked(key, clientMessageID string, now time.Time) {
	// Defensive lazy-init: NewSessionCache seeds this map, but a raw
	// &SessionCache{} literal (some tests) leaves it nil, and a nil-map write
	// panics. Read-only consumers tolerate nil, so this is the one writer that
	// must guard.
	if sc.retractedInjects == nil {
		sc.retractedInjects = make(map[string]map[string]time.Time)
	}
	set := sc.retractedInjects[key]
	if set == nil {
		set = make(map[string]time.Time)
		sc.retractedInjects[key] = set
	}
	set[clientMessageID] = now
	pruneInjectLedgerLocked(sc.retractedInjects, key, now)
	sc.sweepInjectLedgersLocked(now)
}

// RetractInjectWithStatus is the atomic retract used by the HTTP endpoint. It
// resolves, under one critical section, which side of the race this retract
// landed on:
//
//   - "already_committed": the follow-up already passed the retraction filter
//     and entered (or is entering) an LLM turn — its text lives in the session
//     as a user message. The client must NOT re-send the same text as a fresh
//     message; doing so is the force-send / pop-back duplicate.
//   - "retracted": a tombstone is planted. Any copy still in flight — buffered
//     in injectCh, surviving into the mailbox, or arriving late on a future
//     run — will be dropped at its consumption point.
//
// The commit marking happens inside the same sc.mu section as the drain filter
// (ConsumeInjectRetractedOrMarkCommitted / DrainSurvivorsOrCloseInject), so an
// id can never be reported "retracted" while the loop is concurrently
// committing it.
func (sc *SessionCache) RetractInjectWithStatus(key, clientMessageID string) string {
	if key == "" || clientMessageID == "" {
		return "retracted"
	}
	now := time.Now()
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if set := sc.committedInjects[key]; set != nil {
		if at, ok := set[clientMessageID]; ok && now.Sub(at) <= injectLedgerTTL {
			return "already_committed"
		}
	}
	sc.plantRetractTombstoneLocked(key, clientMessageID, now)
	return "retracted"
}

// ConsumeInjectRetracted reports whether a client_message_id was retracted for a
// route, removing it from the set (one-shot) so the tombstone does not linger.
// Called by the agent loop at drain time for each follow-up carrying a client
// id; a true result means "drop this follow-up, the user cancelled it".
func (sc *SessionCache) ConsumeInjectRetracted(key, clientMessageID string) bool {
	if key == "" || clientMessageID == "" {
		return false
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.consumeInjectRetractedLocked(key, clientMessageID, time.Now())
}

// consumeInjectRetractedLocked is the lock-held core of ConsumeInjectRetracted.
func (sc *SessionCache) consumeInjectRetractedLocked(key, clientMessageID string, now time.Time) bool {
	set := sc.retractedInjects[key]
	if set == nil {
		return false
	}
	at, ok := set[clientMessageID]
	if !ok {
		return false
	}
	delete(set, clientMessageID)
	if len(set) == 0 {
		delete(sc.retractedInjects, key)
	}
	// An expired tombstone is a miss — the follow-up proceeds. Deleted above
	// either way (lazy reap).
	return now.Sub(at) <= injectLedgerTTL
}

// ConsumeInjectRetractedOrMarkCommitted is the drain-time checker wired into
// the agent loop (filterRetractedInjects). For each drained follow-up it
// atomically resolves the id to exactly one of two fates:
//
//   - retracted (returns true): tombstone consumed, the loop drops it;
//   - committed (returns false): recorded in the committed ledger BEFORE the
//     loop appends the user turn, so a concurrent RetractInjectWithStatus
//     answers "already_committed" instead of planting a useless tombstone and
//     letting the client re-send the text.
//
// Both transitions happen under one sc.mu section — there is no interleaving
// where a retract slips between the filter check and the commit marking.
func (sc *SessionCache) ConsumeInjectRetractedOrMarkCommitted(key, clientMessageID string) bool {
	if key == "" || clientMessageID == "" {
		return false
	}
	now := time.Now()
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.consumeInjectRetractedLocked(key, clientMessageID, now) {
		return true
	}
	sc.markInjectCommittedLocked(key, clientMessageID, now)
	return false
}

// WasInjectCommitted reports whether a client_message_id entered an LLM turn
// on this route within the ledger TTL. Read-only (no consume).
func (sc *SessionCache) WasInjectCommitted(key, clientMessageID string) bool {
	if key == "" || clientMessageID == "" {
		return false
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	set := sc.committedInjects[key]
	if set == nil {
		return false
	}
	at, ok := set[clientMessageID]
	return ok && time.Since(at) <= injectLedgerTTL
}

// CancelRoute cancels the in-flight run for a route without waiting.
// Used by the hard cancel API endpoint.
//
// entry.mu is held for the entire duration of an in-flight run (acquired by
// LockRouteWithManager, released by UnlockRoute). We must NOT acquire it here
// — that would block until the run finishes, making cancel a no-op.
//
// Instead, we operate entirely under sc.mu:
//   - If entry.cancel is set, call it immediately (run is active).
//   - If entry.cancel is nil but the entry exists, set cancelPending so the
//     runner picks it up via SetRouteCancel before entering loop.Run. This
//     covers the narrow window between LockRouteWithManager returning and
//     route.cancel being registered.
//   - If the route key has no entry in the cache yet, this is a no-op (the
//     API layer still returns "cancelled" for idempotency, but no pending
//     intent is stored — the key must appear in sc.routes for pending to work).
func (sc *SessionCache) CancelRoute(key string) {
	sc.CancelRouteWithReason(key, agenttypes.ReasonUserCancel)
}

// CancelRouteWithReason is the reason-tagged variant of CancelRoute. The
// reason is recorded on the route entry (for pending cancels) and, when a
// CancelCauseFunc is available, threaded through context.WithCancelCause
// so the agent loop's finalizer can recover it via
// agenttypes.ExtractReason(context.Cause(ctx)).
//
// When only the legacy context.CancelFunc is registered (older code paths
// that never called SetRouteCancelCause), the reason is still stamped on
// entry.pendingReason but the cancellation itself is unparameterized.
// agenttypes.ReasonUserCancel is the safe default for those paths.
func (sc *SessionCache) CancelRouteWithReason(key string, reason agenttypes.CancelReason) {
	sc.mu.Lock()
	entry, ok := sc.routes[key]
	var (
		cancel      context.CancelFunc
		cancelCause context.CancelCauseFunc
	)
	if ok && entry != nil {
		cancel = entry.cancel
		cancelCause = entry.cancelCause
		// Mark cancellation synchronously even when an active cancel function
		// exists. Desktop interrupt-send closes/cancels the current run and
		// immediately POSTs the replacement message; the inject gate must see
		// that this route is winding down and start a fresh RunAgent instead
		// of writing the replacement into the dying loop's injectCh.
		entry.cancelPending = true
		entry.pendingReason = reason
	}
	sc.mu.Unlock()
	if cancelCause != nil {
		cancelCause(agenttypes.NewCancelError(reason))
		return
	}
	if cancel != nil {
		cancel()
	}
}

// CancelRouteForRestore cancels the route's in-flight run, waits up to the
// supplied timeout for the run to finish, and (when restoreLast is true and
// the session permits) slices the most recent user message off the session,
// returning it for restoration into a UI input box.
//
// Returns:
//   - restored != nil: the user message that was sliced.
//   - restored == nil, err == nil: cancelled successfully but conditions for
//     restore weren't met (no run, no user message, content followed it).
//   - err == ErrCancelRestoreTimeout: the run didn't exit within timeout;
//     the session was NOT mutated (we don't slice optimistically while the
//     finalizer may still write).
//   - other err: session load/save failed.
func (sc *SessionCache) CancelRouteForRestore(key string, reason agenttypes.CancelReason, restoreLast bool, timeout time.Duration) (*session.RestoredMessage, error) {
	if key == "" {
		return nil, errors.New("route key required")
	}

	sc.mu.Lock()
	entry := sc.routes[key]
	sc.mu.Unlock()

	if entry == nil {
		return nil, nil
	}

	// Capture cancel handle + done channel under sc.mu so a concurrent
	// ClearRouteRunState can't yank them out from under us.
	sc.mu.Lock()
	cancelCause := entry.cancelCause
	cancel := entry.cancel
	doneCh := entry.done
	mgr := entry.manager
	sessID := entry.loadSessionID()
	sc.mu.Unlock()

	switch {
	case cancelCause != nil:
		cancelCause(agenttypes.NewCancelError(reason))
	case cancel != nil:
		cancel()
	default:
		// No active cancel handle yet — mark pending and return.
		sc.mu.Lock()
		entry.cancelPending = true
		entry.pendingReason = reason
		sc.mu.Unlock()
		return nil, nil
	}

	if !restoreLast {
		return nil, nil
	}

	// Wait for the run to actually exit before mutating the session.
	if doneCh != nil {
		select {
		case <-doneCh:
		case <-time.After(timeout):
			return nil, ErrCancelRestoreTimeout
		}
	}

	if mgr == nil || sessID == "" {
		return nil, nil
	}

	// At this point the run has exited, so it's safe to acquire entry.mu
	// briefly to mutate the session under the same lock the runner uses.
	entry.mu.Lock()
	defer entry.mu.Unlock()

	sess, err := mgr.Resume(sessID)
	if err != nil {
		return nil, fmt.Errorf("resume session for restore: %w", err)
	}
	restored, ok := sess.SliceBeforeLastUser()
	if !ok {
		return nil, nil
	}
	if err := mgr.Save(); err != nil {
		return nil, fmt.Errorf("save sliced session: %w", err)
	}
	return restored, nil
}

// ErrCancelRestoreTimeout signals that the in-flight run did not exit
// within the deadline supplied to CancelRouteForRestore. Callers should
// translate this to HTTP 504 — the session was not mutated, so it's safe
// to retry.
var ErrCancelRestoreTimeout = errors.New("agent run did not exit within restore timeout")

// CancelBySessionID cancels any active route whose sessionID matches,
// regardless of route key type (agent:<name>, session:<id>, default:<s>:<c>).
func (sc *SessionCache) CancelBySessionID(sessionID string) {
	sc.mu.Lock()
	var cancels []context.CancelFunc
	for _, entry := range sc.routes {
		// loadSessionID is lock-free; entry.cancel/cancelPending are
		// protected by sc.mu (per SetRouteCancel's documented invariant).
		if entry != nil && entry.loadSessionID() == sessionID {
			if entry.cancel != nil {
				cancels = append(cancels, entry.cancel)
			} else {
				entry.cancelPending = true
			}
		}
	}
	sc.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// ClearSessionBindings drops in-memory route-to-session bindings for a session
// that was reset or deleted. Persisted index rows are cleared by the session
// store; this prevents the live daemon cache from resurrecting the old link.
//
// Two-phase locking: snapshot route entry pointers under sc.mu, then for each
// entry pre-check the lock-free atomic sessionID and only take entry.mu when it
// matches. A non-matching route (e.g. a different session mid-run) holds its
// entry.mu for the entire run, so locking every entry unconditionally would
// block this clear behind an unrelated active run until upstream HTTP timeout.
// Matching routes still acquire entry.mu to order with the runner's defer that
// re-stamps sessionID, then re-confirm under the lock before clearing.
func (sc *SessionCache) ClearSessionBindings(sessionID string) {
	if sessionID == "" {
		return
	}
	sc.mu.Lock()
	entries := make([]*routeEntry, 0, len(sc.routes))
	for _, entry := range sc.routes {
		if entry != nil {
			entries = append(entries, entry)
		}
	}
	sc.mu.Unlock()
	for _, entry := range entries {
		if entry.loadSessionID() != sessionID {
			continue
		}
		entry.mu.Lock()
		if entry.loadSessionID() == sessionID {
			entry.storeSessionID("")
		}
		entry.mu.Unlock()
	}
}

// Evict closes and removes the manager for this agent and drops matching route
// state. For active routes (in-flight run), it marks them as evicting and
// cancels — UnlockRoute finalizes cleanup when the run completes.
// IMPORTANT: sc.mu is released before per-route locking to avoid ABBA deadlock
// (other paths hold entry.mu then briefly acquire sc.mu).
func (sc *SessionCache) Evict(agent string) {
	sc.mu.Lock()
	sessionsDir := sc.sessionsDir(agent)
	if mgr, ok := sc.managers[sessionsDir]; ok && mgr != nil {
		if err := mgr.Close(); err != nil {
			log.Printf("daemon: failed to close session for agent %q: %v", agent, err)
		}
		delete(sc.managers, sessionsDir)
	}

	// Collect route keys to evict, then release sc.mu before per-route work.
	prefix := sc.agentRouteKey(agent)
	var keys []string
	for key := range sc.routes {
		if key == prefix || strings.HasPrefix(key, prefix+":") {
			keys = append(keys, key)
		}
	}
	sc.mu.Unlock()

	for _, key := range keys {
		sc.evictRoute(key)
	}
}

// evictRoute handles a single route eviction without holding sc.mu.
// The entry is never deleted from the map — it stays as a reusable shell.
// This prevents the race where LockRouteWithManager holds an orphaned entry
// and UnlockRoute can't find it to release the mutex.
func (sc *SessionCache) evictRoute(key string) {
	sc.mu.Lock()
	entry := sc.routes[key]
	se := sc.systemEvents
	// Drop the inject ledgers for an explicitly-evicted route: the agent is
	// gone, so no future run will honor a tombstone or answer already_committed
	// for it, and a fresh run on a re-created route re-plants as needed. This
	// reclaims immediately on agent deletion; the periodic sweep handles routes
	// that merely go idle without being evicted.
	delete(sc.retractedInjects, key)
	delete(sc.committedInjects, key)
	sc.mu.Unlock()
	se.Forget(key) // nil-safe; releases the route's S0 queue (best-effort)
	if entry == nil {
		return
	}

	entry.mu.Lock()
	mgr := entry.manager
	active := entry.cancel != nil || entry.done != nil
	if active {
		// Route has an in-flight run — mark for deferred cleanup.
		entry.evicting = true
		if entry.cancel != nil {
			entry.cancel()
		}
		entry.mu.Unlock()
		return // UnlockRoute will finalize when the run completes
	}
	// Nil out manager but keep entry in map — LockRouteWithManager will
	// create a fresh manager on next use (it checks entry.manager == nil).
	entry.manager = nil
	entry.mu.Unlock()

	if mgr != nil {
		if err := mgr.Close(); err != nil {
			log.Printf("daemon: failed to close session for route %q: %v", key, err)
		}
	}
}

// CloseAll cancels active routes, closes all session managers, and nils
// route managers. Route entries stay in the map so in-flight goroutines
// can still call UnlockRoute without missing the entry.
//
// cancel/done are snapshot under sc.mu (not entry.mu) to avoid blocking
// on an in-flight run's held entry.mu. This is safe because cancel() is
// idempotent and done channels only close once.
func (sc *SessionCache) CloseAll() {
	// Snapshot cancel/done for all active routes under sc.mu.
	type activeRoute struct {
		key    string
		cancel context.CancelFunc
		done   chan struct{}
	}
	sc.mu.Lock()
	var active []activeRoute
	for key, route := range sc.routes {
		if route != nil && route.cancel != nil {
			active = append(active, activeRoute{key, route.cancel, route.done})
		}
	}
	sc.mu.Unlock()

	// Cancel active routes and wait briefly — no entry.mu needed.
	for _, ar := range active {
		ar.cancel()
		if ar.done != nil {
			timer := time.NewTimer(5 * time.Second)
			select {
			case <-ar.done:
			case <-timer.C:
				log.Printf("daemon: timed out waiting for route %q to stop", ar.key)
			}
			timer.Stop()
		}
	}

	// Now all runs are stopped — safe to close managers.
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for sessionsDir, mgr := range sc.managers {
		if err := mgr.Close(); err != nil {
			log.Printf("daemon: failed to close session for %q: %v", sessionsDir, err)
		}
	}
	for key, route := range sc.routes {
		if route != nil && route.manager != nil {
			if err := route.manager.Close(); err != nil {
				log.Printf("daemon: failed to close session for route %q: %v", key, err)
			}
			route.manager = nil
		}
	}
	sc.managers = make(map[string]*session.Manager)
}

// ResolveLatestSession returns a snapshot of the latest session for a route.
// Uses TryLock on entry.mu — returns ErrRouteActive if a run is in progress
// to avoid reading session state while it's being mutated.
func (sc *SessionCache) ResolveLatestSession(routeKey string, sessionsDir string) (*session.Session, error) {
	if sessionsDir != "" {
		resolved, err := filepath.EvalSymlinks(filepath.Dir(sessionsDir))
		if err == nil {
			sessionsDir = filepath.Join(resolved, filepath.Base(sessionsDir))
		}
		root, _ := filepath.EvalSymlinks(sc.shannonDir)
		if root == "" {
			root = filepath.Clean(sc.shannonDir)
		}
		if !strings.HasPrefix(filepath.Clean(sessionsDir), root+string(filepath.Separator)) {
			return nil, fmt.Errorf("sessions dir %q is outside shannon root %q", sessionsDir, root)
		}
	}
	sc.mu.Lock()
	entry, ok := sc.routes[routeKey]
	if !ok {
		entry = &routeEntry{lastAccess: time.Now()}
		sc.routes[routeKey] = entry
	}
	if entry.manager == nil && sessionsDir != "" {
		entry.manager = sc.newManager(sessionsDir)
	}
	sc.mu.Unlock()
	if entry.manager == nil {
		return nil, fmt.Errorf("no route entry for %q", routeKey)
	}

	if !entry.mu.TryLock() {
		return nil, ErrRouteActive
	}
	defer entry.mu.Unlock()

	// Resolve the latest INTERACTIVE session (never a schedule/IM session) so
	// heartbeat's "your current conversation context" stays accurate under
	// named-agent multi-session. An agent with only schedule/IM sessions
	// resolves nil here → "no interactive session" error → the caller (the sole
	// production caller is heartbeat) cleanly skips.
	sess, err := entry.manager.ResumeLatestMatching(isInteractiveSource)
	if err != nil || sess == nil {
		return nil, fmt.Errorf("no interactive session for route %q", routeKey)
	}
	return cloneSessionSnapshot(sess), nil
}

// AppendToSession appends messages to the latest session for a route without
// canceling any in-flight run. Returns ErrRouteActive if a run is in progress
// (entry.mu held) to avoid concurrent session mutation.
func (sc *SessionCache) AppendToSession(routeKey string, sessionsDir string, expectedSessionID string, messages []client.Message) error {
	sc.mu.Lock()
	entry, ok := sc.routes[routeKey]
	sc.mu.Unlock()
	if !ok || entry.manager == nil {
		return fmt.Errorf("no route entry for %q", routeKey)
	}

	// Ensure no concurrent routed run is mutating the session.
	if !entry.mu.TryLock() {
		return ErrRouteActive
	}
	defer entry.mu.Unlock()

	// Re-resolve the latest interactive session (matching the read in
	// ResolveLatestSession). If the user switched interactive sessions between
	// snapshot and append, this differs from expectedSessionID → ErrSessionChanged.
	sess, err := entry.manager.ResumeLatestMatching(isInteractiveSource)
	if err != nil || sess == nil {
		return fmt.Errorf("no interactive session for route %q", routeKey)
	}
	if sess.ID != expectedSessionID {
		return ErrSessionChanged
	}

	sess.Messages = append(sess.Messages, messages...)
	now := time.Now()
	for range messages {
		sess.MessageMeta = append(sess.MessageMeta, session.MessageMeta{Source: "heartbeat", Timestamp: &now})
	}
	sess.UpdatedAt = now
	return entry.manager.Save()
}

// SessionsDir returns the sessions directory for the given agent.
// Empty agent name returns the default sessions directory.
func (sc *SessionCache) SessionsDir(agent string) string {
	return sc.sessionsDir(agent)
}

func (sc *SessionCache) sessionsDir(agent string) string {
	if agent == "" {
		return filepath.Join(sc.shannonDir, "sessions")
	}
	return filepath.Join(sc.shannonDir, "agents", agent, "sessions")
}

func (sc *SessionCache) agentRouteKey(agent string) string {
	return "agent:" + agent
}

func (sc *SessionCache) newManager(sessionsDir string) *session.Manager {
	mgr := session.NewManager(sessionsDir)

	sess, err := mgr.ResumeLatest()
	if err != nil {
		log.Printf("daemon: failed to resume session for %q: %v (starting fresh)", sessionsDir, err)
	}
	if sess == nil {
		mgr.NewSession()
	}
	return mgr
}
