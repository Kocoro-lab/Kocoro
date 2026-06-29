package koe

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
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
	// onVoiceState (nil-safe) pushes the ambient voice state to the Desktop control
	// channel (G2) so the Kocoro Island sprite tracks listening/thinking/speaking.
	onVoiceState func(string)
	// curState holds the last emitted voice state (string) so the D3w level pump
	// knows whether to report input (listening) or output (speaking) RMS.
	curState atomic.Value
	// model + onUsage (nil-safe) report per-turn token usage for billing (G3): on
	// each response.done, build {model, response_id, usage} and fire onUsage, which
	// relays via the daemon to Cloud (server-side cost). Koe never sees pricing.
	model   string
	onUsage func(json.RawMessage)
	// Serialized response.create (runResponseSender), adapted from kocoro-reachy's
	// _response_sender_loop to Go/WebRTC: under create_response=false every reply is
	// a MANUAL response.create, and GA rejects one sent while a response is still
	// active (conversation_already_has_active_response). The naive fire-and-forget
	// silently dropped that turn. requestResponse() queues; the sender goroutine
	// sends serially, waits for respCreated/respRejected, and retries a rejection.
	respReq      chan struct{} // queued response.create requests
	respCreated  chan struct{} // signalled (buffered 1) on response.created
	respRejected chan struct{} // signalled (buffered 1) on the active-response error
}

func (h *eventHandler) emitVoiceState(state string) {
	h.curState.Store(state)
	if h.onVoiceState != nil {
		h.onVoiceState(state)
	}
}

// voiceState returns the last emitted voice state ("idle" before the first one).
func (h *eventHandler) voiceState() string {
	if v := h.curState.Load(); v != nil {
		return v.(string)
	}
	return "idle"
}

// bargeIn cuts Kocoro off mid-reply when the user talks over it (E2): cancel the
// in-flight response, clear the server's WebRTC output-audio buffer, and drop the
// locally-queued playback so Kocoro goes quiet immediately. The caller flips the
// voice state to listening.
func (h *eventHandler) bargeIn() {
	_ = h.sendFn(map[string]any{"type": "response.cancel"})
	_ = h.sendFn(map[string]any{"type": "output_audio_buffer.clear"})
	if h.audio != nil {
		h.audio.SetSpeaking(false)
		h.audio.ClearPlayback()
	}
	h.respBusy.Store(false)
}

// reportUsage extracts response_id + usage from a response.done event and fires
// the billing relay (fire-and-forget; a usage failure must not break the call).
func (h *eventHandler) reportUsage(raw []byte) {
	if h.onUsage == nil {
		return
	}
	var rd struct {
		Response struct {
			ID    string          `json:"id"`
			Usage json.RawMessage `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &rd); err != nil || rd.Response.ID == "" || len(rd.Response.Usage) == 0 {
		return // no usage on this response.done (e.g. an early/failed turn)
	}
	body, err := json.Marshal(map[string]any{
		"model":       h.model,
		"response_id": rd.Response.ID,
		"usage":       rd.Response.Usage,
	})
	if err != nil {
		return
	}
	h.onUsage(body)
}

func newEventHandler(disp *Dispatcher, state *CallState, audio *AudioIO, sendFn func(any) error) *eventHandler {
	return &eventHandler{
		disp: disp, state: state, audio: audio, sendFn: sendFn,
		respReq:      make(chan struct{}, 8),
		respCreated:  make(chan struct{}, 1),
		respRejected: make(chan struct{}, 1),
	}
}

const (
	// maxResponseCreateRetries bounds retries when GA rejects an overlapping
	// response.create (mirrors kocoro-reachy's max_retries=5). WORKLOAD: rapid
	// turns under create_response=false; SYMPTOM if unhandled: Kocoro silently skips
	// the turn whose create was rejected. OVERRIDE: raise if a slow back-brain keeps
	// a response active longer than the retries cover.
	maxResponseCreateRetries = 5
	// responseCreateAckTimeout caps the wait for response.created / a rejection after
	// sending. A turn with nothing to say yields neither; we stop rather than spin.
	responseCreateAckTimeout = 5 * time.Second
	// responseRejectRetryDelay spaces retries so we don't hammer the server while an
	// active response drains.
	responseRejectRetryDelay = 150 * time.Millisecond
)

// requestResponse queues exactly one response.create. The serialized sender does the
// actual send — decoupled from the event-handler goroutine so waiting for the
// server's ack can never deadlock the event loop (handleEvent must keep running to
// deliver response.created / response.done).
func (h *eventHandler) requestResponse() {
	select {
	case h.respReq <- struct{}{}:
	default: // queue saturated (a request flood) — drop rather than block the loop
	}
}

// runResponseSender is Koe's serialized response.create worker (started by Connect),
// adapted from kocoro-reachy's _response_sender_loop. For each queued request it
// waits for any active response to finish, sends response.create, waits for
// response.created or the active-response rejection, and retries a rejection.
func (h *eventHandler) runResponseSender(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.respReq:
			h.sendResponseCreate(ctx)
		}
	}
}

func (h *eventHandler) sendResponseCreate(ctx context.Context) {
	for attempt := 0; attempt <= maxResponseCreateRetries; attempt++ {
		if !h.waitRespIdle(ctx) {
			return // ctx done
		}
		drainSignal(h.respCreated) // clear stale acks from the previous turn
		drainSignal(h.respRejected)
		_ = h.sendFn(map[string]any{"type": "response.create"})
		select {
		case <-ctx.Done():
			return
		case <-h.respCreated:
			return // accepted
		case <-h.respRejected:
			// Overlapped an active response — wait a beat for it to drain, then retry.
			select {
			case <-ctx.Done():
				return
			case <-time.After(responseRejectRetryDelay):
			}
		case <-time.After(responseCreateAckTimeout):
			return // neither created nor rejected (nothing to say) — don't spin
		}
	}
}

func drainSignal(c chan struct{}) {
	select {
	case <-c:
	default:
	}
}

func signalNonBlocking(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// sessionConfig builds the session.update event: persona instructions + Plan B's
// five tools. GA Realtime schema — output_modalities locks audio output and the
// voice lives under audio.output (the beta top-level "voice" + missing
// output_modalities silently fell back to TEXT output, so Koe never spoke and
// tool calls were emitted as text; verified against the live API in e2e_test.go).
// tool_choice stays "auto" — forcing a specific function under output_modalities
// ["audio"] makes GA emit the call as text instead of a real function call.
//
// Turn detection uses server-VAD, but create_response is false: OpenAI segments
// and transcribes the user's turn, then Koe decides whether the transcript is
// clear enough to answer. This mirrors kocoro-reachy's "first door" discipline
// for the Desktop push-to-talk shape: noise / stray words should not make Kocoro
// improvise a reply.
func sessionConfig(persona, voice string) map[string]any {
	return map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type":              "realtime",
			"instructions":      persona,
			"output_modalities": []string{"audio"},
			"audio": map[string]any{
				"input": map[string]any{
					"transcription": map[string]any{
						"model": "gpt-4o-mini-transcribe",
					},
					"turn_detection": map[string]any{
						"type":                "server_vad",
						"threshold":           0.65,
						"prefix_padding_ms":   300,
						"silence_duration_ms": 700,
						"create_response":     true,
						"interrupt_response":  true,
					},
				},
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
		Type       string          `json:"type"`
		Name       string          `json:"name"`      // function_call_arguments.done
		CallID     string          `json:"call_id"`   // function call id
		Arguments  json.RawMessage `json:"arguments"` // function args (string-encoded JSON)
		Transcript string          `json:"transcript"`
		Error      struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"` // type=="error" events
	}
	_ = json.Unmarshal(raw, &ev)
	switch ev.Type {
	case "input_audio_buffer.speech_started":
		// Server-VAD detected the user talking. If this lands WHILE Kocoro is speaking
		// it is a barge-in (E2) — cut Kocoro off. (Only reachable on a full-duplex/AEC
		// backend: the v1 half-duplex gate mutes the mic while speaking, so the server
		// can't hear the user then. With VPIO the gate is moot and this fires for real.)
		if h.voiceState() == "speaking" {
			h.bargeIn()
		}
		// The reactive "I hear you" moment (Q4) + barge-in entry: we are listening.
		h.emitVoiceState("listening")
	case "input_audio_buffer.speech_stopped":
		// The user finished talking. create_response=false, so the server will
		// transcribe this turn and Koe will decide whether to answer.
	case "conversation.item.input_audio_transcription.completed":
		h.handleInputTranscript(ctx, ev.Transcript)
	case "conversation.item.input_audio_transcription.failed":
		// Treat failed ASR like unclear audio. Do not guess.
		h.emitVoiceState("listening")
	case "response.created":
		// A response is now generating — injectResult must wait for its
		// response.done before sending another response.create.
		h.respBusy.Store(true)
		signalNonBlocking(h.respCreated) // ack the sender's pending response.create
	case "error":
		// GA rejects a response.create sent while a response is active. Signal the
		// sender to retry instead of silently losing the turn (the exact code
		// kocoro-reachy matches: conversation_already_has_active_response).
		if ev.Error.Code == "conversation_already_has_active_response" {
			signalNonBlocking(h.respRejected)
		}
	case "response.function_call_arguments.done":
		args := unwrapArgs(ev.Arguments)
		h.handleFunctionCall(ctx, ev.CallID, ev.Name, args)
	case "output_audio_buffer.started":
		// WebRTC-only: the server began streaming reply audio — the PRECISE
		// THINKING→SPEAKING boundary (cleaner than inferring from the first audio
		// delta). Gate the mic so server-VAD doesn't hear Koe through the speaker.
		if h.audio != nil {
			h.audio.SetSpeaking(true)
		}
		h.emitVoiceState("speaking")
	case "response.output_audio.delta":
		// Redundant safety: also gate on the first audio delta in case the
		// output_audio_buffer.* markers are absent on some transport. Idempotent
		// with output_audio_buffer.started. Half-duplex echo control (v1; VPIO AEC
		// supersedes). Event name is the GA flattened convention.
		if h.audio != nil {
			h.audio.SetSpeaking(true)
		}
		h.emitVoiceState("speaking")
	case "output_audio_buffer.stopped":
		// WebRTC-only: reply audio fully drained (fires after response.done) — the
		// PRECISE SPEAKING→IDLE boundary. Ungate the mic.
		if h.audio != nil {
			h.audio.SetSpeaking(false)
		}
		h.emitVoiceState("listening")
	case "response.done":
		// Turn finished → ungate the mic + mark the response slot free. (Usage
		// token capture for billing is the deferred daemon usage-relay → Plan D.)
		h.respBusy.Store(false)
		if h.audio != nil {
			h.audio.SetSpeaking(false)
		}
		h.emitVoiceState("listening")
		h.reportUsage(raw)
	}
}

// handleInputTranscript is the response gate for one user turn. With
// create_response=false, Realtime will not answer until Koe sends response.create.
// That lets us suppress noise, filler, and too-short stray speech instead of
// letting the model hallucinate a conversational turn.
func (h *eventHandler) handleInputTranscript(ctx context.Context, transcript string) {
	if !shouldAnswerTranscript(transcript) {
		h.emitVoiceState("listening")
		return
	}
	h.requestResponse()
}

func shouldAnswerTranscript(transcript string) bool {
	norm := normalizeTranscript(transcript)
	if norm == "" {
		return false
	}
	if isFillerTranscript(norm) {
		return false
	}
	// One CJK rune or one short ASCII token is usually a false wake/noise fragment
	// in live mic use ("嗯", "啊", "uh"). Let two-rune turns like "你好" through.
	runes := []rune(norm)
	if len(runes) < 2 {
		return false
	}
	if len(runes) <= 3 && isAllASCII(runes) {
		return false
	}
	return true
}

func normalizeTranscript(transcript string) string {
	return strings.TrimFunc(strings.ToLower(transcript), func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r)
	})
}

func isFillerTranscript(norm string) bool {
	switch norm {
	case "um", "uh", "umm", "hmm", "mm", "ah", "oh", "er",
		"嗯", "呃", "啊", "哦", "额", "唔", "哎", "诶",
		"嗯嗯", "啊啊", "哦哦", "呃呃",
		"はい", "えっと", "あの", "うん":
		return true
	default:
		return false
	}
}

func isAllASCII(runes []rune) bool {
	for _, r := range runes {
		if r > unicode.MaxASCII {
			return false
		}
	}
	return true
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
		h.sendOutput(callID, SayResult{Status: "injected", Say: taskAcknowledgement(req.Text)})
		go func() {
			h.emitVoiceState("thinking") // delegating to the back-brain
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

func taskAcknowledgement(task string) string {
	for _, r := range task {
		if unicode.In(r, unicode.Hiragana, unicode.Katakana) {
			return "確認します。終わったら伝えます。"
		}
	}
	for _, r := range task {
		if unicode.In(r, unicode.Han) {
			return "我来处理，弄好就告诉你。"
		}
	}
	return "I'll handle that and tell you when it's ready."
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
	h.requestResponse()
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
	h.requestResponse()
}

// waitRespIdle blocks until no realtime response is generating, returning true when
// idle and false if ctx is done. Called only by the response sender goroutine (never
// the event-handler goroutine), so it can poll respBusy without deadlocking the loop
// that clears it.
func (h *eventHandler) waitRespIdle(ctx context.Context) bool {
	for h.respBusy.Load() {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(20 * time.Millisecond):
		}
	}
	return true
}
