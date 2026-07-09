//go:build darwin && cgo

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
	// onEndCall (nil-safe) tears the call down when the model calls the end_call
	// voice tool (dismiss / hang up). In the Desktop path it is the endCall closure
	// (plays the goodbye earcon, then closes the session + audio); the standalone/CLI
	// path wires it to a goodbye earcon + process exit; nil only in unit tests, where
	// end_call is a no-op.
	onEndCall func()
	// curState holds the last emitted voice state (string) so the D3w level pump
	// knows whether to report input (listening) or output (speaking) RMS.
	curState atomic.Value
	// model + onUsage (nil-safe) report per-turn token usage for billing (G3): on
	// each response.done, build {model, response_id, usage} and fire onUsage, which
	// relays via the daemon to Cloud (server-side cost). Koe never sees pricing.
	model   string
	onUsage func(json.RawMessage)
	// language is the user-pinned koe reply language ("en"/"ja"/"zh"; "" = follow the
	// utterance). It picks the language of the MECHANICAL spoken fallbacks (transport
	// failure / busy / misheard / agent clarify) that bypass the Realtime model; when
	// empty, fallbackLang infers from the utterance. Set from ConnectOptions.Language.
	language string
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
	// barged is set when a barge-in / explicit interrupt supersedes the active
	// response, and cleared on the next response.created. While set, markSpeaking
	// ignores the cancelled response's trailing audio deltas so they cannot re-open
	// the playback the interrupt just stopped.
	barged atomic.Bool
	// asyncTaskPending keeps Desktop/--once in "thinking" after the model's short
	// spoken ack while do_task is still running or its result speech is queued.
	asyncTaskPending atomic.Bool
	// Local speech endpoint fallback (opt-in, KOE_LOCAL_COMMIT_FALLBACK=1):
	// Realtime VAD can miss low-energy post-VPIO speech even after the local gate
	// has opened. When local speech closes and the server has not committed or
	// created a response, Koe commits the input buffer once and asks for a
	// response. Default off since 2026-07-09: far-field/noisy rooms (Reachy) open
	// the gate on fragments, and the resulting commit_empty rejections turned
	// into spoken "could not hear you" loops.
	localSpeechSeq        atomic.Int64
	localStartCommitSeq   atomic.Int64
	localStartResponseSeq atomic.Int64
	inputCommitSeq        atomic.Int64
	// commitEmptySeq counts input_audio_buffer_commit_empty rejections. The
	// fallback's ack wait snapshots it before the manual commit: a bump means the
	// buffer held (nearly) no audio — the gate opened on a fragment, not a lost
	// utterance — so the fallback drops the turn silently instead of asking the
	// user to repeat.
	commitEmptySeq atomic.Int64
	responseSeq    atomic.Int64
	// lastDoTaskCommitSeq is inputCommitSeq snapshotted at the MOST RECENT do_task
	// dispatch. A completed result compares it against inputCommitSeq at land-time: if
	// the user committed a turn since the last do_task (and it did not itself become a
	// do_task, else lastDoTaskCommitSeq would have advanced), they moved on to plain
	// conversation — the result is stale, suppress the voicing. A follow-up that
	// refines the task advances this marker, so its combined result still voices.
	// Comparing against the LAST do_task (not each result's own dispatch) is what makes
	// "asked for email, it ran 60s, meanwhile I moved on to another topic" suppress
	// correctly. See shouldVoiceDoTaskResult.
	lastDoTaskCommitSeq atomic.Int64
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
	defaultSpeakingTailMS = 900
	// defaultOutputBufferStopWaitMS is the HARD CAP backstop for a lost
	// output_audio_buffer.stopped event — no longer the primary release (that is
	// the drain-aware PlaybackIdle poll). WORKLOAD: long do_task result reads play
	// 15-30s+ past response.done; the old 12s cap cut them mid-word (2026-07-02
	// "Koe interrupts itself"). SYMPTOM if too low: long replies truncated; if too
	// high: a wedged level reading keeps the mic muted this long. OVERRIDE:
	// KOE_OUTPUT_BUFFER_STOP_WAIT_MS.
	defaultOutputBufferStopWaitMS = 60000
	// defaultPlaybackIdleHoldMS is how long the output level must stay silent
	// before the watchdog treats playout as drained. WORKLOAD: TTS speech has
	// sub-second pauses at sentence boundaries. SYMPTOM if too low: a long pause
	// reads as drained and the tail gets cut; if too high: dead air before the mic
	// reopens when the stop event was lost. OVERRIDE: KOE_PLAYBACK_IDLE_HOLD_MS.
	defaultPlaybackIdleHoldMS = 1500
	// playbackIdlePollInterval paces the drain poll; only bounds how quickly the
	// idle hold and hard cap are noticed.
	playbackIdlePollInterval = 25 * time.Millisecond

	defaultLocalCommitFallbackMS = 500
	// defaultLocalCommitAckMS is how long the fallback waits for the server to ack
	// its manual input_audio_buffer.commit before concluding the user's audio never
	// reached the server. WORKLOAD: one WebRTC data-channel round trip (~100-300 ms
	// live). SYMPTOM if too low: a commit that would have landed gets answered with
	// the ask-to-repeat prompt; if too high: extra dead air before Koe reacts to a
	// missed utterance. OVERRIDE: KOE_LOCAL_COMMIT_ACK_MS.
	defaultLocalCommitAckMS = 600
	// localCommitAckPollInterval paces the ack-wait loop; only bounds how quickly a
	// commit ack/response is noticed within the ack window.
	localCommitAckPollInterval = 10 * time.Millisecond
)

// missedSpeechInstructions steers the fallback response when the manual commit was
// NOT acked: the user's words never became a conversation item (observed live
// 2026-07-02 — semantic_vad rejects the commit with an error and the audio is
// gone), so answering from stale context would be a non-sequitur. Ask to repeat.
const missedSpeechInstructions = "The user's last spoken words were lost before they reached you (audio capture problem on this call). In the language of this conversation, briefly tell the user you could not hear what they just said and ask them to say it again. One short sentence. Do not repeat earlier answers and do not guess what they said."

func (h *eventHandler) markSpeaking() {
	if h.barged.Load() {
		// A barge-in / interrupt superseded this response; ignore its trailing audio
		// deltas so they don't re-open the playback we just stopped. The next
		// response.created clears the flag for the new turn.
		return
	}
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
		h.maybeRestoreUserMic()
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

// releaseSpeakingAfterOutputBufferWait is the missing-stop-event watchdog: after
// response.done with the output buffer still active, it releases the speaking
// gate once local playout has actually DRAINED (output level silent for the idle
// hold), with the wait+tail hard cap as backstop. Releasing on a fixed clock cut
// long result reads mid-word — audio playout routinely outlives response.done by
// 15-30s. A real output_audio_buffer.stopped (or a new response) bumps the epoch
// and this poller stands down.
func (h *eventHandler) releaseSpeakingAfterOutputBufferWait() {
	wait := time.Duration(koeEnvInt("KOE_OUTPUT_BUFFER_STOP_WAIT_MS", defaultOutputBufferStopWaitMS)) * time.Millisecond
	tail := time.Duration(koeEnvInt("KOE_SPEAKING_TAIL_MS", defaultSpeakingTailMS)) * time.Millisecond
	hold := time.Duration(koeEnvInt("KOE_PLAYBACK_IDLE_HOLD_MS", defaultPlaybackIdleHoldMS)) * time.Millisecond
	epoch := h.speakingEpoch.Add(1)
	go func() {
		deadline := time.Now().Add(wait + tail)
		ticker := time.NewTicker(playbackIdlePollInterval)
		defer ticker.Stop()
		var idleSince time.Time
		for {
			<-ticker.C
			if h.speakingEpoch.Load() != epoch {
				return // the real stop event (or a new response) took over
			}
			now := time.Now()
			if h.audio == nil || h.audio.PlaybackIdle() {
				if idleSince.IsZero() {
					idleSince = now
				}
				if now.Sub(idleSince) >= hold {
					break // playout genuinely drained — safe to release
				}
			} else {
				idleSince = time.Time{} // still audible — keep waiting
			}
			if now.After(deadline) {
				break // hard cap: never leave the mic muted on a lost stop event
			}
		}
		if h.speakingEpoch.Load() != epoch {
			return
		}
		h.outputBufferActive.Store(false)
		if h.audio != nil {
			h.audio.SetSpeaking(false)
			h.audio.SetPlaybackEnabled(false)
		}
		h.maybeRestoreUserMic()
		h.emitVoiceState(h.voiceStateAfterSpeaking())
	}()
}

// interruptOutput is the explicit interrupt (Desktop /call/interrupt): it also
// clears the input buffer, discarding any in-progress user audio.
func (h *eventHandler) interruptOutput() { h.stopOutput(false) }

// bargeInStopPlayback stops Kocoro's playback the instant the server VAD reports the
// user talking over (input_audio_buffer.speech_started) while barge-in is on. It
// keeps the input buffer: the server is mid-capture of the user's barge-in utterance,
// so clearing it would throw the interruption away.
func (h *eventHandler) bargeInStopPlayback() { h.stopOutput(true) }

// stopOutput tears down the active response's local playback and its server-side turn
// state. It frees the response slot (respBusy) itself rather than waiting for a
// response.done that a cancelled turn may never send, marks `barged` so trailing audio
// deltas can't re-open playback, and truncates the server output buffer so unheard
// audio doesn't linger in history. keepInput preserves the input buffer for barge-in
// (the user is mid-utterance); the explicit interrupt clears it.
func (h *eventHandler) stopOutput(keepInput bool) {
	hadResponse := h.respBusy.Load()
	hadOutput := h.outputBufferActive.Load()
	if h.audio != nil && h.audio.dropCapture() {
		hadOutput = true
	}
	h.speakingEpoch.Add(1)
	h.barged.Store(true)
	h.outputBufferActive.Store(false)
	h.respBusy.Store(false)
	if h.audio != nil {
		h.audio.SetSpeaking(false)
		h.audio.SetPlaybackEnabled(false)
	}
	if !keepInput {
		_ = h.sendFn(map[string]any{"type": "input_audio_buffer.clear"})
	}
	if hadResponse {
		_ = h.sendFn(map[string]any{"type": "response.cancel"})
	}
	if hadOutput {
		_ = h.sendFn(map[string]any{"type": "output_audio_buffer.clear"})
	}
	h.maybeRestoreUserMic()
	h.emitVoiceState(h.voiceStateAfterSpeaking())
}

// isSpeakingOrResponding reports whether Kocoro is currently generating or playing a
// reply — true from response.created through the local playout drain (which routinely
// outlives response.done by many seconds). The barge-in stop keys on this, not on
// respBusy alone, so talk-over during the drain tail still interrupts.
func (h *eventHandler) isSpeakingOrResponding() bool {
	if h.respBusy.Load() || h.outputBufferActive.Load() {
		return true
	}
	return h.audio != nil && h.audio.dropCapture()
}

func (h *eventHandler) observeLocalSpeechStarted() {
	seq := h.localSpeechSeq.Add(1)
	h.localStartCommitSeq.Store(h.inputCommitSeq.Load())
	h.localStartResponseSeq.Store(h.responseSeq.Load())
	if eventLogEnabled() {
		log.Printf("koe[timing]: local_speech_started seq=%d", seq)
	}
}

// taskInFlight reports whether a back-brain do_task is actually running.
// asyncTaskPending is NOT that signal: any response.created (e.g. the spoken
// "on it" ack) and injected follow-ups clear it while the task keeps running —
// live 2026-07-02 10:19:56 a mid-task fallback response hallucinated a result the
// real task delivered 18s later. CallState in-flight tracking is cleared only
// when DoTask returns.
func (h *eventHandler) taskInFlight() bool {
	return h.state != nil && h.state.InFlight() != ""
}

// maybeRestoreUserMic lifts the user's mic-off once no do_task remains in
// flight and playback is releasing (koe-mic-off design: restore hooks onto the
// playback-drain/interrupt release, never the task-zero instant, so room speech
// cannot race the result response in the ~100ms gap). Called at every
// speaking-gate release point plus the no-speech task tail — always BEFORE the
// voice_state emission, so the emitted snapshot already carries mic="on".
func (h *eventHandler) maybeRestoreUserMic() {
	if h.audio == nil || !h.audio.UserMicOff() || h.taskInFlight() {
		return
	}
	if h.audio.UserMicSticky() {
		// Manual mute (plain conversation) — only the user lifts it.
		return
	}
	h.audio.SetUserMicOff(false)
	if eventLogEnabled() {
		log.Printf("koe[mic]: user mic restored (tasks drained)")
	}
}

func (h *eventHandler) observeLocalSpeechEnded(ctx context.Context) {
	if !koeEnvBool("KOE_LOCAL_COMMIT_FALLBACK", false) {
		return
	}
	if h.asyncTaskPending.Load() || h.taskInFlight() {
		if eventLogEnabled() {
			log.Printf("koe[timing]: local_commit_fallback skipped: task pending")
		}
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
		if h.asyncTaskPending.Load() || h.taskInFlight() {
			if eventLogEnabled() {
				log.Printf("koe[timing]: local_commit_fallback skipped after delay: task pending")
			}
			return
		}
		if h.respBusy.Load() || h.outputBufferActive.Load() {
			return
		}
		if eventLogEnabled() {
			log.Printf("koe[timing]: local_commit_fallback seq=%d", seq)
		}
		// Best-effort salvage: if the server-side buffer still holds the audio, the
		// commit turns it into a real user item. Under semantic_vad the commit is
		// usually rejected (server-managed buffer, observed live 2026-07-02), so wait
		// for the ack before deciding how to respond.
		startCommitEmptySeq := h.commitEmptySeq.Load()
		_ = h.sendFn(map[string]any{"type": "input_audio_buffer.commit"})
		ackWait := time.Duration(koeEnvInt("KOE_LOCAL_COMMIT_ACK_MS", defaultLocalCommitAckMS)) * time.Millisecond
		deadline := time.Now().Add(ackWait)
		for time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(localCommitAckPollInterval):
			}
			if h.responseSeq.Load() != startResponseSeq {
				// The server started a response on its own (late natural VAD
				// recovery) — do not stack a second one.
				if eventLogEnabled() {
					log.Printf("koe[timing]: local_commit_fallback seq=%d yielded to server response", seq)
				}
				return
			}
			if h.inputCommitSeq.Load() != startCommitSeq {
				// Commit acked — the user's audio became a conversation item; a
				// plain response answers it.
				if eventLogEnabled() {
					log.Printf("koe[timing]: local_commit_fallback seq=%d commit acked", seq)
				}
				h.requestResponse()
				return
			}
			if h.commitEmptySeq.Load() != startCommitEmptySeq {
				// Rejected as EMPTY: the gate opened on a fragment (residual echo /
				// room noise), not a lost utterance — drop silently. Asking to
				// repeat here amplified every fragment into a spoken turn
				// (2026-07-09 Reachy far-field loop).
				if eventLogEnabled() {
					log.Printf("koe[timing]: local_commit_fallback seq=%d commit rejected as empty; dropping fragment", seq)
				}
				return
			}
		}
		// No ack, no response: the audio never reached the server. Answering from
		// stale context would be a non-sequitur — ask the user to repeat instead.
		if eventLogEnabled() {
			log.Printf("koe[timing]: local_commit_fallback seq=%d commit not acked; asking user to repeat", seq)
		}
		h.requestResponseWith(responseCreateRequest{instructions: missedSpeechInstructions})
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
// segmentation and starts the spoken response automatically. The default (barge-in
// off) is semantic_vad — less eager on ambient/noisy audio and more tolerant of
// backchannels while still deciding end-of-turn server-side, with server-side
// interruption OFF (half-duplex, the mic is muted while Kocoro speaks).
// Barge-in ON inverts both: it defaults to server_vad (reacts to talk-over more
// directly) with interrupt_response ON and a higher VAD threshold (0.60) to resist
// residual speaker echo self-interrupting. KOE_TURN_DETECTION / KOE_VAD_THRESHOLD /
// KOE_INTERRUPT_RESPONSE override either mode. Far-field noise reduction is on by
// default for the laptop speaker/mic case; set KOE_NOISE_REDUCTION=off for raw input.
func sessionConfig(persona, voice string, fullDuplexAEC bool) map[string]any {
	vadSilenceMS := koeEnvInt("KOE_VAD_SILENCE_MS", 900)
	interruptResponse := false
	if fullDuplexAEC {
		interruptResponse = koeEnvBool("KOE_INTERRUPT_RESPONSE", false)
	}
	// Barge-in (interruptResponse) forwards the mic continuously during playback and
	// leans on the server VAD to detect talk-over. Default to server_vad there — it
	// reacts to the user speaking over Kocoro more directly than semantic_vad's
	// "wait for a complete thought" — and raise the detection threshold so residual
	// speaker echo on the uplink is less likely to self-interrupt (headphones need it
	// less, speakers more). Barge-in off keeps the low-eagerness semantic_vad. Both
	// stay env-overridable (KOE_TURN_DETECTION / KOE_VAD_THRESHOLD).
	defaultTurn := "semantic_vad"
	defaultThreshold := 0.50
	if interruptResponse {
		defaultTurn = "server_vad"
		defaultThreshold = 0.60
	}
	vadThreshold := koeEnvFloat("KOE_VAD_THRESHOLD", defaultThreshold)
	turnMode := koeEnvString("KOE_TURN_DETECTION", defaultTurn)
	log.Printf("koe[barge]: sessionConfig fullDuplexAEC=%v interrupt_response=%v turn=%s threshold=%.2f", fullDuplexAEC, interruptResponse, turnMode, vadThreshold)
	var turnDetection map[string]any
	if strings.EqualFold(turnMode, "semantic_vad") {
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
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
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
		// Barge-in off (default): local capture is muted while Kocoro speaks, so this
		// fires only between turns. Barge-in on: the mic stays live during playback, so
		// this is the talk-over signal — stop Kocoro's buffered speech immediately so
		// the interruption is instant (interrupt_response=true cancels the response
		// server-side in parallel).
		if koeEnvBool("KOE_VPIO_BARGE_IN", false) && h.isSpeakingOrResponding() {
			log.Printf("koe[barge]: talk-over detected — stopping playback")
			h.bargeInStopPlayback()
		}
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
		// New turn — clear any barge/interrupt suppression so this response's audio
		// deltas re-open playback normally (markSpeaking is otherwise a no-op while
		// barged is set).
		h.barged.Store(false)
		// A response is now generating — the serialized sender waits for its
		// response.done before sending the next response.create. Gate capture
		// immediately, not only once output_audio_buffer.started arrives: otherwise
		// slow tail audio / room noise in the response-created→first-audio gap can
		// become the next user turn.
		//
		// Bump speakingEpoch like the other gate-set sites (markSpeaking /
		// interruptOutput): re-gating without it lets a pending release tail from the
		// PRIOR turn (releaseSpeakingTail / releaseSpeakingAfterOutputBufferWait, which
		// captured the old epoch) fire mid-new-response, clipping the reply start and
		// re-opening the mic. Only the epoch is bumped, not markSpeaking(): there is no
		// audio yet, so the voice_state must stay "thinking", not flip to "speaking".
		h.speakingEpoch.Add(1)
		h.respBusy.Store(true)
		if h.audio != nil {
			h.audio.SetPlaybackEnabled(true)
			h.audio.SetSpeaking(true)
		}
		signalNonBlocking(h.respCreated) // ack the sender's pending response.create
	case "error":
		// Always log the payload, not just the type: the 2026-07-02 mid-call VAD
		// failures were undiagnosable from "koe[event]: error" alone (commit
		// rejection reason invisible). Errors are rare, so this is not gated on
		// KOE_EVENT_LOG.
		log.Printf("koe[error]: server error code=%q type=%q message=%q", ev.Error.Code, ev.Error.Type, ev.Error.Message)
		// GA rejects a response.create sent while a response is active. Signal the
		// sender to retry instead of silently losing the turn (the exact code
		// kocoro-reachy matches: conversation_already_has_active_response).
		if ev.Error.Code == "conversation_already_has_active_response" {
			signalNonBlocking(h.respRejected)
		}
		// An empty-buffer commit rejection means the manual fallback commit found
		// (nearly) no audio server-side — signal the fallback's ack wait so it
		// classifies the turn as a fragment instead of a missed utterance.
		if ev.Error.Code == "input_audio_buffer_commit_empty" {
			h.commitEmptySeq.Add(1)
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
	// Deterministic dismiss backstop: a whole-utterance control phrase (闭嘴/停/够了/
	// 退出/再见/bye/…) hangs up regardless of whether the model also calls end_call —
	// gpt-realtime-mini is unreliable at that tool (1/7 live), so the fixed vocabulary
	// cannot depend on it. onEndCall (Desktop endCall / standalone cancel) is idempotent,
	// so a racing tool call is harmless. Runs regardless of KOE_TRANSCRIPT_LOG.
	if h.onEndCall != nil && isDismissPhrase(transcript) {
		if h.taskInFlight() && isTaskAmbiguousDismissPhrase(transcript) {
			if eventLogEnabled() {
				log.Printf("koe[call]: dismiss phrase %q left to model while task is running", transcript)
			}
			return
		}
		if eventLogEnabled() {
			log.Printf("koe[call]: dismiss phrase %q — hanging up", transcript)
		}
		// Cut any in-progress auto-response audio IMMEDIATELY (create_response:true may
		// have already started voicing a reply to the dismiss utterance) so nothing
		// leaks before the goodbye earcon. interruptOutput drops local playback + sends
		// response.cancel/output_audio_buffer.clear; the teardown then hangs up.
		h.interruptOutput()
		go h.onEndCall()
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

// shouldVoiceDoTaskResult decides whether a completed do_task result should be
// spoken aloud. The function_call_output is ALWAYS submitted (protocol + it stays
// in context / on Kocoro Desktop); this only gates the response.create that voices
// it — the sole thing that makes Kocoro open its mouth, since an out-of-band tool
// result never triggers the server's auto-response.
//
// Suppressed when the user moved on to plain conversation after the most recent
// do_task (userSpokeSinceLastDoTask): a correction, a topic change ("你弄错了"), or a
// verbal question asked while a long task ran — voicing the now-stale result would
// barge into a conversation that already moved on. A follow-up that refines the task
// dispatches its own do_task, which advances the last-do_task marker, so it does NOT
// count as moving on and the combined result still voices. Injected/empty outcomes
// never voice (the owning run does).
func shouldVoiceDoTaskResult(r SayResult, userSpokeSinceLastDoTask bool) bool {
	if r.Status == "injected" || r.Say == "" {
		return false
	}
	return !userSpokeSinceLastDoTask
}

// handleFunctionCall composes do_task synchronously (C-minimal) or routes the
// fast tools through Dispatch, then sends the function_call_output back.
func (h *eventHandler) handleFunctionCall(ctx context.Context, callID, name string, args []byte) {
	if name == "do_task" {
		// Resolve the mechanical-fallback language once for this call: the pinned koe
		// language wins, else the utterance decides (the task text is a JSON string
		// inside args, so a Han rune anywhere in args signals a Chinese utterance —
		// the JSON keys and agent slugs are ASCII).
		lang := fallbackLang(h.language, string(args))
		req, clarify, err := h.disp.PrepareDoTask(args, lang)
		if err != nil {
			if eventLogEnabled() {
				log.Printf("koe[task]: prepare failed call_id=%q err=%v args=%s", callID, err, logMaybeBytes(args, 500))
			}
			say := fallbackSay(lang, "misheard")
			h.sendOutput(callID, SayResult{Status: "failed", SpokenSummary: say, Say: say})
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
		// Snapshot the commit count at THIS (now the most recent) do_task dispatch. A
		// completed result compares the live inputCommitSeq against this at land-time to
		// tell "user moved on to conversation" from "user is still waiting / a follow-up
		// refined the task" (shouldVoiceDoTaskResult). handleEvent is single-goroutine,
		// so this Store races with no other writer.
		h.lastDoTaskCommitSeq.Store(h.inputCommitSeq.Load())
		h.state.SetInFlightForAgent(req.Text, req.Agent)
		h.asyncTaskPending.Store(true)
		h.emitVoiceState("thinking") // delegating; the model's call-turn ack already played
		go func() {
			// do_task must survive session teardown: a hangup cancels the session ctx
			// that Connect rides, which would abort this in-flight POST /message and
			// kill the back-brain run mid-turn — contradicting do_task's contract that
			// the report persists in the session / Kocoro Desktop. So the delegation
			// call + its result tail ride a cancel-detached copy. The goroutine's only
			// remaining lifetime bound is the daemon responding, guaranteed by the
			// daemon's idle watchdog (matches doTaskClient's Timeout:0, daemon-controlled).
			// The tail no-ops gracefully when the call is already gone: sendFn errors are
			// ignored, requestResponse* drops non-blockingly, onVoiceState is isActive-gated.
			taskCtx := context.WithoutCancel(ctx)
			started := time.Now()
			if eventLogEnabled() {
				log.Printf("koe[task]: start call_id=%q agent=%q burst=%q task=%s", callID, req.Agent, req.ThreadID, logMaybeText(req.Text, 500))
			}
			out, derr := h.disp.client.DoTask(taskCtx, req)
			h.state.ClearInFlightForAgent(req.Agent)
			r := MapDoTaskOutcome(out, derr, lang)
			if eventLogEnabled() {
				log.Printf("koe[task]: done call_id=%q kind=%s status=%s session=%q partial=%t failure=%q reason=%q spoken_len=%d reply_len=%d duration_ms=%d err=%v",
					callID, outcomeKindLog(out.Kind), r.Status, out.SessionID, out.Partial, out.FailureCode, out.Reason,
					len([]rune(r.SpokenSummary)), len([]rune(out.Reply)), time.Since(started).Milliseconds(), derr)
			}
			b, _ := json.Marshal(r)
			h.sendFunctionOutput(callID, b) // satisfy the protocol for this call_id
			// The result is always in the conversation now; only decide whether to VOICE
			// it. Suppress when the user has moved on to conversation since the most
			// recent do_task (correction / topic change / a verbal question during a long
			// task); a follow-up that refined the task advances lastDoTaskCommitSeq so it
			// still voices. Overridable via KOE_SUPPRESS_STALE_RESULT=0 (rollback).
			userSpokeSinceLastDoTask := h.inputCommitSeq.Load() > h.lastDoTaskCommitSeq.Load()
			voice := shouldVoiceDoTaskResult(r, userSpokeSinceLastDoTask)
			suppressedAsStale := !voice && userSpokeSinceLastDoTask && r.Status != "injected" && r.Say != ""
			if suppressedAsStale && !koeEnvBool("KOE_SUPPRESS_STALE_RESULT", true) {
				voice = true // rollback switch: restore the old always-voice behavior
				suppressedAsStale = false
			}
			if eventLogEnabled() {
				log.Printf("koe[tool]: output call_id=%q status=%s voice=%t userSpokeSince=%t output=%s",
					callID, r.Status, voice, userSpokeSinceLastDoTask, logMaybeBytes(b, 500))
			}
			if voice {
				h.requestResponseForSpeech(r.Say) // voice the result (skip when the daemon already replied)
			} else {
				if suppressedAsStale {
					log.Printf("koe[task]: stale result NOT voiced — user took the floor mid-task, call_id=%q", callID)
				}
				h.asyncTaskPending.Store(false)
				h.maybeRestoreUserMic()
				h.emitVoiceState("listening")
			}
		}()
		return
	}
	if name == "end_call" {
		// Dismiss / hang up. Do NOT send a function_call_output or a spoken reply: the
		// teardown closes the session, and the goodbye earcon (played inside onEndCall)
		// is the only feedback. Run in a goroutine — onEndCall closes THIS connection,
		// and the event loop calling it must not block on its own teardown.
		if eventLogEnabled() {
			log.Printf("koe[call]: end_call requested call_id=%q", callID)
		}
		if h.onEndCall != nil {
			h.interruptOutput()
			go h.onEndCall()
		}
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
