package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// intermediateAnswerRecordingHandler records OnText (final-answer emissions) and
// OnIntermediateAnswer (a turn's answer flushed because an inject superseded it)
// separately, so a test can assert which answers were surfaced mid-run vs
// returned as the run's true final reply.
type intermediateAnswerRecordingHandler struct {
	mockHandler
	mu            sync.Mutex
	textCalls     []string
	intermediates []string
}

func (h *intermediateAnswerRecordingHandler) OnText(text string) {
	h.mu.Lock()
	h.textCalls = append(h.textCalls, text)
	h.mu.Unlock()
}

func (h *intermediateAnswerRecordingHandler) OnIntermediateAnswer(text string) {
	h.mu.Lock()
	h.intermediates = append(h.intermediates, text)
	h.mu.Unlock()
}

// TestAgentLoop_InjectSupersedesFinalAnswer_FlushesIntermediate reproduces the
// IM regression where the first turn's answer vanished from the channel: the
// user fires a follow-up before the first reply posts, the end_turn drain-race
// guard injects it and CONTINUES the run, so that first answer becomes an
// intermediate the daemon's no-op OnText drops (final answers reach the channel
// via SendReply at run end only). The loop must surface the superseded answer
// via OnIntermediateAnswer so the daemon can still emit it to the channel
// timeline — and must NOT flush the run's true final answer that way (it returns
// normally / posts via SendReply; a second emission would duplicate it).
func TestAgentLoop_InjectSupersedesFinalAnswer_FlushesIntermediate(t *testing.T) {
	injectCh := make(chan InjectedMessage, 10)
	var mu sync.Mutex
	callCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		n := callCount
		mu.Unlock()
		if n == 1 {
			// Follow-up "B" arrives while the model composes turn-1's end_turn
			// answer "answer-one" — lands in injectCh after iter-0's top drain.
			injectCh <- InjectedMessage{Text: "follow-up B", ClientMessageID: "im-b"}
			_ = json.NewEncoder(w).Encode(nativeResponse("answer-one", "end_turn", nil, 10, 5))
			return
		}
		// Turn-2 (the injected follow-up) produces the run's true final answer.
		_ = json.NewEncoder(w).Encode(nativeResponse("answer-two-final", "end_turn", nil, 10, 5))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	loop := NewAgentLoop(gw, NewToolRegistry(), "medium", "", 10, 2000, 200, nil, nil, nil)
	loop.SetInjectCh(injectCh)
	h := &intermediateAnswerRecordingHandler{}
	loop.SetHandler(h)

	final, _, err := loop.Run(context.Background(), "first message", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	t.Logf("final=%q OnText=%v OnIntermediateAnswer=%v", final, h.textCalls, h.intermediates)

	// The superseded turn-1 answer must be flushed exactly once as an intermediate.
	if len(h.intermediates) != 1 || h.intermediates[0] != "answer-one" {
		t.Fatalf("REPRO: expected OnIntermediateAnswer once with the superseded turn-1 answer %q, got %v", "answer-one", h.intermediates)
	}
	// The run's true final answer must NOT be flushed as an intermediate.
	for _, s := range h.intermediates {
		if s == "answer-two-final" {
			t.Fatalf("final answer must NOT be flushed as intermediate (would duplicate on the channel)")
		}
	}
	if final != "answer-two-final" {
		t.Fatalf("expected run to return the final answer %q, got %q", "answer-two-final", final)
	}
}
