package mcp

import (
	"log"
	"sync"
	"time"
)

// Reconnect pacing. A failed async connect used to be terminal: the result
// callback logged it, audited it, and returned, so a server that lost the
// startup race stayed "enabled but dead" until the user restarted the daemon
// (2026-08-12: rotating a Notion token left the stdio server down because the
// replacement npx process raced the SIGTERM'd one for its port).
//
// The ladder below recovers from that class of transient failure without
// hammering a genuinely broken server.
const (
	// reconnectInitialBackoff paces the first retry. Long enough for a killed
	// npx process group (npx → npm exec → node) to release its port, short
	// enough that a user who just saved a token sees the server come up while
	// still looking at the settings pane.
	reconnectInitialBackoff = 5 * time.Second

	// reconnectMaxBackoff caps the ladder so a server that is down for a long
	// outage still gets periodic attempts without a runaway timer.
	reconnectMaxBackoff = 5 * time.Minute

	// reconnectMaxAttempts bounds automatic recovery per failure streak.
	//
	// Workload: transient failures (port still held by a dying process group,
	// npx cold-fetching a package, a laptop resuming from sleep) clear within
	// seconds to a couple of minutes. Six attempts spans ~5m15s of wall clock
	// (5+10+20+40+80+160s), which covers those without retrying a genuinely
	// broken config — wrong command, revoked credential — indefinitely.
	//
	// Symptom when it binds: the server stops retrying and stays disconnected;
	// GET /config/status keeps reporting it, and the model sees none of its
	// tools.
	//
	// Override path: any explicit user action re-arms the streak via Forget —
	// POST /config/reload and the Desktop enable/disable toggle both route
	// there. There is deliberately no config knob: a server that cannot come
	// up in five minutes needs attention, not more retries.
	reconnectMaxAttempts = 6
)

// reconnectBackoff returns the delay before the given 1-based retry attempt.
func reconnectBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := attempt - 1
	// Bound the shift before applying it — 1<<64 wraps to a non-positive
	// duration, which would either fire in a hot loop or never fire at all.
	if shift >= 20 {
		return reconnectMaxBackoff
	}
	d := reconnectInitialBackoff << shift
	if d <= 0 || d > reconnectMaxBackoff {
		return reconnectMaxBackoff
	}
	return d
}

// ReconnectScheduler turns a failed MCP connect into a bounded, backed-off
// background retry. It owns no connection state of its own: the caller
// supplies the action that re-attempts a connect (in the daemon that is a
// fresh StartConnectAll for the one server), and the scheduler decides
// whether and when to run it.
//
// The zero value is not usable — construct with NewReconnectScheduler.
type ReconnectScheduler struct {
	mu       sync.Mutex
	servers  map[string]*reconnectEntry
	stopped  bool
	maxTries int

	// reconnect performs one connect attempt for the named server. It runs on
	// the timer goroutine and must not block for long.
	reconnect func(serverName string)

	// after schedules fn to run after d and returns a cancel func. Swapped in
	// tests for a deterministic clock.
	//
	// It is called while s.mu is held, so fn MUST run asynchronously —
	// time.AfterFunc does. A substitute that invokes fn inline would deadlock
	// on the re-entrant lock in fire.
	after func(d time.Duration, fn func()) func()
}

type reconnectEntry struct {
	attempts int
	// cancel stops the pending retry, nil when nothing is scheduled.
	cancel func()
}

// NewReconnectScheduler returns a scheduler that calls reconnect for each
// retry. A nil reconnect degrades to scheduling no-ops rather than panicking
// on the timer goroutine.
func NewReconnectScheduler(reconnect func(serverName string)) *ReconnectScheduler {
	return &ReconnectScheduler{
		servers:   make(map[string]*reconnectEntry),
		maxTries:  reconnectMaxAttempts,
		reconnect: reconnect,
		after: func(d time.Duration, fn func()) func() {
			t := time.AfterFunc(d, fn)
			return func() { t.Stop() }
		},
	}
}

// OnFailure records a failed connect for serverName and schedules the next
// retry. It is a no-op when the scheduler is stopped, when a retry for this
// server is already pending, or when the server has exhausted its attempts.
func (s *ReconnectScheduler) OnFailure(serverName string) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	entry := s.servers[serverName]
	if entry == nil {
		entry = &reconnectEntry{}
		s.servers[serverName] = entry
	}
	// A pending retry already covers this failure. Two reports for the same
	// server can legitimately arrive close together (async connect result plus
	// a supervisor probe); firing both would race for the same process group.
	if entry.cancel != nil {
		s.mu.Unlock()
		return
	}
	if entry.attempts >= s.maxTries {
		s.mu.Unlock()
		log.Printf("[mcp] %s: giving up automatic reconnect after %d attempts; reload or re-enable the server to retry", serverName, entry.attempts)
		return
	}
	entry.attempts++
	delay := reconnectBackoff(entry.attempts)
	attempt := entry.attempts
	entry.cancel = s.after(delay, func() { s.fire(serverName) })
	s.mu.Unlock()

	log.Printf("[mcp] %s: scheduling reconnect attempt %d/%d in %v", serverName, attempt, s.maxTries, delay)
}

// fire runs one retry. It clears the pending marker first so a failure
// reported by the attempt itself can schedule the next rung.
func (s *ReconnectScheduler) fire(serverName string) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	if entry := s.servers[serverName]; entry != nil {
		entry.cancel = nil
	}
	reconnect := s.reconnect
	s.mu.Unlock()

	if reconnect != nil {
		reconnect(serverName)
	}
}

// OnSuccess clears the failure streak for serverName so a later failure
// starts again at the initial backoff.
func (s *ReconnectScheduler) OnSuccess(serverName string) {
	s.Forget(serverName)
}

// Forget drops all retry state for serverName, cancelling any pending
// attempt. This is the escape hatch for explicit user action: a reload or an
// enable toggle re-arms a server that had exhausted its attempts.
func (s *ReconnectScheduler) Forget(serverName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.servers[serverName]
	if entry == nil {
		return
	}
	if entry.cancel != nil {
		entry.cancel()
	}
	delete(s.servers, serverName)
}

// Stop cancels every pending retry and blocks further scheduling. Used when
// the daemon shuts down or when a reload supersedes the manager these retries
// belong to — a resurrected connection on a dead manager would leak a process
// group. Idempotent.
func (s *ReconnectScheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	s.stopped = true
	for _, entry := range s.servers {
		if entry.cancel != nil {
			entry.cancel()
			entry.cancel = nil
		}
	}
}
