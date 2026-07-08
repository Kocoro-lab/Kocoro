//go:build darwin && cgo

package koe

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestEndCallToolTriggersHangupWithoutOutput pins the end_call wiring: the tool
// invokes onEndCall (the Desktop hang-up + goodbye earcon) and sends NO
// function_call_output — the teardown is the response, and a spoken reply is
// exactly what dismiss must avoid.
func TestEndCallToolTriggersHangupWithoutOutput(t *testing.T) {
	audio, err := NewAudioIO()
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	state := NewCallState("burst-end", "")
	disp := NewDispatcher(NewDaemonClient(""), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	cap := &captureSender{}
	h := newEventHandler(disp, state, audio, cap.send)
	called := make(chan struct{}, 1)
	h.onEndCall = func() { called <- struct{}{} }

	ev, _ := json.Marshal(map[string]any{
		"type": "response.function_call_arguments.done",
		"name": "end_call", "call_id": "c1", "arguments": "{}",
	})
	h.handleEvent(context.Background(), ev)

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("end_call did not invoke onEndCall")
	}
	if n := cap.countType("conversation.item.create"); n != 0 {
		t.Errorf("end_call must not send a function_call_output, got %d conversation.item.create", n)
	}
	if n := cap.countType("response.create"); n != 0 {
		t.Errorf("end_call must not request a spoken response, got %d response.create", n)
	}
}

// TestDismissTranscriptHangsUp pins the deterministic backstop: a whole-utterance
// dismiss phrase in the input transcription hangs up (onEndCall) even when the model
// never calls the end_call tool — the reliable path for the fixed vocabulary. A
// non-dismiss transcript must NOT hang up.
func TestDismissTranscriptHangsUp(t *testing.T) {
	newH := func() (*eventHandler, chan struct{}) {
		audio, err := NewAudioIO()
		if err != nil {
			t.Fatalf("NewAudioIO: %v", err)
		}
		state := NewCallState("burst-dismiss", "")
		disp := NewDispatcher(NewDaemonClient(""), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
		h := newEventHandler(disp, state, audio, (&captureSender{}).send)
		hung := make(chan struct{}, 1)
		h.onEndCall = func() { hung <- struct{}{} }
		return h, hung
	}
	feed := func(h *eventHandler, transcript string) {
		raw, _ := json.Marshal(map[string]any{
			"type":       "conversation.item.input_audio_transcription.completed",
			"transcript": transcript,
		})
		h.handleEvent(context.Background(), raw)
	}

	t.Run("dismiss phrase hangs up", func(t *testing.T) {
		h, hung := newH()
		feed(h, "闭嘴。")
		select {
		case <-hung:
		case <-time.After(2 * time.Second):
			t.Fatal("dismiss transcript did not hang up")
		}
	})
	t.Run("non-dismiss transcript stays on the call", func(t *testing.T) {
		h, hung := newH()
		feed(h, "解释一下量子纠缠")
		select {
		case <-hung:
			t.Fatal("a normal request must not hang up")
		case <-time.After(300 * time.Millisecond):
		}
	})
}

// TestEndCallToolNilHookIsSafe: the standalone/CLI path leaves onEndCall nil, so a
// stray end_call must be an inert no-op, never a panic.
func TestEndCallToolNilHookIsSafe(t *testing.T) {
	audio, err := NewAudioIO()
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	state := NewCallState("burst-end2", "")
	disp := NewDispatcher(NewDaemonClient(""), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	h := newEventHandler(disp, state, audio, (&captureSender{}).send)
	// onEndCall stays nil.
	ev, _ := json.Marshal(map[string]any{
		"type": "response.function_call_arguments.done",
		"name": "end_call", "call_id": "c1", "arguments": "{}",
	})
	h.handleEvent(context.Background(), ev) // must not panic
}
