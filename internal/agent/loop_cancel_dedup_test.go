package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// ctxCancellingTool cancels the run's context when executed, simulating a
// user-initiated cancel (Esc / Cmd+Enter interrupt-send) landing while a tool
// is running. The loop then observes ctx.Err() at the top of the next
// iteration — the exact window where the lastText teardown flush fires.
type ctxCancellingTool struct {
	cancel context.CancelFunc
}

func (c *ctxCancellingTool) Info() ToolInfo {
	return ToolInfo{
		Name:        "cancel_run",
		Description: "cancels the run context for testing",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

func (c *ctxCancellingTool) Run(ctx context.Context, args string) (ToolResult, error) {
	c.cancel()
	return ToolResult{Content: "ok"}, nil
}

func (c *ctxCancellingTool) RequiresApproval() bool     { return false }
func (c *ctxCancellingTool) IsReadOnlyCall(string) bool { return true }

func countAssistantMessagesContaining(msgs []client.Message, text string) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "assistant" && strings.Contains(m.Content.Text(), text) {
			n++
		}
	}
	return n
}

// TestAgentLoop_CancelAfterNativeToolPreamble_NoDuplicateText reproduces the
// "Cmd+Enter makes the assistant reply twice" bug: on the native tool path the
// preamble is persisted inside the text+tool_use assistant message by
// buildAssistantMessage, and the cancel teardown used to append the same text
// AGAIN as a string assistant message (fingerprint on disk: block message at
// idx N, verbatim string copy at idx N+2, 24µs apart). The teardown must skip
// the flush when the preamble is already recorded.
func TestAgentLoop_CancelAfterNativeToolPreamble_NoDuplicateText(t *testing.T) {
	const preamble = "Process is stable, checking the daemon WebSocket connection:"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		n := callCount
		mu.Unlock()
		if n == 1 {
			_ = json.NewEncoder(w).Encode(nativeResponseWithID(preamble, "tool_use",
				toolCallWithID("cancel_run", `{}`, "toolu_cancel_1"), 10, 5))
			return
		}
		// The cancelled context should prevent a second LLM call entirely;
		// answering end_turn here keeps the test honest if it ever happens.
		_ = json.NewEncoder(w).Encode(nativeResponse("should not be reached", "end_turn", nil, 10, 5))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	reg.Register(&ctxCancellingTool{cancel: cancel})
	loop := NewAgentLoop(gw, reg, "medium", "", 10, 2000, 200, nil, nil, nil)

	text, _, err := loop.Run(ctx, "restart the backend", nil, nil)
	if err == nil {
		t.Fatal("expected ctx cancellation error, got nil")
	}
	if text != preamble {
		t.Errorf("cancel teardown should still RETURN the partial answer, got %q", text)
	}

	msgs := loop.RunMessages()
	if got := countAssistantMessagesContaining(msgs, preamble); got != 1 {
		for i, m := range msgs {
			t.Logf("msg[%d] role=%s text=%q", i, m.Role, m.Content.Text())
		}
		t.Errorf("preamble must be persisted exactly once, found in %d assistant messages", got)
	}
}

// TestAgentLoop_CancelAfterXMLToolPreamble_FlushStillRecordsText guards the
// other half of the contract: the XML fallback path (tool calls without
// native IDs) never persists the preamble via buildAssistantMessage, so the
// cancel teardown's flush is the ONLY record of the partial answer. The dedup
// guard must not drop it.
func TestAgentLoop_CancelAfterXMLToolPreamble_FlushStillRecordsText(t *testing.T) {
	const preamble = "Looking that up with a legacy tool:"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		n := callCount
		mu.Unlock()
		if n == 1 {
			// No tool-call ID → hasNativeToolIDs is false → XML fallback path.
			_ = json.NewEncoder(w).Encode(nativeResponse(preamble, "tool_use",
				toolCall("cancel_run", `{}`), 10, 5))
			return
		}
		_ = json.NewEncoder(w).Encode(nativeResponse("should not be reached", "end_turn", nil, 10, 5))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	reg.Register(&ctxCancellingTool{cancel: cancel})
	loop := NewAgentLoop(gw, reg, "medium", "", 10, 2000, 200, nil, nil, nil)

	if _, _, err := loop.Run(ctx, "do legacy work", nil, nil); err == nil {
		t.Fatal("expected ctx cancellation error, got nil")
	}

	msgs := loop.RunMessages()
	if got := countAssistantMessagesContaining(msgs, preamble); got != 1 {
		for i, m := range msgs {
			t.Logf("msg[%d] role=%s text=%q", i, m.Role, m.Content.Text())
		}
		t.Errorf("XML-path preamble must be flushed exactly once, found in %d assistant messages", got)
	}
}

// TestAgentLoop_MaxIterFallback_NoDuplicateText covers the third lastText
// consumer: when the iteration cap is hit and the force-stop synthesis turn
// fails, the legacy fallback used to re-append the already-persisted native
// preamble. The fallback must dedup the append while still returning lastText
// to the caller (IM transports send the return value as the reply).
func TestAgentLoop_MaxIterFallback_NoDuplicateText(t *testing.T) {
	const preamble = "Gathering data before the cap:"

	var mu sync.Mutex
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		n := callCount
		mu.Unlock()
		if n == 1 {
			_ = json.NewEncoder(w).Encode(nativeResponseWithID(preamble, "tool_use",
				toolCallWithID("capture_snapshot", `{}`, "toolu_cap_1"), 10, 5))
			return
		}
		// Force-stop synthesis turn fails fast (400 is non-retryable) so the
		// loop takes the legacy lastText fallback.
		http.Error(w, `{"error":"forced synthesis failure"}`, http.StatusBadRequest)
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	reg.Register(&snapshotCapturingTool{})
	loop := NewAgentLoop(gw, reg, "medium", "", 1, 2000, 200, nil, nil, nil)

	text, _, err := loop.Run(context.Background(), "hit the cap", nil, nil)
	if !errors.Is(err, ErrMaxIterReached) {
		t.Fatalf("expected ErrMaxIterReached, got %v", err)
	}
	if text != preamble {
		t.Errorf("fallback should still RETURN lastText, got %q", text)
	}

	msgs := loop.RunMessages()
	if got := countAssistantMessagesContaining(msgs, preamble); got != 1 {
		for i, m := range msgs {
			t.Logf("msg[%d] role=%s text=%q", i, m.Role, m.Content.Text())
		}
		t.Errorf("preamble must be persisted exactly once, found in %d assistant messages", got)
	}
}
