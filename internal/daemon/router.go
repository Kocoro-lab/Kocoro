package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

var ErrSessionChanged = errors.New("session changed since pre-check")
var ErrRouteActive = errors.New("route has an active run")

type routeEntry struct {
	mu            sync.Mutex
	cancel        context.CancelFunc
	cancelPending bool // set under sc.mu when CancelRoute fires before cancel is assigned
	done          chan struct{}
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
// - routes: one lock/cancel/inflight channel per routing key
// - managers: one shared session.Manager per sessions directory for non-routed usage
// - route manager: lazily created session.Manager per route for routed runs
type SessionCache struct {
	mu         sync.Mutex
	routes     map[string]*routeEntry
	managers   map[string]*session.Manager
	shannonDir string
}

// NewSessionCache creates a cache rooted at the given shannon directory.
func NewSessionCache(shannonDir string) *SessionCache {
	return &SessionCache{
		routes:     make(map[string]*routeEntry),
		managers:   make(map[string]*session.Manager),
		shannonDir: shannonDir,
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
	if pending {
		cancel()
	}
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
)

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
	sc.mu.Lock()
	entry, ok := sc.routes[key]
	if !ok || entry == nil {
		sc.mu.Unlock()
		return InjectNoActiveRun
	}
	ch := entry.injectCh
	done := entry.done
	activeCWD := entry.activeCWD
	sc.mu.Unlock()
	if done == nil {
		return InjectNoActiveRun
	}
	if ch == nil {
		return InjectBusy
	}
	requestCWD := normalizeCWDForCompare(msg.CWD)
	if requestCWD != "" && requestCWD != normalizeCWDForCompare(activeCWD) {
		return InjectCWDConflict
	}
	select {
	case ch <- msg:
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

// SetRouteRunState updates the externally visible run state for a route.
// This is used by injection/cancel paths that must not block on entry.mu while
// the active run holds it for the duration of execution.
func (sc *SessionCache) SetRouteRunState(key string, done chan struct{}, injectCh chan agent.InjectedMessage, activeCWD string) {
	if key == "" {
		return
	}
	sc.mu.Lock()
	entry, ok := sc.routes[key]
	if ok && entry != nil {
		entry.done = done
		entry.injectCh = injectCh
		entry.activeCWD = activeCWD
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
	sc.mu.Unlock()
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
	sc.mu.Lock()
	entry, ok := sc.routes[key]
	var cancel context.CancelFunc
	if ok && entry != nil {
		cancel = entry.cancel
		if cancel == nil {
			entry.cancelPending = true
		}
	}
	sc.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

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
// Two-phase locking: snapshot route entry pointers under sc.mu, then take
// each entry.mu so the clear blocks until any active run on a matching
// route finishes — matches the user expectation that "clear" really
// clears, even if the runner's defer would otherwise re-stamp sessionID
// after we cleared it. The actual sessionID load/store is atomic, but we
// keep the entry.mu acquire for ordering with the runner's defer.
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
	sc.mu.Unlock()
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

	sess, err := entry.manager.ResumeLatest()
	if err != nil || sess == nil {
		return nil, fmt.Errorf("no session for route %q", routeKey)
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

	sess, err := entry.manager.ResumeLatest()
	if err != nil || sess == nil {
		return fmt.Errorf("no session for route %q", routeKey)
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
