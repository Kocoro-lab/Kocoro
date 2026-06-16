package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// TestAgentLoop_InjectCommittedBroadcaster_FiresAlongsideHandler verifies the
// channel-agnostic commit hook: when a drained follow-up carries a
// ClientMessageID, the broadcaster fires with the same (id, text) pair as the
// per-request InjectCommitHandler — and fires EVEN WITHOUT such a handler,
// since its whole purpose is reaching clients that don't own the run's stream
// (the daemon wires it to the EventBus). An id-less message must not fire it.
func TestAgentLoop_InjectCommittedBroadcaster_FiresAlongsideHandler(t *testing.T) {
	const clientID = "local-broadcast-1"
	const steerText = "steer from another channel"

	injectCh := make(chan InjectedMessage, 10)
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			injectCh <- InjectedMessage{ClientMessageID: clientID, Text: steerText}
			injectCh <- InjectedMessage{Text: "no client id — keyboard/IM inject"}
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

	// Deliberately NO InjectCommitHandler on the run's handler — the
	// broadcaster must fire regardless (mockHandler does not implement it).
	loop.SetHandler(&mockHandler{})

	var gotIDs, gotTexts []string
	loop.SetInjectCommittedBroadcaster(func(id, text string) {
		gotIDs = append(gotIDs, id)
		gotTexts = append(gotTexts, text)
	})

	if _, _, err := loop.Run(context.Background(), "work", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(gotIDs) != 1 || gotIDs[0] != clientID {
		t.Errorf("broadcaster ids = %v, want exactly [%q] (id-less inject must not fire)", gotIDs, clientID)
	}
	if len(gotTexts) != 1 || gotTexts[0] != steerText {
		t.Errorf("broadcaster texts = %v, want [%q]", gotTexts, steerText)
	}
}
