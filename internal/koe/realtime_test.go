package koe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureSender records every oai-events client message the handler sends. A
// mutex guards it because async do_task injects the result from a goroutine while
// the test reads on the main goroutine.
type captureSender struct {
	mu   sync.Mutex
	sent []map[string]any
}

func (c *captureSender) send(v any) error {
	b, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	c.mu.Lock()
	c.sent = append(c.sent, m)
	c.mu.Unlock()
	return nil
}

// sentContains reports whether any captured frame's JSON contains sub.
func (c *captureSender) sentContains(sub string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.sent {
		b, _ := json.Marshal(m)
		if strings.Contains(string(b), sub) {
			return true
		}
	}
	return false
}

// TestHandleFunctionCallDoTaskAsync verifies the C-full deferred-ack flow: the
// fast-ack function_call_output is sent SYNCHRONOUSLY (Koe speaks "on it"), then
// the back-brain result is injected from the goroutine and voiced.
func TestHandleFunctionCallDoTaskAsync(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"reply": "Reminder added.", "agent": "default"})
	}))
	defer srv.Close()

	// ONE CallState shared by the dispatcher and the event handler, mirroring
	// production Connect: SetInFlight on the goroutine must be visible to a
	// get_status routed through the same dispatcher.
	state := NewCallState("burst-x", "")
	disp := NewDispatcher(NewDaemonClient(srv.URL), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	cap := &captureSender{}
	h := newEventHandler(disp, state, nil, cap.send)

	h.handleFunctionCall(context.Background(), "call-1", "do_task", []byte(`{"task":"remind me"}`))

	// The fast-ack must already be on the wire SYNCHRONOUSLY (Koe speaks "on it").
	if !cap.sentContains("function_call_output") {
		t.Fatal("fast-ack function_call_output not sent synchronously")
	}
	// The result is injected after the back-brain turn — wait for it.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cap.sentContains("Reminder added.") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("injected do_task result never sent")
}

// TestHandleEventGatesMicWhileSpeaking locks the half-duplex gate into the event
// loop: a structurally-correct gate (C2) is inert unless handleEvent actually
// toggles it. This also pins the exact OpenAI event names — a rename would make
// the gate silently never fire, which this test would catch.
func TestHandleEventGatesMicWhileSpeaking(t *testing.T) {
	audio, err := NewAudioIO() // codec only, no device — SetSpeaking/dropCapture work headless
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	state := NewCallState("burst-x", "")
	disp := NewDispatcher(NewDaemonClient(""), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	h := newEventHandler(disp, state, audio, func(any) error { return nil })

	if audio.dropCapture() {
		t.Fatal("mic must not be gated before any speaking event")
	}
	h.handleEvent(context.Background(), []byte(`{"type":"response.output_audio.delta"}`))
	if !audio.dropCapture() {
		t.Error("response.output_audio.delta must gate the mic (SetSpeaking true)")
	}
	h.handleEvent(context.Background(), []byte(`{"type":"response.done"}`))
	if audio.dropCapture() {
		t.Error("response.done must ungate the mic (SetSpeaking false)")
	}
}

// TestHandleEventBargeIn pins E2: a speech_started WHILE speaking cancels the reply,
// clears the server + local output audio, ungates the mic, and flips to listening.
func TestHandleEventBargeIn(t *testing.T) {
	audio, err := NewAudioIO()
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	state := NewCallState("burst-x", "")
	disp := NewDispatcher(NewDaemonClient(""), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	cap := &captureSender{}
	h := newEventHandler(disp, state, audio, cap.send)

	// Enter speaking + queue some reply audio locally.
	h.handleEvent(context.Background(), []byte(`{"type":"output_audio_buffer.started"}`))
	if h.voiceState() != "speaking" {
		t.Fatalf("setup: expected speaking, got %q", h.voiceState())
	}
	audio.Play(make([]int16, audioFrameSize))
	audio.Play(make([]int16, audioFrameSize))
	if len(audio.playBuf) == 0 {
		t.Fatal("setup: playBuf should have queued frames")
	}

	// The user talks over Kocoro.
	h.handleEvent(context.Background(), []byte(`{"type":"input_audio_buffer.speech_started"}`))

	if !cap.sentContains("response.cancel") {
		t.Error("barge-in must cancel the in-flight response")
	}
	if !cap.sentContains("output_audio_buffer.clear") {
		t.Error("barge-in must clear the server output-audio buffer")
	}
	if len(audio.playBuf) != 0 {
		t.Errorf("barge-in must drain local playback, playBuf len = %d", len(audio.playBuf))
	}
	if audio.dropCapture() {
		t.Error("barge-in must ungate the mic")
	}
	if h.voiceState() != "listening" {
		t.Errorf("barge-in must flip to listening, got %q", h.voiceState())
	}
}

// TestHandleEventNoBargeWhenListening: a speech_started while merely listening must
// NOT fire the interrupt (no spurious response.cancel), only re-affirm listening.
func TestHandleEventNoBargeWhenListening(t *testing.T) {
	state := NewCallState("burst-x", "")
	disp := NewDispatcher(NewDaemonClient(""), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	cap := &captureSender{}
	h := newEventHandler(disp, state, nil, cap.send)

	h.handleEvent(context.Background(), []byte(`{"type":"input_audio_buffer.speech_started"}`))
	if cap.sentContains("response.cancel") {
		t.Error("speech_started while listening must not cancel a response")
	}
	if h.voiceState() != "listening" {
		t.Errorf("expected listening, got %q", h.voiceState())
	}
}

// TestHandleEventVoiceStateSequence pins the precise state machine (D1w): the
// WebRTC output_audio_buffer.started/stopped markers drive SPEAKING/IDLE, and
// input_audio_buffer.speech_started surfaces the reactive listening moment. A
// rename of any of these GA event names would silently break the Island sprite —
// this test catches it.
func TestHandleEventVoiceStateSequence(t *testing.T) {
	audio, _ := NewAudioIO()
	state := NewCallState("burst-seq", "")
	disp := NewDispatcher(NewDaemonClient(""), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	h := newEventHandler(disp, state, audio, func(any) error { return nil })
	var states []string
	h.onVoiceState = func(s string) { states = append(states, s) }

	for _, e := range []string{
		`{"type":"input_audio_buffer.speech_started"}`, // user talking → listening
		`{"type":"response.created"}`,                  // thinking (no voice_state)
		`{"type":"output_audio_buffer.started"}`,       // reply audio begins → speaking
		`{"type":"output_audio_buffer.stopped"}`,       // reply drained → listening
		`{"type":"response.done"}`,                     // turn done → listening
	} {
		h.handleEvent(context.Background(), []byte(e))
	}
	want := []string{"listening", "speaking", "listening", "listening"}
	if len(states) != len(want) {
		t.Fatalf("voice states = %v, want %v", states, want)
	}
	for i := range want {
		if states[i] != want[i] {
			t.Fatalf("voice state[%d] = %q, want %q (full: %v)", i, states[i], want[i], states)
		}
	}

	// The precise WebRTC markers must also drive the mic gate.
	h2 := newEventHandler(disp, state, audio, func(any) error { return nil })
	h2.handleEvent(context.Background(), []byte(`{"type":"output_audio_buffer.started"}`))
	if !audio.dropCapture() {
		t.Error("output_audio_buffer.started must gate the mic")
	}
	h2.handleEvent(context.Background(), []byte(`{"type":"output_audio_buffer.stopped"}`))
	if audio.dropCapture() {
		t.Error("output_audio_buffer.stopped must ungate the mic")
	}
}
