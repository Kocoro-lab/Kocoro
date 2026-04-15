package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// TestCheckpoint_FiresAfterToolBatch verifies that when a turn executes a
// tool and returns text, the checkpoint hook fires at least once with the
// tool result in loop.RunMessages() — proving mid-turn persistence point
// is upstream of the post-turn save path.
func TestCheckpoint_FiresAfterToolBatch(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			json.NewEncoder(w).Encode(nativeResponse("", "tool_use",
				toolCall("mock_tool", `{}`), 10, 5))
			return
		}
		json.NewEncoder(w).Encode(nativeResponse("done", "end_turn", nil, 5, 3))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	reg.Register(&mockTool{name: "mock_tool"})
	loop := NewAgentLoop(gw, reg, "medium", "", 25, 2000, 200, nil, nil, nil)
	loop.SetEnableStreaming(false)
	loop.SetHandler(&mockHandler{approveResult: true})

	var checkpointCount atomic.Int32
	var checkpointMsgCount atomic.Int32
	loop.SetCheckpointFunc(func(ctx context.Context) error {
		checkpointCount.Add(1)
		checkpointMsgCount.Store(int32(len(loop.RunMessages())))
		return nil
	})

	text, _, err := loop.Run(context.Background(), "run tool", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "done" {
		t.Fatalf("want 'done', got %q", text)
	}
	if n := checkpointCount.Load(); n < 1 {
		t.Fatalf("expected >=1 checkpoint call after tool batch, got %d", n)
	}
	if msgs := checkpointMsgCount.Load(); msgs < 2 {
		// At checkpoint time we expect at minimum the assistant tool_use
		// and the corresponding tool_result in RunMessages.
		t.Fatalf("checkpoint saw only %d run messages, expected >=2", msgs)
	}
}

// TestCheckpoint_NoOpWhenNotDirty verifies that a text-only turn (no tools,
// no compaction, no force-stop) never fires the checkpoint hook.
func TestCheckpoint_NoOpWhenNotDirty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(nativeResponse("plain text reply", "end_turn", nil, 5, 3))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	loop := NewAgentLoop(gw, NewToolRegistry(), "medium", "", 25, 2000, 200, nil, nil, nil)
	loop.SetEnableStreaming(false)
	loop.SetHandler(&mockHandler{approveResult: true})

	var checkpointCount atomic.Int32
	loop.SetCheckpointFunc(func(ctx context.Context) error {
		checkpointCount.Add(1)
		return nil
	})

	_, _, err := loop.Run(context.Background(), "say hi", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := checkpointCount.Load(); n != 0 {
		t.Fatalf("text-only turn fired checkpoint %d times, want 0", n)
	}
}

// TestCheckpoint_IdempotentUnderRepeatedCalls verifies that calling the
// checkpoint from multiple fire points across a turn always sees a
// well-formed RunMessages snapshot (can be re-applied from scratch to a
// session without drift). This models the crucial "idempotent rebuild"
// requirement flagged in the plan's review.
func TestCheckpoint_IdempotentUnderRepeatedCalls(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		switch callCount {
		case 1, 2, 3:
			// Three tool iterations.
			json.NewEncoder(w).Encode(nativeResponse("", "tool_use",
				toolCall("mock_tool", fmt.Sprintf(`{"step":%d}`, callCount)), 10, 5))
		default:
			json.NewEncoder(w).Encode(nativeResponse("all done", "end_turn", nil, 5, 3))
		}
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	reg.Register(&mockTool{name: "mock_tool"})
	loop := NewAgentLoop(gw, reg, "medium", "", 25, 2000, 200, nil, nil, nil)
	loop.SetEnableStreaming(false)
	loop.SetHandler(&mockHandler{approveResult: true})

	// Record msg count seen at each checkpoint — must be monotonically
	// non-decreasing (compaction can shrink, but in this test there's none).
	var snapshots []int
	loop.SetCheckpointFunc(func(ctx context.Context) error {
		snapshots = append(snapshots, len(loop.RunMessages()))
		return nil
	})

	_, _, err := loop.Run(context.Background(), "loop", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(snapshots) < 2 {
		t.Fatalf("expected at least 2 checkpoints across 3 tool batches, got %d: %v", len(snapshots), snapshots)
	}
	for i := 1; i < len(snapshots); i++ {
		if snapshots[i] < snapshots[i-1] {
			t.Fatalf("checkpoint %d shrank (%d < %d); RunMessages must grow monotonically in this scenario: %v",
				i, snapshots[i], snapshots[i-1], snapshots)
		}
	}
}

// TestMaybeCheckpoint_SaveFailureRetainsDirty verifies that a checkpoint
// callback returning an error leaves the tracker's dirty flag SET and
// does not stamp lastCheckpointAt. The next fire point retries the save
// without being throttled by the debounce window. A naive implementation
// that takes-dirty-and-stamps-time before the callback would let storage
// errors silently drop pending work.
func TestMaybeCheckpoint_SaveFailureRetainsDirty(t *testing.T) {
	loop := NewAgentLoop(nil, NewToolRegistry(), "m", "", 1, 1, 1, nil, nil, nil)
	loop.tracker = newPhaseTracker()
	loop.SetCheckpointMinInterval(100 * time.Millisecond)

	var fireCount atomic.Int32
	// Simulate a persistent storage error — every call returns error.
	loop.SetCheckpointFunc(func(ctx context.Context) error {
		fireCount.Add(1)
		return errors.New("simulated disk full")
	})

	loop.tracker.MarkDirty()
	loop.maybeCheckpoint(context.Background())
	if fireCount.Load() != 1 {
		t.Fatalf("fire 1: want 1 call, got %d", fireCount.Load())
	}
	// Dirty must still be set (save failed).
	if !loop.tracker.IsDirty() {
		t.Fatal("save failure cleared dirty flag — pending work would be silently lost")
	}

	// Next call (even within debounce window) must retry, not be throttled,
	// because lastCheckpointAt was NOT stamped on the failed call.
	loop.maybeCheckpoint(context.Background())
	if fireCount.Load() != 2 {
		t.Fatalf("fire 2 (after failure): want 2 calls (retry), got %d — debounce was wrongly stamped on failure", fireCount.Load())
	}
	if !loop.tracker.IsDirty() {
		t.Fatal("second failure also dropped dirty")
	}
}

// TestMaybeCheckpoint_DebouncePreservesDirty verifies finding #2's fix:
// when the debounce window skips a checkpoint, the tracker's dirty flag
// is left set so the next fire point persists the pending durable state.
// A naive implementation that TakeDirty-before-debounce would lose this
// work if the process crashed in the window.
func TestMaybeCheckpoint_DebouncePreservesDirty(t *testing.T) {
	loop := NewAgentLoop(nil, NewToolRegistry(), "m", "", 1, 1, 1, nil, nil, nil)
	loop.tracker = newPhaseTracker()
	loop.SetCheckpointMinInterval(100 * time.Millisecond)

	var fires atomic.Int32
	loop.SetCheckpointFunc(func(ctx context.Context) error { fires.Add(1); return nil })

	// Fire 1: dirty set, no prior checkpoint — should fire.
	loop.tracker.MarkDirty()
	loop.maybeCheckpoint(context.Background())
	if n := fires.Load(); n != 1 {
		t.Fatalf("fire 1: want 1, got %d", n)
	}

	// Fire 2: dirty set again, but within debounce window — must skip
	// AND leave dirty set so the next call persists.
	loop.tracker.MarkDirty()
	loop.maybeCheckpoint(context.Background())
	if n := fires.Load(); n != 1 {
		t.Fatalf("fire 2 (debounced): want 1 (skipped), got %d", n)
	}
	if !loop.tracker.TakeDirty() {
		t.Fatal("debounced skip consumed dirty flag — crash in window would lose work")
	}
}

// TestCheckpoint_SurvivesCancelMidTurn verifies the crash/abort-after-
// checkpoint recovery: if we cancel mid-turn, the checkpoint callback
// captured the tool batch before the cancel. This is the core guarantee
// of Slice 4 — work completed before the abort is not lost.
func TestCheckpoint_SurvivesCancelMidTurn(t *testing.T) {
	var inToolCount atomic.Int32
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		// First two calls: run a tool. Third: hang until cancel.
		if callCount <= 2 {
			json.NewEncoder(w).Encode(nativeResponse("", "tool_use",
				toolCall("mock_tool", fmt.Sprintf(`{"step":%d}`, callCount)), 10, 5))
			return
		}
		// Hang the third LLM call — simulates the "upstream stall" scenario.
		// Bounded so server.Close() can reap the handler goroutine.
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	reg.Register(&mockTool{name: "mock_tool"})
	loop := NewAgentLoop(gw, reg, "medium", "", 25, 2000, 200, nil, nil, nil)
	loop.SetEnableStreaming(false)
	loop.SetHandler(&mockHandler{approveResult: true})

	var capturedSnapshots atomic.Int32
	var lastSnapshotMsgs atomic.Int32
	loop.SetCheckpointFunc(func(ctx context.Context) error {
		capturedSnapshots.Add(1)
		lastSnapshotMsgs.Store(int32(len(loop.RunMessages())))
		inToolCount.Add(1)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	_, _, _ = loop.Run(ctx, "stall", nil, nil)

	if n := capturedSnapshots.Load(); n < 1 {
		t.Fatalf("expected >=1 checkpoint before cancel, got %d", n)
	}
	if msgs := lastSnapshotMsgs.Load(); msgs < 2 {
		t.Fatalf("last checkpoint snapshot had only %d msgs — tool batch not persisted before stall", msgs)
	}
}
