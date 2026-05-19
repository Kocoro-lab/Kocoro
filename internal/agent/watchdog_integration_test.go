package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// hangingLLMClient is a minimal client.LLMClient whose Complete blocks on
// ctx.Done. Used to exercise the watchdog end-to-end inside AgentLoop.Run.
type hangingLLMClient struct {
	calls atomic.Int32
}

func (h *hangingLLMClient) Complete(ctx context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
	h.calls.Add(1)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (h *hangingLLMClient) CompleteStream(ctx context.Context, req client.CompletionRequest, _ func(client.StreamDelta)) (*client.CompletionResponse, error) {
	return h.Complete(ctx, req)
}

// recordingHandler captures OnRunStatus events for assertions.
type recordingHandler struct {
	mockHandler
	mu       sync.Mutex
	statuses []string
}

func (h *recordingHandler) OnRunStatus(code, detail string) {
	h.mu.Lock()
	h.statuses = append(h.statuses, code+":"+detail)
	h.mu.Unlock()
}

func (h *recordingHandler) Statuses() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.statuses))
	copy(out, h.statuses)
	return out
}

func (h *recordingHandler) HasCode(code string) bool {
	for _, s := range h.Statuses() {
		if len(s) >= len(code) && s[:len(code)] == code {
			return true
		}
	}
	return false
}

func TestAgentLoop_Watchdog_SoftStatus_HangingClient(t *testing.T) {
	gw := &hangingLLMClient{}
	loop := NewAgentLoop(gw, NewToolRegistry(), "medium", "", 25, 2000, 200, nil, nil, nil)
	loop.SetEnableStreaming(false)
	loop.idleSoftTimeout = 30 * time.Millisecond
	loop.watchdogTick = 5 * time.Millisecond
	handler := &recordingHandler{mockHandler: mockHandler{approveResult: true}}
	loop.SetHandler(handler)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	_, _, err := loop.Run(ctx, "hello", nil, nil)
	if err == nil {
		t.Fatal("expected cancel error from hanging client")
	}

	if !handler.HasCode("idle_soft") {
		t.Fatalf("expected idle_soft status, got: %v", handler.Statuses())
	}
}

func TestAgentLoop_Watchdog_ForceStop_HardTimeout_SurfacesHardIdleError(t *testing.T) {
	// Regression for finding #4: during PhaseForceStop, completeWithRetry
	// must preserve ErrHardIdleTimeout in the error chain (via context.Cause)
	// rather than collapsing it into ctx.Err() == context.Canceled.
	gw := &hangingLLMClient{}
	loop := NewAgentLoop(gw, NewToolRegistry(), "medium", "", 25, 2000, 200, nil, nil, nil)
	loop.SetEnableStreaming(false)
	handler := &recordingHandler{mockHandler: mockHandler{approveResult: true}}
	loop.SetHandler(handler)

	// Simulate ForceStop-style call directly by entering PhaseForceStop and
	// running completeWithRetry against a ctx cancelled by a cause.
	loop.tracker = newPhaseTracker()
	loop.tracker.Enter(PhaseForceStop)

	ctx, cancel := context.WithCancelCause(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel(ErrHardIdleTimeout)
	}()

	_, err := loop.completeWithRetry(ctx, client.CompletionRequest{})
	if err == nil {
		t.Fatal("expected cancel error")
	}
	if !errors.Is(err, ErrHardIdleTimeout) {
		t.Fatalf("want ErrHardIdleTimeout via context.Cause, got: %v", err)
	}
}

func TestAgentLoop_Watchdog_HardTimeout_CancelsWithCause(t *testing.T) {
	gw := &hangingLLMClient{}
	loop := NewAgentLoop(gw, NewToolRegistry(), "medium", "", 25, 2000, 200, nil, nil, nil)
	loop.SetEnableStreaming(false)
	loop.idleSoftTimeout = 0
	loop.idleHardTimeout = 40 * time.Millisecond
	loop.watchdogTick = 5 * time.Millisecond
	handler := &recordingHandler{mockHandler: mockHandler{approveResult: true}}
	loop.SetHandler(handler)

	_, _, err := loop.Run(context.Background(), "hello", nil, nil)
	if err == nil {
		t.Fatal("expected hard-timeout error")
	}
	if !errors.Is(err, ErrHardIdleTimeout) {
		t.Fatalf("want ErrHardIdleTimeout in error chain, got: %v", err)
	}
	status := loop.LastRunStatus()
	if !status.Partial {
		t.Errorf("expected Partial=true on hard-timeout, got: %+v", status)
	}
	if !handler.HasCode("idle_hard") {
		t.Errorf("expected idle_hard status event, got: %v", handler.Statuses())
	}
}

func TestAgentLoop_Watchdog_HardZero_NoCancellation(t *testing.T) {
	// Regression guard: default rollout (hard=0) must not change
	// cancellation semantics. Run should complete on a cooperating client
	// without any watchdog-originated cancel.
	callCount := 0
	gw := fakeLLMClient{
		resp: func() *client.CompletionResponse {
			callCount++
			if callCount == 1 {
				return &client.CompletionResponse{
					OutputText:   "ok",
					FinishReason: "end_turn",
				}
			}
			return &client.CompletionResponse{OutputText: "", FinishReason: "end_turn"}
		},
	}
	loop := NewAgentLoop(&gw, NewToolRegistry(), "medium", "", 25, 2000, 200, nil, nil, nil)
	loop.SetEnableStreaming(false)
	loop.idleSoftTimeout = 10 * time.Millisecond // would fire if we stalled
	loop.idleHardTimeout = 0                     // disabled — must not cancel
	handler := &recordingHandler{mockHandler: mockHandler{approveResult: true}}
	loop.SetHandler(handler)

	text, _, err := loop.Run(context.Background(), "hi", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error with hard=0: %v", err)
	}
	if text != "ok" {
		t.Fatalf("want text %q, got %q", "ok", text)
	}
}

// fakeLLMClient is a tiny cooperating client that returns a fixed response.
type fakeLLMClient struct {
	resp func() *client.CompletionResponse
}

func (f *fakeLLMClient) Complete(ctx context.Context, _ client.CompletionRequest) (*client.CompletionResponse, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return f.resp(), nil
}

func (f *fakeLLMClient) CompleteStream(ctx context.Context, req client.CompletionRequest, _ func(client.StreamDelta)) (*client.CompletionResponse, error) {
	return f.Complete(ctx, req)
}

// streamIdleLLMClient surfaces ErrStreamIdleTimeout from CompleteStream and
// counts both Complete and CompleteStream calls. The agent loop's silent-drop
// short-circuit must prevent any retry — completeCalls MUST stay 0.
type streamIdleLLMClient struct {
	streamCalls   atomic.Int32
	completeCalls atomic.Int32
	deltaText     string // optional partial text to deliver before the timeout
}

func (s *streamIdleLLMClient) Complete(ctx context.Context, _ client.CompletionRequest) (*client.CompletionResponse, error) {
	s.completeCalls.Add(1)
	return nil, client.ErrStreamIdleTimeout
}

func (s *streamIdleLLMClient) CompleteStream(ctx context.Context, _ client.CompletionRequest, onDelta func(client.StreamDelta)) (*client.CompletionResponse, error) {
	s.streamCalls.Add(1)
	if s.deltaText != "" && onDelta != nil {
		onDelta(client.StreamDelta{Text: s.deltaText})
	}
	return nil, client.ErrStreamIdleTimeout
}

// TestAgentLoop_StreamIdleTimeout_NoRetry exercises the full agent-side
// handling of a silent stream drop:
//   - CompleteStream returns ErrStreamIdleTimeout
//   - loop short-circuits the streaming→Complete fallback (no retry)
//   - isRetryableLLMError refuses to retry the inline attempt loop
//   - OnRunStatus("stream_idle_timeout", …) fires for downstream consumers
//   - LastRunStatus().Partial == true so UIs render the "timed out, here's
//     what we have" hint instead of a hard error
func TestAgentLoop_StreamIdleTimeout_NoRetry(t *testing.T) {
	gw := &streamIdleLLMClient{deltaText: "half a sentence"}
	loop := NewAgentLoop(gw, NewToolRegistry(), "medium", "", 25, 2000, 200, nil, nil, nil)
	loop.SetEnableStreaming(true)
	handler := &recordingHandler{mockHandler: mockHandler{approveResult: true}}
	loop.SetHandler(handler)

	partial, _, err := loop.Run(context.Background(), "hello", nil, nil)
	if err == nil {
		t.Fatal("expected stream-idle error, got nil")
	}
	if !errors.Is(err, client.ErrStreamIdleTimeout) {
		t.Fatalf("want ErrStreamIdleTimeout in error chain, got: %v", err)
	}
	if partial != "half a sentence" {
		t.Errorf("expected partial text to be preserved, got %q", partial)
	}
	if got := gw.streamCalls.Load(); got != 1 {
		t.Errorf("CompleteStream calls = %d, want 1 (no retry)", got)
	}
	if got := gw.completeCalls.Load(); got != 0 {
		t.Errorf("Complete fallback calls = %d, want 0 (must short-circuit on stream_idle_timeout)", got)
	}
	if !handler.HasCode("stream_idle_timeout") {
		t.Errorf("expected stream_idle_timeout status event, got: %v", handler.Statuses())
	}
	if status := loop.LastRunStatus(); !status.Partial {
		t.Errorf("expected Partial=true on stream-idle-timeout, got: %+v", status)
	}
}
