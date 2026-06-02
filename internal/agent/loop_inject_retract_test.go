package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// TestAgentLoop_RetractedInject_DroppedAtDrain verifies the steering-retract
// path: a follow-up whose ClientMessageID the injectRetractedChecker reports as
// retracted is dropped at the drain boundary — it never reaches the LLM and
// never fires OnInjectedCommitted — while a non-retracted follow-up injected in
// the same batch is kept. This is the loop-side half of the fix for "cancel a
// queued draft whose inject was already sent".
func TestAgentLoop_RetractedInject_DroppedAtDrain(t *testing.T) {
	const keptID = "local-keep"
	const retractedID = "local-retract"
	const keptText = "keep this follow-up"
	const retractedText = "drop this cancelled follow-up"

	injectCh := make(chan InjectedMessage, 10)
	var mu sync.Mutex
	callCount := 0
	var secondReqBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		callCount++
		n := callCount
		if n == 2 {
			secondReqBody = string(body)
		}
		mu.Unlock()
		if n == 1 {
			// Two follow-ups injected mid-batch: one will be retracted, one kept.
			injectCh <- InjectedMessage{ClientMessageID: retractedID, Text: retractedText}
			injectCh <- InjectedMessage{ClientMessageID: keptID, Text: keptText}
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
	loop.SetInjectRetractedChecker(func(id string) bool { return id == retractedID })
	h := &injectCommitRecordingHandler{}
	loop.SetHandler(h)

	if _, _, err := loop.Run(context.Background(), "do work", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(secondReqBody, retractedText) {
		t.Errorf("retracted follow-up leaked into LLM request #2 (should have been dropped):\n%s", secondReqBody)
	}
	if !strings.Contains(secondReqBody, keptText) {
		t.Errorf("kept follow-up missing from LLM request #2:\n%s", secondReqBody)
	}
	if len(h.committedIDs) != 1 || h.committedIDs[0] != keptID {
		t.Errorf("expected only kept id (%q) to fire OnInjectedCommitted, got %v", keptID, h.committedIDs)
	}
}
