package koe

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"time"
)

// eventHandler dispatches decoded oai-events and composes do_task. sendFn frames a
// value as an oai-events client message (e.g. a conversation.item.create with a
// function_call_output, then response.create). In production sendFn is the data
// channel SendText; in tests it captures.
type eventHandler struct {
	disp   *Dispatcher
	state  *CallState
	audio  *AudioIO // nil in unit tests; the production half-duplex gate target
	sendFn func(any) error
	// respBusy is true while a realtime response is generating. injectResult must
	// not send response.create while one is active (GA rejects it with
	// conversation_already_has_active_response — surfaced in the async-injection
	// de-risk). Maintained from response.created/response.done in handleEvent.
	respBusy atomic.Bool
}

func newEventHandler(disp *Dispatcher, state *CallState, audio *AudioIO, sendFn func(any) error) *eventHandler {
	return &eventHandler{disp: disp, state: state, audio: audio, sendFn: sendFn}
}

// sessionConfig builds the session.update event: persona instructions + Plan B's
// five tools. GA Realtime schema — output_modalities locks audio output and the
// voice lives under audio.output (the beta top-level "voice" + missing
// output_modalities silently fell back to TEXT output, so Koe never spoke and
// tool calls were emitted as text; verified against the live API in e2e_test.go).
// tool_choice stays "auto" — forcing a specific function under output_modalities
// ["audio"] makes GA emit the call as text instead of a real function call.
// turn-detection defaults to server-VAD (OpenAI segments).
func sessionConfig(persona, voice string) map[string]any {
	return map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type":              "realtime",
			"instructions":      persona,
			"output_modalities": []string{"audio"},
			"audio": map[string]any{
				"output": map[string]any{"voice": voice},
			},
			"tools":       ToolDefs(),
			"tool_choice": "auto",
		},
	}
}

// handleEvent routes one decoded oai-events message.
func (h *eventHandler) handleEvent(ctx context.Context, raw []byte) {
	var ev struct {
		Type      string          `json:"type"`
		Name      string          `json:"name"`      // function_call_arguments.done
		CallID    string          `json:"call_id"`   // function call id
		Arguments json.RawMessage `json:"arguments"` // function args (string-encoded JSON)
	}
	_ = json.Unmarshal(raw, &ev)
	switch ev.Type {
	case "response.created":
		// A response is now generating — injectResult must wait for its
		// response.done before sending another response.create.
		h.respBusy.Store(true)
	case "response.function_call_arguments.done":
		args := unwrapArgs(ev.Arguments)
		h.handleFunctionCall(ctx, ev.CallID, ev.Name, args)
	case "response.output_audio.delta":
		// Koe started/continues speaking → gate the mic so server-VAD doesn't
		// hear Koe through the speaker as a new turn (half-duplex echo control,
		// v1; C-full replaces with VPIO AEC). Event name follows the GA flattened
		// convention confirmed via the spike's response.output_audio_transcript.delta.
		if h.audio != nil {
			h.audio.SetSpeaking(true)
		}
	case "response.done":
		// Turn finished → ungate the mic + mark the response slot free. (Usage
		// token capture for billing is the deferred daemon usage-relay → Plan D.)
		h.respBusy.Store(false)
		if h.audio != nil {
			h.audio.SetSpeaking(false)
		}
	}
}

// unwrapArgs normalizes the arguments field: OpenAI sends function arguments as a
// JSON STRING, so "{\"task\":\"x\"}" must be unquoted to raw JSON bytes.
func unwrapArgs(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return []byte(`{}`)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []byte(s)
	}
	return raw // already an object
}

// handleFunctionCall composes do_task synchronously (C-minimal) or routes the
// fast tools through Dispatch, then sends the function_call_output back.
func (h *eventHandler) handleFunctionCall(ctx context.Context, callID, name string, args []byte) {
	if name == "do_task" {
		req, clarify, err := h.disp.PrepareDoTask(args)
		if err != nil {
			h.sendOutput(callID, SayResult{Status: "failed", Say: "我没听清，能再说一次吗？"})
			return
		}
		if clarify != nil {
			h.sendOutput(callID, *clarify)
			return
		}
		// C-full async (deferred-ack): fast-ack so Koe speaks a short "on it"
		// instead of going silent for the whole back-brain job, then run the turn
		// in a goroutine and inject the result so Koe voices it. ctx is the
		// Koe-process context (Connect's), so the turn is cancelled on Ctrl-C but
		// outlives this (already-closed) realtime turn.
		h.state.SetInFlight(req.Text)
		h.sendOutput(callID, SayResult{Status: "injected", Say: "在弄了"})
		go func() {
			out, derr := h.disp.client.DoTask(ctx, req)
			h.state.ClearInFlight()
			h.injectResult(ctx, MapDoTaskOutcome(out, derr))
		}()
		return
	}
	// Fast tools (cancel/get_status/control_app/switch_agent).
	outBytes, err := h.disp.Dispatch(ctx, name, args)
	if err != nil {
		h.sendOutput(callID, SayResult{Status: "failed", FailReason: err.Error()})
		return
	}
	var raw json.RawMessage = outBytes
	h.sendRaw(callID, raw)
}

// sendOutput frames a SayResult as a function_call_output + asks for a spoken
// response.
func (h *eventHandler) sendOutput(callID string, r SayResult) {
	b, _ := json.Marshal(r)
	h.sendRaw(callID, b)
}

func (h *eventHandler) sendRaw(callID string, output json.RawMessage) {
	_ = h.sendFn(map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type":    "function_call_output",
			"call_id": callID,
			"output":  string(output),
		},
	})
	_ = h.sendFn(map[string]any{"type": "response.create"})
}

// injectResult voices an async do_task result by injecting it as an assistant
// message + asking for a spoken response — the async-injection recipe verified
// live in e2e_test.go (a function_call_output won't re-voice; an assistant
// output_text message does). It serializes response.create against the in-flight
// (fast-ack) response so GA doesn't reject it with
// conversation_already_has_active_response.
func (h *eventHandler) injectResult(ctx context.Context, r SayResult) {
	if r.Status == "injected" || r.Say == "" {
		return // OutcomeInjected carries an empty say — nothing new to voice.
	}
	_ = h.sendFn(map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type":    "message",
			"role":    "assistant",
			"content": []map[string]any{{"type": "output_text", "text": r.Say}},
		},
	})
	h.waitRespIdle(ctx)
	_ = h.sendFn(map[string]any{"type": "response.create"})
}

// waitRespIdle blocks until no realtime response is generating (or ctx is done).
// In basic async the fast-ack response is long finished by the time DoTask
// returns, so this returns immediately; it guards the edge where DoTask is fast.
func (h *eventHandler) waitRespIdle(ctx context.Context) {
	for h.respBusy.Load() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}
