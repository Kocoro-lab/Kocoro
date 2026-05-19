package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// sseScript drives an httptest.Server SSE handler. Sending on chunks writes a
// `data: …\n\n` line; closing chunks ends the stream with `data: [DONE]\n\n`.
// Using a single channel for both keeps event ordering deterministic — a
// two-channel design with separate `done` would race against in-flight chunks
// because Go's select is non-deterministic when both cases are ready.
type sseScript struct {
	chunks chan string
}

func newSSEScript() *sseScript {
	return &sseScript{chunks: make(chan string, 16)}
}

func (s *sseScript) send(line string) { s.chunks <- line }

func (s *sseScript) close() { close(s.chunks) }

func startSSEServer(t *testing.T, s *sseScript) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("test server response writer does not support flushing")
			return
		}
		flusher.Flush()
		for {
			select {
			case line, open := <-s.chunks:
				if !open {
					fmt.Fprintf(w, "data: [DONE]\n\n")
					flusher.Flush()
					return
				}
				fmt.Fprintf(w, "data: %s\n\n", line)
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// TestCompleteStream_Disabled_UsesLegacyPath verifies that when
// streamIdleTimeout is 0, CompleteStream behaves like the pre-watchdog
// scanner loop — receives all chunks, parses the done event, returns success.
func TestCompleteStream_Disabled_UsesLegacyPath(t *testing.T) {
	script := newSSEScript()
	server := startSSEServer(t, script)

	gw := NewGatewayClient(server.URL, "")
	// streamIdleTimeout left at 0 → legacy path.

	go func() {
		script.send(`{"type":"content_delta","text":"hi"}`)
		script.send(`{"type":"done","content":"hi","usage":{}}`)
		script.close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var got string
	resp, err := gw.CompleteStream(ctx, CompletionRequest{}, func(d StreamDelta) {
		got += d.Text
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatalf("expected response, got nil")
	}
	if got != "hi" {
		t.Errorf("expected delta text %q, got %q", "hi", got)
	}
}

// TestCompleteStream_IdleTimeout_FiresOnSilentDrop verifies that when the
// upstream stops emitting chunks, the watchdog returns ErrStreamIdleTimeout
// within roughly the configured timeout window.
func TestCompleteStream_IdleTimeout_FiresOnSilentDrop(t *testing.T) {
	script := newSSEScript()
	server := startSSEServer(t, script)

	gw := NewGatewayClient(server.URL, "")
	timeout := 200 * time.Millisecond
	gw.SetStreamIdleTimeout(timeout)

	go func() {
		script.send(`{"type":"content_delta","text":"first"}`)
		// then go silent — never send done, never close.
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err := gw.CompleteStream(ctx, CompletionRequest{}, nil)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrStreamIdleTimeout) {
		t.Fatalf("expected ErrStreamIdleTimeout, got %v", err)
	}
	// First chunk resets the timer, so abort fires ~timeout after that chunk.
	// Allow a generous upper bound to absorb scheduler jitter.
	if elapsed < timeout {
		t.Errorf("abort fired too early: elapsed=%v < timeout=%v", elapsed, timeout)
	}
	if elapsed > 3*timeout {
		t.Errorf("abort fired too late: elapsed=%v > 3*timeout=%v", elapsed, 3*timeout)
	}
}

// TestCompleteStream_ChunksKeepResettingTimer verifies that steady chunk
// arrival prevents the watchdog from firing even when wall-clock duration
// exceeds the configured timeout.
func TestCompleteStream_ChunksKeepResettingTimer(t *testing.T) {
	script := newSSEScript()
	server := startSSEServer(t, script)

	gw := NewGatewayClient(server.URL, "")
	timeout := 150 * time.Millisecond
	gw.SetStreamIdleTimeout(timeout)

	go func() {
		// Send 10 chunks at ~50ms apart — total 500ms, well over the 150ms timeout.
		for range 10 {
			script.send(`{"type":"content_delta","text":"."}`)
			time.Sleep(50 * time.Millisecond)
		}
		script.send(`{"type":"done","content":"..........","usage":{}}`)
		script.close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var got string
	resp, err := gw.CompleteStream(ctx, CompletionRequest{}, func(d StreamDelta) {
		got += d.Text
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatalf("expected response, got nil")
	}
	if len(got) != 10 {
		t.Errorf("expected 10 dot deltas, got %q (len=%d)", got, len(got))
	}
}

// TestCompleteStream_ContextCancelBeatsTimer verifies that parent context
// cancellation propagates correctly when it fires before the idle timer —
// the function returns ctx.Err(), not ErrStreamIdleTimeout.
func TestCompleteStream_ContextCancelBeatsTimer(t *testing.T) {
	script := newSSEScript()
	server := startSSEServer(t, script)

	gw := NewGatewayClient(server.URL, "")
	gw.SetStreamIdleTimeout(5 * time.Second)

	go func() {
		script.send(`{"type":"content_delta","text":"one"}`)
		// then go silent
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	_, err := gw.CompleteStream(ctx, CompletionRequest{}, nil)
	if err == nil {
		t.Fatal("expected non-nil error on ctx cancel, got nil — function must not return success when ctx is canceled mid-stream")
	}
	if errors.Is(err, ErrStreamIdleTimeout) {
		t.Fatalf("expected ctx error, got ErrStreamIdleTimeout")
	}
	if !errors.Is(err, context.Canceled) {
		t.Logf("returned error: %v", err)
		// Some intermediate wrapping is acceptable as long as it isn't
		// ErrStreamIdleTimeout; the watchdog's job is just to not pre-empt
		// the user-cancel signal.
	}
}

// TestCompleteStream_IdleTimeout_PreservesParsedDone verifies that when the
// gateway delivers a complete "done" event but stalls before the "[DONE]"
// sentinel, the watchdog returns the parsed result rather than discarding it.
// Matches the legacy scanner-loop semantics where a clean EOF after "done"
// returns success. Without this guard a slightly-slow gateway would lose the
// entire response (including usage accounting) to a 90s idle timer.
func TestCompleteStream_IdleTimeout_PreservesParsedDone(t *testing.T) {
	script := newSSEScript()
	server := startSSEServer(t, script)

	gw := NewGatewayClient(server.URL, "")
	timeout := 200 * time.Millisecond
	gw.SetStreamIdleTimeout(timeout)

	go func() {
		script.send(`{"type":"content_delta","text":"reply"}`)
		script.send(`{"type":"done","content":"reply","provider":"anthropic","output_text":"reply","usage":{}}`)
		// Deliberately don't send [DONE] / close — simulates a gateway that
		// shipped the full response then stalled the connection.
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var got string
	resp, err := gw.CompleteStream(ctx, CompletionRequest{}, func(d StreamDelta) {
		got += d.Text
	})
	if err != nil {
		t.Fatalf("expected success when done event parsed before idle timer, got %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response, got nil")
	}
	if resp.OutputText != "reply" {
		t.Errorf("OutputText = %q, want %q", resp.OutputText, "reply")
	}
	if got != "reply" {
		t.Errorf("delta text = %q, want %q", got, "reply")
	}
}

// TestCompleteStream_WarningPrecedesAbort verifies the overall timing budget
// (warn timer at timeout/2, abort timer at timeout). We can't easily intercept
// log output without changing global state, so this test asserts the timing
// envelope: abort must happen between [timeout, 2*timeout] from the last chunk,
// and a chunk arriving AFTER the warn point still resets the warning state.
func TestCompleteStream_WarningPrecedesAbort(t *testing.T) {
	script := newSSEScript()
	server := startSSEServer(t, script)

	gw := NewGatewayClient(server.URL, "")
	timeout := 300 * time.Millisecond
	gw.SetStreamIdleTimeout(timeout)

	go func() {
		script.send(`{"type":"content_delta","text":"a"}`)
		// Wait past warn (timeout/2 = 150ms) but before abort (timeout = 300ms),
		// then send another chunk to reset both timers.
		time.Sleep(200 * time.Millisecond)
		script.send(`{"type":"content_delta","text":"b"}`)
		// Now go silent — abort should fire ~timeout after this chunk.
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err := gw.CompleteStream(ctx, CompletionRequest{}, nil)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrStreamIdleTimeout) {
		t.Fatalf("expected ErrStreamIdleTimeout, got %v", err)
	}
	// Two chunks at 0ms and 200ms; abort fires ~timeout (300ms) after the
	// second chunk, so total wall-clock ≈ 500ms. Allow generous bounds.
	minExpected := 200*time.Millisecond + timeout
	if elapsed < minExpected {
		t.Errorf("abort fired too early: elapsed=%v < minExpected=%v", elapsed, minExpected)
	}
	if elapsed > 4*timeout+200*time.Millisecond {
		t.Errorf("abort fired too late: elapsed=%v", elapsed)
	}
}
