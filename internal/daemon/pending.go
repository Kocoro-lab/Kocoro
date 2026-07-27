package daemon

import (
	"context"
	"log"
	"sync"
	"time"
)

// pendingEntry tracks one in-flight request inside a pendingCore. The `emitted`
// flag lets CancelAll skip cleanup events for requests whose request payload
// was never published (the transport send failed before emission).
type pendingEntry[D any] struct {
	ch      chan D
	emitted bool
}

// pendingCore is the shared request/resolve lifecycle behind both
// ApprovalBroker and QuestionBroker: a pending map keyed by request id, a
// cap-1 result channel per entry, the emit-guard critical section run under
// mu after a successful send, and the at-most-one-terminal-event / bus-ID
// ordering invariants (see Resolve / cancelAll). The two interaction kinds
// differ only in their wire face — payload types and transport send — so the
// lifecycle is factored here once rather than copied (see CLAUDE.md
// "Request/resolve interactions").
//
// D is the decision/result type buffered onto each entry's channel:
// ApprovalDecision for approvals, questionResolution for questions.
type pendingCore[D any] struct {
	mu           sync.Mutex
	pending      map[string]*pendingEntry[D]
	onRegister   func(requestID string) // called when a pending entry is created
	onDeregister func(requestID string) // called when a pending entry is cleaned up
	onCleanup    func(requestID string) // daemon-originated terminal paths (timeout/ctx/cancelAll)
}

func newPendingCore[D any]() *pendingCore[D] {
	return &pendingCore[D]{pending: make(map[string]*pendingEntry[D])}
}

// run drives one request through the shared lifecycle: register → send →
// emit-guard → wait. It blocks until a decision is delivered via Resolve, the
// timeout elapses, ctx is cancelled, or cancelAll fires.
//
//   - daemonTerminal is the value returned (and buffered onto pa.ch by
//     cancelAll) when a daemon-originated path terminates the request instead
//     of an external Resolve: send failure, timeout, ctx cancel, cancelAll.
//   - send performs the transport publish (WS/SSE). It must be reconnect-safe
//     (a method on the transport, not a closure over a live conn). On error the
//     request is abandoned WITHOUT firing onCleanup — nothing was ever shown.
//   - emit publishes the request payload to the bus. It runs inside the broker
//     mutex once send succeeds so it cannot race a concurrent cancelAll.
func (c *pendingCore[D]) run(ctx context.Context, reqID string, timeout time.Duration, daemonTerminal D, send func() error, emit func()) D {
	pa := &pendingEntry[D]{ch: make(chan D, 1)}

	c.mu.Lock()
	c.pending[reqID] = pa
	c.mu.Unlock()

	if c.onRegister != nil {
		c.onRegister(reqID)
	}

	defer func() {
		if c.onDeregister != nil {
			c.onDeregister(reqID)
		}
		c.mu.Lock()
		delete(c.pending, reqID)
		c.mu.Unlock()
	}()

	if err := send(); err != nil {
		return daemonTerminal
	}

	// Publish the request payload only AFTER send succeeds so the bus never
	// shows a card for a request that never reached the foreground client.
	// emit + emitted=true run inside the broker mutex as a single critical
	// section: a concurrent cancelAll (WS disconnect mid-emit) is either fully
	// sequenced before us (we observe pa missing from pending and skip emission
	// so neither event reaches the bus) or fully sequenced after us (it sees
	// emitted=true and fires the matching cleanup). Without this serialization
	// the race window between emit and emitted=true could leak an orphan
	// request payload into the ring, leaving a stale Desktop inbox card.
	c.mu.Lock()
	if _, stillPending := c.pending[reqID]; !stillPending {
		c.mu.Unlock()
		// Resolve or cancelAll terminated us between send and now. Both send to
		// pa.ch under the same mutex before deleting from pending, so a value is
		// buffered; drain it for the correct decision.
		select {
		case decision := <-pa.ch:
			return decision
		default:
			return daemonTerminal
		}
	}
	emit()
	pa.emitted = true
	c.mu.Unlock()

	select {
	case decision := <-pa.ch:
		return decision
	case <-time.After(timeout):
		if c.claimForCleanup(reqID) {
			log.Printf("daemon: pending interaction %s timed out", reqID)
			if c.onCleanup != nil {
				c.onCleanup(reqID)
			}
			return daemonTerminal
		}
		// Lost the delete race to Resolve / cancelAll. Honor the real decision
		// they buffered onto pa.ch instead of producing a conflicting daemon
		// terminal state.
		select {
		case decision := <-pa.ch:
			return decision
		default:
			return daemonTerminal
		}
	case <-ctx.Done():
		if c.claimForCleanup(reqID) {
			if c.onCleanup != nil {
				c.onCleanup(reqID)
			}
			return daemonTerminal
		}
		select {
		case decision := <-pa.ch:
			return decision
		default:
			return daemonTerminal
		}
	}
}

// claimForCleanup is the inverse of Resolve: it removes reqID from pending
// under the broker mutex and reports whether this caller won the delete race.
// Used by the timeout / ctx-cancel branches so the daemon-originated cleanup
// event is emitted only when no ingress (HTTP resolve, WS response) has already
// claimed the request. Pairs with the at-most-one terminal-event contract.
func (c *pendingCore[D]) claimForCleanup(reqID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.pending[reqID]; !ok {
		return false
	}
	delete(c.pending, reqID)
	return true
}

// Resolve delivers a decision to a pending request. Returns true if this call
// is the one that terminated the request (the entry was still in pending);
// false if a prior Resolve / cancelAll / timeout / ctx-cancel already claimed
// it. Ingress callers (HTTP resolve, WS response) use the return value to gate
// their bus emission so a daemon-cleanup path firing concurrently cannot
// produce a conflicting terminal event for the same request_id.
//
// beforeDeliver, when non-nil, runs UNDER the broker mutex AFTER the claim
// succeeds but BEFORE the decision is buffered onto pa.ch. Ingress callers pass
// a resolved-event emitter here so the bus event is guaranteed an ID earlier
// than any event the run goroutine — and the agent loop reading its decision —
// can emit after waking up. Without this serialization the run goroutine can
// resume on another P in parallel with the bus emit (pa.ch is buffered cap-1 so
// the send is non-blocking and wakes the receiver immediately), letting an
// agent tool_status land on the bus with a lower ID than the resolved event —
// Desktop would then dismiss the card AFTER observing the next tool already
// running. Pass nil from non-ingress paths (tests, internal stubs) that do not
// produce a bus event.
//
// The decision is buffered onto pa.ch WHILE the broker mutex is held so a
// concurrent run that races to acquire the lock after send-success is
// guaranteed to see either (pa still in pending, emit normally) or (pa removed
// + decision ready to drain).
func (c *pendingCore[D]) Resolve(requestID string, decision D, beforeDeliver func()) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	pa, ok := c.pending[requestID]
	if !ok {
		return false
	}
	if beforeDeliver != nil {
		beforeDeliver()
	}
	select {
	case pa.ch <- decision:
	default:
	}
	delete(c.pending, requestID)
	return true
}

// cancelAll delivers daemonTerminal to all pending requests and clears the map.
// Called on transport disconnect to unblock all waiting goroutines. Fires
// onCleanup for any pending entry whose request payload had already been
// emitted so the corresponding inbox card on Desktop is dismissed.
//
// onCleanup is invoked under the broker mutex, BEFORE the terminal value is
// buffered onto pa.ch, for the same bus-ID ordering reason documented on
// Resolve: the run goroutine becomes runnable on another P the instant the
// channel send completes, and any event its agent loop emits after seeing the
// terminal must land on the bus with a higher ID than the cleanup resolved
// event. emitBusJSON only contends the bus mutex with non-blocking subscriber
// sends, so holding the broker mutex across emits here is bounded — even at
// 200K-context-era pending depths cancelAll keeps a single-digit number of
// in-flight interactions.
func (c *pendingCore[D]) cancelAll(daemonTerminal D) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, pa := range c.pending {
		if pa.emitted && c.onCleanup != nil {
			c.onCleanup(id)
		}
		select {
		case pa.ch <- daemonTerminal:
		default:
		}
		delete(c.pending, id)
	}
}
