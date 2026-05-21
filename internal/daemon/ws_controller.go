package daemon

import (
	"context"
	"sync"
)

// WSController manages the WebSocket goroutine lifecycle. Before this
// existed the daemon main loop blocked on RunWithReconnect for the
// process lifetime — fine when api_key came from yaml at boot, but
// hostile to "sign in later" UX. Now AuthManager starts the WS on
// signed_in entry and stops it on sign_out exit.
//
// Stop() cancels the run context but does not Join the goroutine.
// RunWithReconnect observes ctx.Done in its own select and exits on its
// own schedule (within one Listen iteration). A subsequent Start can race
// with the still-shutting-down goroutine — the `running` flag is set
// false in the goroutine's deferred cleanup, so Start that sees true
// short-circuits and a Start that follows a fully-drained Stop sees
// false. The transient overlap is harmless because Client guards its
// own conn with writeMu.
type WSController struct {
	mu      sync.Mutex
	client  *Client
	cancel  context.CancelFunc
	running bool
	parent  context.Context
}

// NewWSController wraps a Client with start/stop control. parent is the
// daemon-level context; child contexts are derived from it so daemon
// shutdown cascades cleanly to in-flight WS goroutines.
func NewWSController(parent context.Context, client *Client) *WSController {
	return &WSController{client: client, parent: parent}
}

// Start spins up the reconnect loop if it isn't already running. The
// context passed in is ignored in favor of `parent` because we want the
// WS to outlive any HTTP request scope that triggered the start.
func (w *WSController) Start(_ context.Context) {
	w.mu.Lock()
	if w.running || w.client == nil || w.parent == nil {
		w.mu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(w.parent)
	w.cancel = cancel
	w.running = true
	w.mu.Unlock()

	go func() {
		defer func() {
			w.mu.Lock()
			w.running = false
			w.cancel = nil
			w.mu.Unlock()
		}()
		w.client.RunWithReconnect(runCtx)
	}()
}

// Stop cancels the run context. Idempotent; safe to call when not running.
func (w *WSController) Stop() {
	w.mu.Lock()
	cancel := w.cancel
	w.cancel = nil
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// IsRunning reports whether the goroutine is currently active. Useful
// in tests; production code should not branch on this (use auth state
// instead).
func (w *WSController) IsRunning() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running
}
