package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// injectCommitRecordingHandler embeds mockHandler (full EventHandler) and adds
// InjectCommitHandler to record OnInjectedCommitted callbacks.
type injectCommitRecordingHandler struct {
	mockHandler
	committedIDs   []string
	committedTexts []string
}

func (h *injectCommitRecordingHandler) OnInjectedCommitted(clientMessageID, text string) {
	h.committedIDs = append(h.committedIDs, clientMessageID)
	h.committedTexts = append(h.committedTexts, text)
}

// TestAgentLoop_InjectCommitted_FiresOnDrain verifies that when the loop drains
// a mid-run injected follow-up carrying a ClientMessageID, it calls the
// handler's InjectCommitHandler at the drain (consume) boundary — the exact
// signal the Desktop SSE path uses to flip a queued-draft card into a real
// user bubble. A message WITHOUT a ClientMessageID must NOT fire it.
func TestAgentLoop_InjectCommitted_FiresOnDrain(t *testing.T) {
	const clientID = "local-abc123"
	const steerText = "summarize to desktop"

	injectCh := make(chan InjectedMessage, 10)
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			// User "types" a steered follow-up during the first tool batch.
			injectCh <- InjectedMessage{ClientMessageID: clientID, Text: steerText}
			_ = json.NewEncoder(w).Encode(nativeResponse("", "tool_use", toolCall("capture_snapshot", `{}`), 10, 5))
		} else {
			_ = json.NewEncoder(w).Encode(nativeResponse("done", "end_turn", nil, 10, 5))
		}
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	reg.Register(&snapshotCapturingTool{})
	loop := NewAgentLoop(gw, reg, "medium", "", 10, 2000, 200, nil, nil, nil)
	loop.SetInjectCh(injectCh)
	h := &injectCommitRecordingHandler{}
	loop.SetHandler(h)

	if _, _, err := loop.Run(context.Background(), "do A, B, C in sequence", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(h.committedIDs) != 1 {
		t.Fatalf("expected exactly 1 OnInjectedCommitted, got %d (ids=%v)", len(h.committedIDs), h.committedIDs)
	}
	if h.committedIDs[0] != clientID {
		t.Errorf("client id = %q, want %q", h.committedIDs[0], clientID)
	}
	if h.committedTexts[0] != steerText {
		t.Errorf("text = %q, want %q", h.committedTexts[0], steerText)
	}
}

// TestAgentLoop_InjectCommitted_SkipsWithoutClientID verifies a drained inject
// with no ClientMessageID (TUI keyboard / IM / legacy) does NOT fire the
// committed callback — only client-id-carrying injects (Desktop) do.
func TestAgentLoop_InjectCommitted_SkipsWithoutClientID(t *testing.T) {
	injectCh := make(chan InjectedMessage, 10)
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			injectCh <- InjectedMessage{Text: "no client id here"} // ClientMessageID empty
			_ = json.NewEncoder(w).Encode(nativeResponse("", "tool_use", toolCall("capture_snapshot", `{}`), 10, 5))
		} else {
			_ = json.NewEncoder(w).Encode(nativeResponse("done", "end_turn", nil, 10, 5))
		}
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	reg.Register(&snapshotCapturingTool{})
	loop := NewAgentLoop(gw, reg, "medium", "", 10, 2000, 200, nil, nil, nil)
	loop.SetInjectCh(injectCh)
	h := &injectCommitRecordingHandler{}
	loop.SetHandler(h)

	if _, _, err := loop.Run(context.Background(), "do work", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(h.committedIDs) != 0 {
		t.Fatalf("expected 0 OnInjectedCommitted (no client id), got %d", len(h.committedIDs))
	}
}
