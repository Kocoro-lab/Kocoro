package koe

import (
	"context"
	"encoding/json"
)

// eventHandler dispatches decoded oai-events and composes do_task. sendFn frames a
// value as an oai-events client message (e.g. a conversation.item.create with a
// function_call_output, then response.create). In production sendFn is the data
// channel SendText; in tests it captures.
type eventHandler struct {
	disp   *Dispatcher
	state  *CallState
	sendFn func(any) error
}

func newEventHandler(disp *Dispatcher, state *CallState, sendFn func(any) error) *eventHandler {
	return &eventHandler{disp: disp, state: state, sendFn: sendFn}
}

// sessionConfig builds the session.update event: persona instructions + Plan B's
// five tools. Voice/turn-detection defaults are server-VAD (OpenAI segments).
func sessionConfig(persona, voice string) map[string]any {
	return map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type":         "realtime",
			"instructions": persona,
			"voice":        voice,
			"tools":        ToolDefs(),
			"tool_choice":  "auto",
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
	case "response.function_call_arguments.done":
		args := unwrapArgs(ev.Arguments)
		h.handleFunctionCall(ctx, ev.CallID, ev.Name, args)
	case "response.output_audio.delta":
		// Koe started speaking → gate the mic (half-duplex echo control).
		// (No-op here if audio is nil; wired in Task 5.)
	case "response.done":
		// Turn finished → ungate mic; usage is captured for billing by the deferred daemon usage-relay (→ Plan D Cloud ingest).
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
		// C-minimal: BLOCK for the back-brain turn (simplest first loop). C-full
		// fast-acks "在弄了" and runs this in a goroutine via the same mapper.
		h.state.SetInFlight(req.Text)
		out, derr := h.disp.client.DoTask(ctx, req)
		h.state.ClearInFlight()
		h.sendOutput(callID, MapDoTaskOutcome(out, derr))
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
