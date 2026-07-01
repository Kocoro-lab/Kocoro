package koe

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
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
	// respBusy is true while a realtime response is generating. The serialized
	// sender must not send response.create while one is active (GA rejects it with
	// conversation_already_has_active_response). Maintained from
	// response.created/response.done in handleEvent.
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
	// fullDuplexAEC means the local audio backend already has echo cancellation
	// (VPIO). Even in that mode, server interruption is off by default because a
	// laptop speaker/mic pair still needs an intent-level barge-in gate, not just
	// energy. Set KOE_INTERRUPT_RESPONSE=1 only for explicit barge-in experiments.
	fullDuplexAEC bool
	// Timing markers for event-log diagnostics. They are intentionally best-effort:
	// Realtime can emit multiple responses for one user turn (e.g. spoken ack then
	// function call), so these describe the most recent active segment.
	speechStartedAt   time.Time
	speechStoppedAt   time.Time
	responseCreatedAt time.Time
	outputStartedAt   time.Time
	responseDoneAt    time.Time
	// outputBufferActive tracks WebRTC playback markers. response.done can arrive
	// before the local output buffer is fully drained, so it must not immediately
	// release the echo gate while speaker tail is still audible.
	outputBufferActive atomic.Bool
	speakingEpoch      atomic.Int64
	// asyncTaskPending keeps Desktop/--once in "thinking" after the model's short
	// spoken ack while do_task is still running or its result speech is queued.
	asyncTaskPending atomic.Bool
	// Local speech endpoint fallback: Realtime VAD can miss low-energy post-VPIO
	// speech even after the local gate has opened. When local speech closes and the
	// server has not committed or created a response, Koe commits the input buffer
	// once and asks for a response.
	localSpeechSeq        atomic.Int64
	localStartCommitSeq   atomic.Int64
	localStartResponseSeq atomic.Int64
	inputCommitSeq        atomic.Int64
	responseSeq           atomic.Int64
	// Serialized response.create (runResponseSender), adapted from kocoro-reachy's
	// _response_sender_loop to Go/WebRTC: do_task results and fast-tool outputs still
	// need a MANUAL response.create, and GA rejects one sent while a response is
	// active (conversation_already_has_active_response). The naive fire-and-forget
	// silently dropped that turn. requestResponse() queues; the sender goroutine
	// sends serially, waits for respCreated/respRejected, and retries a rejection.
	respReq      chan responseCreateRequest // queued response.create requests
	respCreated  chan struct{}              // signalled (buffered 1) on response.created
	respRejected chan struct{}              // signalled (buffered 1) on the active-response error
}

type responseCreateRequest struct {
	instructions string
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

const (
	defaultSpeakingTailMS         = 900
	defaultOutputBufferStopWaitMS = 12000
	defaultLocalCommitFallbackMS  = 500
)

func (h *eventHandler) markSpeaking() {
	h.speakingEpoch.Add(1)
	if h.audio != nil {
		h.audio.SetPlaybackEnabled(true)
		h.audio.SetSpeaking(true)
	}
	h.emitVoiceState("speaking")
}

func (h *eventHandler) releaseSpeakingAfter(delay time.Duration) {
	epoch := h.speakingEpoch.Add(1)
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		<-timer.C
		if h.speakingEpoch.Load() != epoch {
			return
		}
		if h.audio != nil {
			h.audio.SetSpeaking(false)
			h.audio.SetPlaybackEnabled(false)
		}
		h.emitVoiceState(h.voiceStateAfterSpeaking())
	}()
}

func (h *eventHandler) voiceStateAfterSpeaking() string {
	if h.asyncTaskPending.Load() {
		return "thinking"
	}
	return "listening"
}

func (h *eventHandler) releaseSpeakingTail() {
	h.releaseSpeakingAfter(time.Duration(koeEnvInt("KOE_SPEAKING_TAIL_MS", defaultSpeakingTailMS)) * time.Millisecond)
}

func (h *eventHandler) releaseSpeakingAfterOutputBufferWait() {
	wait := time.Duration(koeEnvInt("KOE_OUTPUT_BUFFER_STOP_WAIT_MS", defaultOutputBufferStopWaitMS)) * time.Millisecond
	tail := time.Duration(koeEnvInt("KOE_SPEAKING_TAIL_MS", defaultSpeakingTailMS)) * time.Millisecond
	epoch := h.speakingEpoch.Add(1)
	go func() {
		timer := time.NewTimer(wait + tail)
		defer timer.Stop()
		<-timer.C
		if h.speakingEpoch.Load() != epoch {
			return
		}
		h.outputBufferActive.Store(false)
		if h.audio != nil {
			h.audio.SetSpeaking(false)
			h.audio.SetPlaybackEnabled(false)
		}
		h.emitVoiceState(h.voiceStateAfterSpeaking())
	}()
}

func (h *eventHandler) interruptOutput() {
	hadResponse := h.respBusy.Load()
	hadOutput := h.outputBufferActive.Load()
	if h.audio != nil && h.audio.dropCapture() {
		hadOutput = true
	}
	h.speakingEpoch.Add(1)
	h.outputBufferActive.Store(false)
	h.respBusy.Store(false)
	if h.audio != nil {
		h.audio.SetSpeaking(false)
		h.audio.SetPlaybackEnabled(false)
	}
	_ = h.sendFn(map[string]any{"type": "input_audio_buffer.clear"})
	if hadResponse {
		_ = h.sendFn(map[string]any{"type": "response.cancel"})
	}
	if hadOutput {
		_ = h.sendFn(map[string]any{"type": "output_audio_buffer.clear"})
	}
	h.emitVoiceState(h.voiceStateAfterSpeaking())
}

func (h *eventHandler) observeLocalSpeechStarted() {
	seq := h.localSpeechSeq.Add(1)
	h.localStartCommitSeq.Store(h.inputCommitSeq.Load())
	h.localStartResponseSeq.Store(h.responseSeq.Load())
	if eventLogEnabled() {
		log.Printf("koe[timing]: local_speech_started seq=%d", seq)
	}
}

func (h *eventHandler) observeLocalSpeechEnded(ctx context.Context) {
	if !koeEnvBool("KOE_LOCAL_COMMIT_FALLBACK", true) {
		return
	}
	seq := h.localSpeechSeq.Load()
	if seq == 0 {
		return
	}
	startCommitSeq := h.localStartCommitSeq.Load()
	startResponseSeq := h.localStartResponseSeq.Load()
	delay := time.Duration(koeEnvInt("KOE_LOCAL_COMMIT_FALLBACK_MS", defaultLocalCommitFallbackMS)) * time.Millisecond
	if eventLogEnabled() {
		log.Printf("koe[timing]: local_speech_ended seq=%d fallback_ms=%d", seq, delay.Milliseconds())
	}
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if h.localSpeechSeq.Load() != seq {
			return
		}
		if h.inputCommitSeq.Load() != startCommitSeq || h.responseSeq.Load() != startResponseSeq {
			return
		}
		if h.respBusy.Load() || h.outputBufferActive.Load() {
			return
		}
		if eventLogEnabled() {
			log.Printf("koe[timing]: local_commit_fallback seq=%d", seq)
		}
		_ = h.sendFn(map[string]any{"type": "input_audio_buffer.commit"})
		h.requestResponse()
	}()
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
		respReq:      make(chan responseCreateRequest, 8),
		respCreated:  make(chan struct{}, 1),
		respRejected: make(chan struct{}, 1),
	}
}

const (
	// maxResponseCreateRetries bounds retries when GA rejects an overlapping
	// response.create (mirrors kocoro-reachy's max_retries=5). WORKLOAD: async
	// do_task result voicing while another response is active; SYMPTOM if unhandled:
	// Kocoro silently skips the turn whose create was rejected. OVERRIDE: raise if a
	// slow back-brain keeps a response active longer than the retries cover.
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
	h.requestResponseWith(responseCreateRequest{})
}

func (h *eventHandler) requestResponseForSpeech(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		h.requestResponse()
		return
	}
	h.requestResponseWith(responseCreateRequest{instructions: exactSpeechInstructions(text)})
}

func (h *eventHandler) requestResponseWith(req responseCreateRequest) {
	select {
	case h.respReq <- req:
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
		case req := <-h.respReq:
			h.sendResponseCreate(ctx, req)
		}
	}
}

func (h *eventHandler) sendResponseCreate(ctx context.Context, req responseCreateRequest) {
	for attempt := 0; attempt <= maxResponseCreateRetries; attempt++ {
		if !h.waitRespIdle(ctx) {
			return // ctx done
		}
		drainSignal(h.respCreated) // clear stale acks from the previous turn
		drainSignal(h.respRejected)
		_ = h.sendFn(responseCreatePayload(req))
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

func responseCreatePayload(req responseCreateRequest) map[string]any {
	payload := map[string]any{"type": "response.create"}
	if strings.TrimSpace(req.instructions) != "" {
		payload["response"] = map[string]any{"instructions": req.instructions}
	}
	return payload
}

func exactSpeechInstructions(text string) string {
	text = strings.ReplaceAll(strings.TrimSpace(text), "</spoken_summary>", "</spoken-summary>")
	return "Speak the completed Kocoro result to the user. Say exactly the text between <spoken_summary> and </spoken_summary>. Do not add a greeting, preface, follow-up question, extra fact, markdown, JSON, or tool detail.\n<spoken_summary>\n" + text + "\n</spoken_summary>"
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

func eventLogEnabled() bool { return os.Getenv("KOE_EVENT_LOG") == "1" }

func transcriptLogEnabled() bool { return os.Getenv("KOE_TRANSCRIPT_LOG") == "1" }

func logMaybeText(s string, max int) string {
	if transcriptLogEnabled() {
		return shortLogString(s, max)
	}
	return fmt.Sprintf("<redacted chars=%d>", len([]rune(s)))
}

func logMaybeBytes(b []byte, max int) string {
	if transcriptLogEnabled() {
		return shortLogString(string(b), max)
	}
	return fmt.Sprintf("<redacted bytes=%d>", len(b))
}

func shortLogString(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if max > 0 && len(r) > max {
		return string(r[:max]) + "..."
	}
	return s
}

func elapsedMS(from, to time.Time) int64 {
	if from.IsZero() || to.IsZero() {
		return -1
	}
	return to.Sub(from).Milliseconds()
}

func outcomeKindLog(kind OutcomeKind) string {
	switch kind {
	case OutcomeCompleted:
		return "completed"
	case OutcomeInjected:
		return "injected"
	case OutcomeRejected:
		return "rejected"
	default:
		return "unknown"
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
// Turn detection uses Realtime VAD with create_response=true: OpenAI owns turn
// segmentation and starts the spoken response automatically. Desktop defaults to
// semantic_vad because it is less eager on ambient/noisy audio while still deciding
// end-of-turn server-side. Set KOE_TURN_DETECTION=server_vad to compare the lower
// latency deterministic path.
// Server-side interruption is disabled by default even with VPIO/AEC: without a
// reliable intent gate, server-side barge-in is exactly how residual speaker echo
// turns into self-interruption. Set KOE_INTERRUPT_RESPONSE=1 only for explicit
// barge-in experiments. Far-field noise reduction is enabled by default for the
// laptop speaker/mic case; set KOE_NOISE_REDUCTION=off to compare raw input.
func sessionConfig(persona, voice string, fullDuplexAEC bool) map[string]any {
	vadThreshold := koeEnvFloat("KOE_VAD_THRESHOLD", 0.50)
	vadSilenceMS := koeEnvInt("KOE_VAD_SILENCE_MS", 900)
	interruptResponse := false
	if fullDuplexAEC {
		interruptResponse = koeEnvBool("KOE_INTERRUPT_RESPONSE", false)
	}
	var turnDetection map[string]any
	if strings.EqualFold(koeEnvString("KOE_TURN_DETECTION", "semantic_vad"), "semantic_vad") {
		turnDetection = map[string]any{
			"type":               "semantic_vad",
			"eagerness":          koeEnvString("KOE_SEMANTIC_VAD_EAGERNESS", "low"),
			"create_response":    true,
			"interrupt_response": interruptResponse,
		}
	} else {
		turnDetection = map[string]any{
			"type":                "server_vad",
			"threshold":           vadThreshold,
			"prefix_padding_ms":   300,
			"silence_duration_ms": vadSilenceMS,
			"create_response":     true,
			"interrupt_response":  interruptResponse,
		}
	}
	input := map[string]any{
		"transcription": map[string]any{
			"model": "gpt-4o-mini-transcribe",
		},
		"turn_detection": turnDetection,
	}
	noiseReduction := koeEnvString("KOE_NOISE_REDUCTION", "far_field")
	if !strings.EqualFold(noiseReduction, "off") {
		input["noise_reduction"] = map[string]any{"type": noiseReduction}
	}
	return map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type":              "realtime",
			"instructions":      persona,
			"output_modalities": []string{"audio"},
			"audio": map[string]any{
				"input":  input,
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
	if os.Getenv("KOE_EVENT_LOG") == "1" {
		log.Printf("koe[event]: %s", ev.Type)
	}
	switch ev.Type {
	case "input_audio_buffer.speech_started":
		h.speechStartedAt = time.Now()
		if eventLogEnabled() {
			log.Printf("koe[timing]: speech_started")
		}
		// Server-VAD detected the user talking — the reactive "I hear you" moment.
		// In the default VPIO policy, local capture is still muted while Kocoro
		// speaks, so self-echo should not reach server VAD.
		h.emitVoiceState("listening")
	case "input_audio_buffer.speech_stopped":
		h.speechStoppedAt = time.Now()
		if eventLogEnabled() {
			log.Printf("koe[timing]: speech_stopped speech_ms=%d", elapsedMS(h.speechStartedAt, h.speechStoppedAt))
		}
		// The user finished talking. create_response=true lets the server start the
		// spoken response automatically.
	case "input_audio_buffer.committed":
		h.inputCommitSeq.Add(1)
	case "conversation.item.input_audio_transcription.completed":
		h.handleInputTranscript(ev.Transcript)
	case "conversation.item.input_audio_transcription.failed":
		// Treat failed ASR like unclear audio. Do not guess.
		h.emitVoiceState("listening")
	case "response.created":
		h.responseSeq.Add(1)
		h.responseCreatedAt = time.Now()
		if eventLogEnabled() {
			log.Printf("koe[timing]: response_created after_speech_stop_ms=%d", elapsedMS(h.speechStoppedAt, h.responseCreatedAt))
		}
		h.asyncTaskPending.Store(false)
		// A response is now generating — the serialized sender waits for its
		// response.done before sending the next response.create. Gate capture
		// immediately, not only once output_audio_buffer.started arrives: otherwise
		// slow tail audio / room noise in the response-created→first-audio gap can
		// become the next user turn.
		h.respBusy.Store(true)
		if h.audio != nil {
			h.audio.SetPlaybackEnabled(true)
			h.audio.SetSpeaking(true)
		}
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
		if eventLogEnabled() {
			log.Printf("koe[tool]: call name=%q call_id=%q args=%s", ev.Name, ev.CallID, logMaybeBytes(args, 500))
		}
		h.handleFunctionCall(ctx, ev.CallID, ev.Name, args)
	case "output_audio_buffer.started":
		h.outputStartedAt = time.Now()
		if eventLogEnabled() {
			log.Printf("koe[timing]: output_started after_response_created_ms=%d", elapsedMS(h.responseCreatedAt, h.outputStartedAt))
		}
		// WebRTC-only: the server began streaming reply audio — the PRECISE
		// THINKING→SPEAKING boundary (cleaner than inferring from the first audio
		// delta). This drives the local speaking gate so playback does not feed back
		// into the next turn.
		h.outputBufferActive.Store(true)
		h.markSpeaking()
	case "response.output_audio.delta":
		// Redundant safety: also gate on the first audio delta in case the
		// output_audio_buffer.* markers are absent on some transport. Idempotent
		// with output_audio_buffer.started. Event name is the GA flattened
		// convention.
		h.markSpeaking()
	case "output_audio_buffer.stopped":
		now := time.Now()
		if eventLogEnabled() {
			log.Printf("koe[timing]: output_stopped after_response_done_ms=%d output_ms=%d", elapsedMS(h.responseDoneAt, now), elapsedMS(h.outputStartedAt, now))
		}
		if !h.outputBufferActive.Swap(false) {
			if eventLogEnabled() {
				log.Printf("koe[timing]: output_stopped ignored after local release")
			}
			return
		}
		// WebRTC-only: reply audio fully drained (fires after response.done) — the
		// PRECISE SPEAKING→IDLE boundary. Keep a short local tail because CoreAudio
		// can still have speaker energy after the server says its output buffer ended.
		h.releaseSpeakingTail()
	case "response.done":
		h.responseDoneAt = time.Now()
		if eventLogEnabled() {
			log.Printf("koe[timing]: response_done response_ms=%d output_elapsed_ms=%d", elapsedMS(h.responseCreatedAt, h.responseDoneAt), elapsedMS(h.outputStartedAt, h.responseDoneAt))
		}
		// Turn finished → mark the response slot free. Do not immediately ungate the
		// mic if output_audio_buffer.started fired; response.done can precede local
		// playback drain, and releasing here lets Koe hear its own tail.
		h.respBusy.Store(false)
		if h.outputBufferActive.Load() {
			h.releaseSpeakingAfterOutputBufferWait()
		} else {
			h.releaseSpeakingTail()
		}
		h.reportUsage(raw)
	case "response.output_audio_transcript.done":
		if transcriptLogEnabled() && ev.Transcript != "" {
			log.Printf("koe[assistant]: %q", shortLogString(ev.Transcript, 500))
		}
	}
}

// handleInputTranscript logs the user's transcript for diagnostics only. Under
// create_response:true the server already auto-creates the response, so this must
// NOT send response.create. Off by default (privacy: user voice content); opt in
// with KOE_TRANSCRIPT_LOG=1.
func (h *eventHandler) handleInputTranscript(transcript string) {
	if os.Getenv("KOE_TRANSCRIPT_LOG") == "1" {
		log.Printf("koe[transcript]: %q", transcript)
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
			if eventLogEnabled() {
				log.Printf("koe[task]: prepare failed call_id=%q err=%v args=%s", callID, err, logMaybeBytes(args, 500))
			}
			h.sendOutput(callID, SayResult{Status: "failed", SpokenSummary: "我没听清，能再说一次吗？", Say: "我没听清，能再说一次吗？"})
			return
		}
		if clarify != nil {
			if eventLogEnabled() {
				log.Printf("koe[task]: clarify call_id=%q status=%s say_len=%d", callID, clarify.Status, len([]rune(clarify.Say)))
			}
			h.sendOutput(callID, *clarify)
			return
		}
		// reachy say-and-ask: the model speaks its own short ack out loud in the
		// call turn (persona-driven), so we do NOT inject a placeholder fast-ack —
		// that extra voiced turn is where the model improvised a guessed answer. Run
		// the back-brain turn in the background and feed the REAL result back as the
		// single function_call_output for this call_id, then voice it (mirroring
		// reachy's BackgroundToolManager). ctx is Connect's, cancelled on Ctrl-C.
		h.state.SetInFlightForAgent(req.Text, req.Agent)
		h.asyncTaskPending.Store(true)
		h.emitVoiceState("thinking") // delegating; the model's call-turn ack already played
		go func() {
			started := time.Now()
			if eventLogEnabled() {
				log.Printf("koe[task]: start call_id=%q agent=%q burst=%q task=%s", callID, req.Agent, req.ThreadID, logMaybeText(req.Text, 500))
			}
			out, derr := h.disp.client.DoTask(ctx, req)
			h.state.ClearInFlightForAgent(req.Agent)
			r := MapDoTaskOutcome(out, derr)
			if eventLogEnabled() {
				log.Printf("koe[task]: done call_id=%q kind=%s status=%s session=%q partial=%t failure=%q reason=%q spoken_len=%d reply_len=%d duration_ms=%d err=%v",
					callID, outcomeKindLog(out.Kind), r.Status, out.SessionID, out.Partial, out.FailureCode, out.Reason,
					len([]rune(r.SpokenSummary)), len([]rune(out.Reply)), time.Since(started).Milliseconds(), derr)
			}
			b, _ := json.Marshal(r)
			h.sendFunctionOutput(callID, b) // satisfy the protocol for this call_id
			if eventLogEnabled() {
				log.Printf("koe[tool]: output call_id=%q status=%s voice=%t output=%s", callID, r.Status, r.Status != "injected" && r.Say != "", logMaybeBytes(b, 500))
			}
			if r.Status != "injected" && r.Say != "" {
				h.requestResponseForSpeech(r.Say) // voice the result (skip when the daemon already replied)
			} else {
				h.asyncTaskPending.Store(false)
				h.emitVoiceState("listening")
			}
		}()
		return
	}
	// Fast tools (cancel/get_status/control_app/switch_agent).
	outBytes, err := h.disp.Dispatch(ctx, name, args)
	if err != nil {
		if eventLogEnabled() {
			log.Printf("koe[tool]: dispatch failed name=%q call_id=%q err=%v args=%s", name, callID, err, logMaybeBytes(args, 500))
		}
		h.sendOutput(callID, SayResult{Status: "failed", FailReason: err.Error()})
		return
	}
	if eventLogEnabled() {
		log.Printf("koe[tool]: dispatch done name=%q call_id=%q output=%s", name, callID, logMaybeBytes(outBytes, 500))
	}
	var raw json.RawMessage = outBytes
	h.sendRaw(callID, raw)
}

// sendOutput frames a SayResult as a function_call_output + asks for a spoken
// response (the synchronous error/clarify + fast-tool path).
func (h *eventHandler) sendOutput(callID string, r SayResult) {
	b, _ := json.Marshal(r)
	h.sendFunctionOutput(callID, b)
	if r.Say != "" {
		h.requestResponseForSpeech(r.Say)
		return
	}
	h.requestResponse()
}

// sendFunctionOutput submits the function_call_output for call_id (required by the
// protocol after a function_call). It does NOT request a voiced response — the
// caller decides whether to voice (the async do_task result voices; an
// already-replied/injected outcome does not).
func (h *eventHandler) sendFunctionOutput(callID string, output json.RawMessage) {
	_ = h.sendFn(map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type":    "function_call_output",
			"call_id": callID,
			"output":  string(output),
		},
	})
}

func (h *eventHandler) sendRaw(callID string, output json.RawMessage) {
	h.sendFunctionOutput(callID, output)
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
