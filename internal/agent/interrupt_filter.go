package agent

import (
	"context"
	"sync"

	"github.com/Kocoro-lab/ShanClaw/internal/agenttypes"
)

// InterruptFilteredContext wraps parent with a child context that propagates
// every cancellation EXCEPT ReasonInterrupt. The wrapped context inherits
// all parent.Value()s (CWD, audit logger, approval broker, request-id) via
// context.WithoutCancel — we never use context.Background(), which would
// silently drop those values.
//
// Purpose (Phase 6): when a queued user message arrives mid-tool, the
// daemon cancels the active context with cause=ReasonInterrupt to signal
// "interrupt and immediately drain the queue". Tools that are safe to
// abort mid-execution (file_read, glob, grep, etc. — see
// IsCancelableMidTurn) opt into receiving this cancel directly. Tools
// that are NOT safe (bash, file_write, archive_extract, network POSTs)
// must finish their work before the queued message becomes the next
// user turn; for them, this wrapper swallows the Interrupt cancel and
// forwards every other cancel (UserCancel, Background, IdleTimeout) as-is.
//
// Implementation: we derive from context.WithoutCancel(parent) so the
// child's lifetime is independent from parent's cancel signal — only
// our explicit cancel call (triggered by non-Interrupt parent cancels or
// by the caller's defer) ends the child. Values inherited from parent
// still resolve via the WithoutCancel wrapper.
//
// The wrapper returns (child, cancel). Caller must call cancel when the
// tool completes to release the goroutine watching parent.Done().
func InterruptFilteredContext(parent context.Context) (context.Context, context.CancelFunc) {
	// WithoutCancel breaks parent's cancel chain but preserves values.
	// WithCancelCause then gives us a fresh, independently-cancellable
	// context that we drive based on parent.Done() inspection.
	base := context.WithoutCancel(parent)
	child, cancel := context.WithCancelCause(base)

	stop := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		select {
		case <-parent.Done():
			cause := context.Cause(parent)
			if r, ok := agenttypes.ExtractReason(cause); ok && r == agenttypes.ReasonInterrupt {
				// Swallow — tool is blocking, let it finish naturally.
				return
			}
			cancel(cause)
		case <-stop:
			return
		}
	}()

	wrappedCancel := func() {
		cancel(context.Canceled)
		// Signal the watcher to exit if parent hasn't fired yet.
		stopOnce.Do(func() { close(stop) })
	}
	return child, wrappedCancel
}

// dispatchCtx returns the context a tool should receive based on its
// CancelableMidTurn classification:
//
//   - Cancelable tools (file_read, glob, grep, etc.) get the parent context
//     directly. They see every cancel signal including ReasonInterrupt.
//   - Blocking tools (bash, file_write, HTTP, GUI, paid-quota network) get
//     an InterruptFilteredContext wrapper. They are immune to mid-turn
//     submit-driven Interrupt cancels but still respect UserCancel and
//     watchdog IdleTimeout.
//
// The second return value is a cancel func that the caller MUST invoke
// (typically via defer) to release the watcher goroutine. For cancelable
// tools the cancel is a no-op (returns nil); the caller's defer is still
// safe because the docs say "if cancel != nil, defer cancel()".
func dispatchCtx(parent context.Context, t Tool) (context.Context, context.CancelFunc) {
	if IsCancelableMidTurn(t) {
		return parent, nil
	}
	return InterruptFilteredContext(parent)
}
