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

// recordedIntermediate pairs a flushed intermediate answer with the cloud
// message id of the inbound message that turn was answering — the daemon uses
// the id to complete that message's own channel reply (so group-chat / rapid
// follow-ups render as separate messages, not one merged reply).
type recordedIntermediate struct {
	text           string
	cloudMessageID string
}

// intermediateAnswerRecordingHandler records OnText (final-answer emissions) and
// OnIntermediateAnswer (a turn's answer flushed because an inject superseded it)
// separately, so a test can assert which answers were surfaced mid-run vs
// returned as the run's true final reply.
type intermediateAnswerRecordingHandler struct {
	mockHandler
	mu            sync.Mutex
	textCalls     []string
	intermediates []recordedIntermediate
}

func (h *intermediateAnswerRecordingHandler) OnText(text string) {
	h.mu.Lock()
	h.textCalls = append(h.textCalls, text)
	h.mu.Unlock()
}

func (h *intermediateAnswerRecordingHandler) OnIntermediateAnswer(text, cloudMessageID string) {
	h.mu.Lock()
	h.intermediates = append(h.intermediates, recordedIntermediate{text: text, cloudMessageID: cloudMessageID})
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
	if len(h.intermediates) != 1 || h.intermediates[0].text != "answer-one" {
		t.Fatalf("REPRO: expected OnIntermediateAnswer once with the superseded turn-1 answer %q, got %v", "answer-one", h.intermediates)
	}
	// The run's true final answer must NOT be flushed as an intermediate.
	for _, s := range h.intermediates {
		if s.text == "answer-two-final" {
			t.Fatalf("final answer must NOT be flushed as intermediate (would duplicate on the channel)")
		}
	}
	if final != "answer-two-final" {
		t.Fatalf("expected run to return the final answer %q, got %q", "answer-two-final", final)
	}
}

// TestAgentLoop_IntermediateAnswer_CarriesRespondedMessageID verifies that when
// a follow-up from a DIFFERENT inbound message supersedes turn-1, the loop
// carries the cloud message id of the message each turn was answering: the
// intermediate (turn-1's answer) is tagged with the PRIMARY message's id, and
// the run's final reply id (ReplyCloudMessageID) advances to the injected
// message's id. The daemon uses these to complete each inbound message's own
// channel reply, so a group chat renders two separate messages instead of one
// merged reply (the Teams "two replies collapsed into one" bug).
func TestAgentLoop_IntermediateAnswer_CarriesRespondedMessageID(t *testing.T) {
	injectCh := make(chan InjectedMessage, 10)
	var mu sync.Mutex
	callCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		n := callCount
		mu.Unlock()
		if n == 1 {
			// Follow-up from a second user (its own cloud message id) lands while
			// the model composes turn-1's end_turn answer for the primary message.
			injectCh <- InjectedMessage{Text: "follow-up B", CloudMessageID: "msg-wayland"}
			_ = json.NewEncoder(w).Encode(nativeResponse("answer-one", "end_turn", nil, 10, 5))
			return
		}
		_ = json.NewEncoder(w).Encode(nativeResponse("answer-two-final", "end_turn", nil, 10, 5))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	loop := NewAgentLoop(gw, NewToolRegistry(), "medium", "", 10, 2000, 200, nil, nil, nil)
	loop.SetInjectCh(injectCh)
	loop.SetReplyCloudMessageID("msg-awek") // primary inbound message
	h := &intermediateAnswerRecordingHandler{}
	loop.SetHandler(h)

	final, _, err := loop.Run(context.Background(), "first message", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	// turn-1's answer (answer-one) was the reply to the PRIMARY message, so the
	// intermediate must carry the primary id, NOT the injected follow-up's id.
	if len(h.intermediates) != 1 {
		t.Fatalf("expected exactly one intermediate, got %v", h.intermediates)
	}
	if got := h.intermediates[0]; got.text != "answer-one" || got.cloudMessageID != "msg-awek" {
		t.Fatalf("intermediate = %+v, want {text:answer-one cloudMessageID:msg-awek}", got)
	}
	// The run's final reply answers the INJECTED message, so the final reply id
	// must have advanced to the injected message's id.
	if final != "answer-two-final" {
		t.Fatalf("final = %q, want answer-two-final", final)
	}
	if got := loop.ReplyCloudMessageID(); got != "msg-wayland" {
		t.Fatalf("ReplyCloudMessageID() = %q, want msg-wayland (the last message processed)", got)
	}
	// msg-awek was independently replied (answer-one via OnIntermediateAnswer) and
	// pruned; only the un-replied final target remains for the daemon to co-ack.
	if got := loop.PendingAckIDs(); len(got) != 1 || got[0] != "msg-wayland" {
		t.Fatalf("PendingAckIDs() = %v, want [msg-wayland]", got)
	}
}

// TestAgentLoop_FollowUpDuringToolUse_AdvancesReplyTarget_NoIntermediate pins the
// top-of-loop drain contract the daemon's ack path depends on. A follow-up that
// arrives while the model is using tools is drained at the TOP of the next
// iteration (not the end_turn race), which advances ReplyCloudMessageID to the
// follow-up WITHOUT firing OnIntermediateAnswer — the primary is absorbed into
// the combined turn with no standalone answer. The daemon must therefore ack the
// primary via the final-reply redirect path (TestClient_RedirectReply), since no
// intermediate answer carries it. If OnIntermediateAnswer fired here the daemon
// would double-handle; if the target failed to advance the final reply would
// wrongly stay on the primary id.
func TestAgentLoop_FollowUpDuringToolUse_AdvancesReplyTarget_NoIntermediate(t *testing.T) {
	injectCh := make(chan InjectedMessage, 10)
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			// Follow-up from a second inbound lands while the tool runs → drained
			// at the top of the next iteration, not the end_turn race.
			injectCh <- InjectedMessage{Text: "follow-up B", CloudMessageID: "msg-wayland"}
			_ = json.NewEncoder(w).Encode(nativeResponse("", "tool_use", toolCall("capture_snapshot", `{}`), 10, 5))
			return
		}
		_ = json.NewEncoder(w).Encode(nativeResponse("merged-answer", "end_turn", nil, 10, 5))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	reg.Register(&snapshotCapturingTool{})
	loop := NewAgentLoop(gw, reg, "medium", "", 10, 2000, 200, nil, nil, nil)
	loop.SetInjectCh(injectCh)
	loop.SetReplyCloudMessageID("msg-awek")
	h := &intermediateAnswerRecordingHandler{}
	loop.SetHandler(h)

	final, _, err := loop.Run(context.Background(), "first message", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Reply target advanced to the absorbed follow-up: the run's final reply
	// belongs to msg-wayland.
	if got := loop.ReplyCloudMessageID(); got != "msg-wayland" {
		t.Fatalf("ReplyCloudMessageID() = %q, want msg-wayland", got)
	}
	if final != "merged-answer" {
		t.Fatalf("final = %q, want merged-answer", final)
	}
	// No intermediate fired — the primary's ack cannot come from
	// OnIntermediateAnswer; it must come from the daemon's redirect-path ack.
	h.mu.Lock()
	n := len(h.intermediates)
	h.mu.Unlock()
	if n != 0 {
		t.Fatalf("OnIntermediateAnswer fired %d times; top-of-loop drain must not flush an intermediate", n)
	}
	// No intermediate fired, so neither id was pruned: the daemon must co-ack BOTH
	// the absorbed primary and the followup after the final reply is delivered.
	if got := loop.PendingAckIDs(); len(got) != 2 || got[0] != "msg-awek" || got[1] != "msg-wayland" {
		t.Fatalf("PendingAckIDs() = %v, want [msg-awek msg-wayland]", got)
	}
}
