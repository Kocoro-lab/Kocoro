package client

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

type SSEEvent struct {
	ID    string
	Event string
	Data  string
}

// StreamSSE streams SSE from url with the legacy one-shot contract: a single
// connection, no idle watchdog, no reconnect. A clean EOF (with or without an
// `event: done`) returns nil. Equivalent to StreamSSEWithOptions with a zero
// options value.
func StreamSSE(ctx context.Context, url string, apiKey string, handler func(SSEEvent)) error {
	return StreamSSEWithOptions(ctx, url, apiKey, StreamSSEOptions{}, handler)
}

// reconnectMaxBackoff caps the exponential reconnect backoff.
const reconnectMaxBackoff = 30 * time.Second

// StreamSSEOptions configures StreamSSEWithOptions. The zero value reproduces
// the legacy one-shot StreamSSE behavior (no idle watchdog, no reconnect).
type StreamSSEOptions struct {
	// IdleTimeout aborts the current connection when no line (event OR
	// heartbeat comment) arrives within this window, then reconnects if the
	// reconnect budget allows. Cloud pings every 10s, so set this to a small
	// multiple (e.g. 45s) — it detects dead connections, it does NOT bound
	// total workflow duration (that is the caller's ctx deadline). 0 disables.
	IdleTimeout time.Duration

	// MaxReconnects bounds reconnect attempts after a connection FAILURE
	// (idle timeout or read/connect error). An orderly EOF without a `done`
	// terminator is treated as end-of-stream, not a failure, so it does not
	// reconnect. 0 disables reconnect entirely.
	MaxReconnects int

	// ReconnectBackoffBase is the first backoff delay; it doubles each attempt
	// up to reconnectMaxBackoff. 0 defaults to 1s. Exposed for fast tests.
	ReconnectBackoffBase time.Duration
}

// ErrSSEIdleTimeout is returned by streamSSEOnce when no line arrived within
// the idle window. The reconnect loop treats it as a recoverable disconnect.
var ErrSSEIdleTimeout = fmt.Errorf("sse: idle timeout (no data within window)")

// StreamSSEWithOptions streams SSE from url, calling handler for each event.
// With reconnect enabled it persists the last seen `id:` and resumes via the
// Last-Event-ID header after an unexpected disconnect; cloud's ReplaySince is
// strictly seq>N so resumed streams never re-deliver a seen event. Returns nil
// when the stream ends with `event: done`, ctx.Err() on cancellation, or the
// last connection error once the reconnect budget is exhausted.
func StreamSSEWithOptions(ctx context.Context, url, apiKey string, opts StreamSSEOptions, handler func(SSEEvent)) error {
	backoffBase := opts.ReconnectBackoffBase
	if backoffBase <= 0 {
		backoffBase = time.Second
	}
	lastEventID := ""
	attempt := 0
	for {
		done, err := streamSSEOnce(ctx, url, apiKey, lastEventID, opts.IdleTimeout, func(ev SSEEvent) {
			if ev.ID != "" {
				lastEventID = ev.ID
			}
			handler(ev)
		})
		if done {
			return nil
		}
		// Clean EOF (orderly FIN) without an `event: done`: treat as
		// end-of-stream, do NOT reconnect. Reconnect is reserved for genuine
		// connection failures (idle timeout, read/connect error) where
		// resuming with Last-Event-ID is correct. An orderly server close is
		// end-of-stream; the caller (cloudflow) recovers any result via the
		// REST /tasks/{id} fallback — cheaper and more reliable than re-reading
		// the SSE. Also preserves the legacy StreamSSE contract (clean EOF → nil).
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if opts.MaxReconnects <= 0 || attempt >= opts.MaxReconnects {
			return err
		}
		attempt++
		// First reconnect is immediate: a transient drop usually resumes
		// instantly via Last-Event-ID, so don't pay backoffBase before the
		// cheap retry. Exponential backoff applies from the second attempt on.
		var backoff time.Duration
		if attempt > 1 {
			backoff = min(backoffBase<<uint(attempt-2), reconnectMaxBackoff)
		}
		log.Printf("client: SSE reconnect %d/%d after %s (last_event_id=%q): %v",
			attempt, opts.MaxReconnects, backoff, lastEventID, err)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// sseLineMsg carries one scanned SSE line (or a terminal signal) from the
// scanner goroutine to the read loop. Package-level so the type is nameable by
// both the goroutine and the loop (an inline anonymous struct in a channel
// type is not assignable to a named-type channel — channel element types must
// be identical, not merely structurally equal).
type sseLineMsg struct {
	line string
	eof  bool
	err  error
}

// streamSSEOnce runs one connection attempt. done=true means the stream ended
// with `event: done`. On idle timeout / read error / non-200 it returns a
// non-nil err so the caller may reconnect. lastEventID, when non-empty, is sent
// as the Last-Event-ID header to resume.
func streamSSEOnce(ctx context.Context, url, apiKey, lastEventID string, idleTimeout time.Duration, handler func(SSEEvent)) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("SSE connect failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("SSE returned %d", resp.StatusCode)
	}

	// Scanner runs in a goroutine; the main goroutine multiplexes line receipt
	// against an idle timer so a silently stalled connection (no ping, no FIN)
	// is detected. Mirrors completeStreamWatchdog in gateway.go.
	lineCh := make(chan sseLineMsg, 64)
	scanCtx, scanCancel := context.WithCancel(ctx)
	defer scanCancel()
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			select {
			case lineCh <- sseLineMsg{line: scanner.Text()}:
			case <-scanCtx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case lineCh <- sseLineMsg{err: err}:
			case <-scanCtx.Done():
			}
			return
		}
		select {
		case lineCh <- sseLineMsg{eof: true}:
		case <-scanCtx.Done():
		}
	}()

	// idleC is nil when idleTimeout == 0; a receive on a nil channel blocks
	// forever, so the idle select arm never fires — equivalent to "no
	// watchdog" without branching the loop.
	var idleC <-chan time.Time
	var idleTimer *time.Timer
	if idleTimeout > 0 {
		idleTimer = time.NewTimer(idleTimeout)
		defer idleTimer.Stop()
		idleC = idleTimer.C
	}

	var current SSEEvent
	for {
		select {
		case msg := <-lineCh:
			if msg.err != nil {
				return false, fmt.Errorf("SSE read error: %w", msg.err)
			}
			if msg.eof {
				return false, nil
			}
			if idleTimer != nil {
				resetTimer(idleTimer, idleTimeout)
			}
			line := msg.line
			if strings.HasPrefix(line, ":") {
				continue // heartbeat comment
			}
			if line == "" {
				if current.Event != "" || current.Data != "" {
					if current.Event == "done" {
						return true, nil
					}
					handler(current)
					current = SSEEvent{}
				}
				continue
			}
			field, value, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "id":
				current.ID = value
			case "event":
				current.Event = value
			case "data":
				if current.Data != "" {
					current.Data += "\n" + value
				} else {
					current.Data = value
				}
			}
		case <-idleC:
			scanCancel()
			resp.Body.Close()
			return false, ErrSSEIdleTimeout
		}
	}
}
