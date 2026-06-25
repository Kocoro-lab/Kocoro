package koe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureSender records every oai-events client message the handler sends.
// handleFunctionCall sends TWO frames per call (the function_call_output, then a
// response.create), so we keep all of them rather than only the last — otherwise
// the trailing response.create would mask the function_call_output we assert on.
type captureSender struct{ sent []map[string]any }

func (c *captureSender) send(v any) error {
	b, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	c.sent = append(c.sent, m)
	return nil
}

// sentContains reports whether any captured frame's JSON contains sub.
func (c *captureSender) sentContains(sub string) bool {
	for _, m := range c.sent {
		b, _ := json.Marshal(m)
		if strings.Contains(string(b), sub) {
			return true
		}
	}
	return false
}

func TestHandleFunctionCallDoTask(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"reply": "It's sunny.", "agent": "default"})
	}))
	defer srv.Close()

	// ONE CallState shared by the dispatcher and the event handler, mirroring
	// production Connect (Task 5): SetInFlight on the handler must be visible to a
	// get_status routed through the same dispatcher.
	state := NewCallState("burst-x", "")
	disp := NewDispatcher(NewDaemonClient(srv.URL), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	cap := &captureSender{}
	h := newEventHandler(disp, state, nil, cap.send)

	h.handleFunctionCall(context.Background(), "call-1", "do_task", []byte(`{"task":"weather?"}`))

	// The function_call_output frame must carry the say-contract reply.
	if !cap.sentContains("It's sunny.") {
		t.Errorf("no sent frame carried the reply say; sent=%v", cap.sent)
	}
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
