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
	done    chan struct{} // closed when the current run goroutine exits; nil when not running
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
	done := make(chan struct{})
	w.cancel = cancel
	w.done = done
	w.running = true
	w.mu.Unlock()

	go func() {
		defer func() {
			w.mu.Lock()
			w.running = false
			w.cancel = nil
			w.done = nil
			w.mu.Unlock()
			close(done) // unblock any Restart waiting to join this run
		}()
		w.client.RunWithReconnect(runCtx)
	}()
}

// Restart stops the current run (if any) and starts a fresh one once the old
// goroutine has fully drained. Stop()+Start() cannot be used for an in-place
// key swap: Stop only cancels the run context, leaving `running` true until
// the goroutine's deferred cleanup, so an immediately-following Start
// short-circuits and the WS never reconnects with the new key (the
// account-switch hazard). Restart joins the old goroutine via its done
// channel before starting — in the background, so HTTP callers (AdoptKey)
// don't block on the WS handshake. No-op-safe when not running.
func (w *WSController) Restart(_ context.Context) {
	w.mu.Lock()
	cancel := w.cancel
	done := w.done
	w.cancel = nil
	w.mu.Unlock()

	if cancel == nil {
		w.Start(w.parent) // nothing running — just start
		return
	}
	cancel()
	go func() {
		if done != nil {
			<-done // wait for the old run's defer to flip running → false
		}
		w.Start(w.parent)
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
