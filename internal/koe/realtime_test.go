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

// countType counts captured frames whose "type" equals typ.
func (c *captureSender) countType(typ string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, m := range c.sent {
		if m["type"] == typ {
			n++
		}
	}
	return n
}

func (c *captureSender) types() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.sent))
	for _, m := range c.sent {
		typ, _ := m["type"].(string)
		out = append(out, typ)
	}
	return out
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

func (c *captureSender) responseCreateInstructions() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, m := range c.sent {
		if m["type"] != "response.create" {
			continue
		}
		resp, ok := m["response"].(map[string]any)
		if !ok {
			out = append(out, "")
			continue
		}
		instr, _ := resp["instructions"].(string)
		out = append(out, instr)
	}
	return out
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

	// reachy say-and-ask: NO synchronous placeholder ack — the model spoke its own
	// ack in the call turn. The single function_call_output carrying the REAL result
	// is sent after the back-brain turn completes.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cap.sentContains("Reminder added.") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("do_task result function_call_output never sent")
}

func TestHandleFunctionCallInjectedFollowupDoesNotDoubleSpeak(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var mu sync.Mutex
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		n := requests
		mu.Unlock()
		switch n {
		case 1:
			close(firstStarted)
			<-releaseFirst
			_ = json.NewEncoder(w).Encode(map[string]any{"reply": "Final combined result.", "agent": "default"})
		case 2:
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "injected", "route": "default:koe:burst-x"})
		default:
			t.Errorf("unexpected do_task request #%d", n)
			w.WriteHeader(http.StatusTooManyRequests)
		}
	}))
	defer srv.Close()

	state := NewCallState("burst-x", "")
	disp := NewDispatcher(NewDaemonClient(srv.URL), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	cap := &captureSender{}
	h := newEventHandler(disp, state, nil, cap.send)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.runResponseSender(ctx)

	h.handleFunctionCall(ctx, "call-1", "do_task", []byte(`{"task":"add a reminder"}`))
	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first do_task did not start")
	}

	h.handleFunctionCall(ctx, "call-2", "do_task", []byte(`{"task":"change it to 6pm"}`))
	waitUntil(t, func() bool { return cap.sentContains("injected") }, "injected follow-up did not get function_call_output")
	time.Sleep(150 * time.Millisecond)
	if got := cap.countType("response.create"); got != 0 {
		t.Fatalf("injected follow-up must not request a voiced response, got %d response.create", got)
	}
	if got := state.InFlight(); got == "" {
		t.Fatal("injected follow-up cleared in-flight state while the original do_task was still running")
	}

	close(releaseFirst)
	waitUntil(t, func() bool { return cap.sentContains("Final combined result.") }, "final do_task result was not sent")
	waitUntil(t, func() bool { return cap.countType("response.create") >= 1 }, "final do_task result did not request voice")
	if got := cap.countType("response.create"); got != 1 {
		t.Fatalf("final result should request exactly one voiced response, got %d", got)
	}
	instr := cap.responseCreateInstructions()
	if len(instr) != 1 || !strings.Contains(instr[0], "Say exactly the text between <spoken_summary>") ||
		!strings.Contains(instr[0], "Final combined result.") {
		t.Fatalf("final result response.create must pin exact spoken_summary instructions, got %#v", instr)
	}
}

func TestHandleEventFunctionCallArgumentsDoneDelegatesDoTask(t *testing.T) {
	gotReq := make(chan DoTaskRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req DoTaskRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		gotReq <- req
		_ = json.NewEncoder(w).Encode(map[string]any{"reply": "Checked Gmail.", "spoken_summary": "You have three new emails.", "agent": "default"})
	}))
	defer srv.Close()

	state := NewCallState("burst-x", "")
	disp := NewDispatcher(NewDaemonClient(srv.URL), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	cap := &captureSender{}
	h := newEventHandler(disp, state, nil, cap.send)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.runResponseSender(ctx)

	h.handleEvent(ctx, []byte(`{"type":"response.function_call_arguments.done","name":"do_task","call_id":"call-1","arguments":"{\"task\":\"check my Gmail inbox\"}"}`))

	select {
	case req := <-gotReq:
		if req.Source != "koe" {
			t.Fatalf("DoTask Source = %q, want koe", req.Source)
		}
		if req.Text != "check my Gmail inbox" {
			t.Fatalf("DoTask Text = %q", req.Text)
		}
		if req.ThreadID != "burst-x" {
			t.Fatalf("DoTask ThreadID = %q, want burst-x", req.ThreadID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Realtime function_call_arguments.done did not reach daemon DoTask")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cap.sentContains("You have three new emails.") {
			instr := cap.responseCreateInstructions()
			if len(instr) != 1 || !strings.Contains(instr[0], "You have three new emails.") ||
				!strings.Contains(instr[0], "Do not add a greeting, preface, follow-up question") {
				t.Fatalf("do_task response.create must constrain speech to spoken_summary, got %#v", instr)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("do_task spoken_summary was not sent as function_call_output")
}

// TestHandleEventGatesMicWhileSpeaking locks the half-duplex gate into the event
// loop: a structurally-correct gate (C2) is inert unless handleEvent actually
// toggles it. This also pins the exact OpenAI event names — a rename would make
// the gate silently never fire, which this test would catch.
func TestHandleEventGatesMicWhileSpeaking(t *testing.T) {
	t.Setenv("KOE_SPEAKING_TAIL_MS", "1")
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
	waitUntil(t, func() bool { return !audio.dropCapture() }, "response.done did not ungate the mic")
	if audio.dropCapture() {
		t.Error("response.done must ungate the mic (SetSpeaking false)")
	}
}

func TestHandleEventGatesMicAsSoonAsResponseStarts(t *testing.T) {
	t.Setenv("KOE_SPEAKING_TAIL_MS", "1")
	audio, err := NewAudioIO()
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	state := NewCallState("burst-x", "")
	disp := NewDispatcher(NewDaemonClient(""), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	h := newEventHandler(disp, state, audio, func(any) error { return nil })

	h.handleEvent(context.Background(), []byte(`{"type":"response.created"}`))
	if !audio.dropCapture() {
		t.Fatal("response.created must gate capture before the first output audio marker")
	}
	h.handleEvent(context.Background(), []byte(`{"type":"response.done"}`))
	waitUntil(t, func() bool { return !audio.dropCapture() }, "response.done did not ungate response-created capture gate")
}

func TestHandleEventDoesNotUngateBeforeOutputBufferStops(t *testing.T) {
	t.Setenv("KOE_SPEAKING_TAIL_MS", "1")
	t.Setenv("KOE_OUTPUT_BUFFER_STOP_WAIT_MS", "200")
	audio, err := NewAudioIO()
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	state := NewCallState("burst-x", "")
	disp := NewDispatcher(NewDaemonClient(""), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	h := newEventHandler(disp, state, audio, func(any) error { return nil })

	h.handleEvent(context.Background(), []byte(`{"type":"output_audio_buffer.started"}`))
	if !audio.dropCapture() {
		t.Fatal("output_audio_buffer.started must gate capture")
	}
	h.handleEvent(context.Background(), []byte(`{"type":"response.done"}`))
	time.Sleep(30 * time.Millisecond)
	if !audio.dropCapture() {
		t.Fatal("response.done must not ungate while output_audio_buffer is still active")
	}
	h.handleEvent(context.Background(), []byte(`{"type":"output_audio_buffer.stopped"}`))
	waitUntil(t, func() bool { return !audio.dropCapture() }, "output_audio_buffer.stopped did not release the speaking gate")
}

func TestInterruptOutputStopsPlaybackAndClearsRealtimeBuffers(t *testing.T) {
	audio, err := NewAudioIO()
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	state := NewCallState("burst-x", "")
	disp := NewDispatcher(NewDaemonClient(""), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	cap := &captureSender{}
	h := newEventHandler(disp, state, audio, cap.send)
	audio.SetPlaybackEnabled(true)
	audio.SetSpeaking(true)
	audio.Play(make([]int16, audioFrameSize))
	h.respBusy.Store(true)
	h.outputBufferActive.Store(true)

	h.interruptOutput()

	if audio.dropCapture() {
		t.Fatal("interruptOutput must reopen local capture immediately")
	}
	if got := len(audio.playBuf); got != 0 {
		t.Fatalf("interruptOutput must drain local playback queue, got %d frame(s)", got)
	}
	if h.respBusy.Load() || h.outputBufferActive.Load() {
		t.Fatal("interruptOutput must clear local response/output state")
	}
	want := []string{"input_audio_buffer.clear", "response.cancel", "output_audio_buffer.clear"}
	if got := cap.types(); !equalStringSlices(got, want) {
		t.Fatalf("sent event types = %v, want %v", got, want)
	}
}

func TestInterruptOutputWhenIdleOnlyClearsInput(t *testing.T) {
	state := NewCallState("burst-x", "")
	disp := NewDispatcher(NewDaemonClient(""), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	cap := &captureSender{}
	h := newEventHandler(disp, state, nil, cap.send)

	h.interruptOutput()

	want := []string{"input_audio_buffer.clear"}
	if got := cap.types(); !equalStringSlices(got, want) {
		t.Fatalf("sent event types = %v, want %v", got, want)
	}
}

func TestHandleEventKeepsThinkingWhileAsyncTaskPending(t *testing.T) {
	t.Setenv("KOE_SPEAKING_TAIL_MS", "1")
	t.Setenv("KOE_OUTPUT_BUFFER_STOP_WAIT_MS", "1")
	state := NewCallState("burst-x", "")
	disp := NewDispatcher(NewDaemonClient(""), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	h := newEventHandler(disp, state, nil, func(any) error { return nil })

	var mu sync.Mutex
	var states []string
	h.onVoiceState = func(s string) {
		mu.Lock()
		defer mu.Unlock()
		states = append(states, s)
	}
	lastState := func() string {
		mu.Lock()
		defer mu.Unlock()
		if len(states) == 0 {
			return ""
		}
		return states[len(states)-1]
	}

	h.asyncTaskPending.Store(true)
	h.handleEvent(context.Background(), []byte(`{"type":"output_audio_buffer.started"}`))
	h.handleEvent(context.Background(), []byte(`{"type":"response.done"}`))
	waitUntil(t, func() bool { return lastState() == "thinking" }, "pending do_task should keep voice state thinking after output release")

	h.handleEvent(context.Background(), []byte(`{"type":"response.created"}`))
	if h.asyncTaskPending.Load() {
		t.Fatal("result response.created should clear async task pending")
	}
}

func TestHandleEventReleasesWhenOutputBufferStopIsLate(t *testing.T) {
	t.Setenv("KOE_SPEAKING_TAIL_MS", "1")
	t.Setenv("KOE_OUTPUT_BUFFER_STOP_WAIT_MS", "10")
	audio, err := NewAudioIO()
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	state := NewCallState("burst-x", "")
	disp := NewDispatcher(NewDaemonClient(""), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	h := newEventHandler(disp, state, audio, func(any) error { return nil })

	h.handleEvent(context.Background(), []byte(`{"type":"output_audio_buffer.started"}`))
	h.handleEvent(context.Background(), []byte(`{"type":"response.done"}`))
	waitUntil(t, func() bool { return !audio.dropCapture() }, "late output_audio_buffer.stopped left the mic gated")

	h.handleEvent(context.Background(), []byte(`{"type":"output_audio_buffer.stopped"}`))
	if audio.dropCapture() {
		t.Fatal("stale output_audio_buffer.stopped must not re-gate capture")
	}
}

func TestHandleEventKeepsMicGatedUntilLateOutputBufferStop(t *testing.T) {
	t.Setenv("KOE_SPEAKING_TAIL_MS", "1")
	t.Setenv("KOE_OUTPUT_BUFFER_STOP_WAIT_MS", "200")
	audio, err := NewAudioIO()
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	state := NewCallState("burst-x", "")
	disp := NewDispatcher(NewDaemonClient(""), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	h := newEventHandler(disp, state, audio, func(any) error { return nil })

	h.handleEvent(context.Background(), []byte(`{"type":"output_audio_buffer.started"}`))
	h.handleEvent(context.Background(), []byte(`{"type":"response.done"}`))
	time.Sleep(50 * time.Millisecond)
	if !audio.dropCapture() {
		t.Fatal("response.done must not release the mic while output buffer is still active")
	}

	h.handleEvent(context.Background(), []byte(`{"type":"output_audio_buffer.stopped"}`))
	waitUntil(t, func() bool { return !audio.dropCapture() }, "output_audio_buffer.stopped did not release the mic")
}

func TestHandleEventIgnoresStaleOutputBufferStopAfterLocalRelease(t *testing.T) {
	t.Setenv("KOE_SPEAKING_TAIL_MS", "1")
	t.Setenv("KOE_OUTPUT_BUFFER_STOP_WAIT_MS", "1")
	audio, err := NewAudioIO()
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	state := NewCallState("burst-x", "")
	disp := NewDispatcher(NewDaemonClient(""), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	h := newEventHandler(disp, state, audio, func(any) error { return nil })

	h.handleEvent(context.Background(), []byte(`{"type":"output_audio_buffer.started"}`))
	h.handleEvent(context.Background(), []byte(`{"type":"response.done"}`))
	waitUntil(t, func() bool { return !audio.dropCapture() }, "first response did not release")

	h.handleEvent(context.Background(), []byte(`{"type":"response.created"}`))
	h.handleEvent(context.Background(), []byte(`{"type":"output_audio_buffer.stopped"}`))
	time.Sleep(20 * time.Millisecond)
	if !audio.dropCapture() {
		t.Fatal("stale output_audio_buffer.stopped must not ungate a new response-created gate")
	}
}

func TestHandleEventMarksSpeakingWithFullDuplexAEC(t *testing.T) {
	t.Setenv("KOE_SPEAKING_TAIL_MS", "1")
	audio, err := NewAudioIO()
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	state := NewCallState("burst-x", "")
	disp := NewDispatcher(NewDaemonClient(""), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	h := newEventHandler(disp, state, audio, func(any) error { return nil })
	h.fullDuplexAEC = true

	h.handleEvent(context.Background(), []byte(`{"type":"response.output_audio.delta"}`))
	if !audio.dropCapture() {
		t.Error("VPIO/full-duplex mode must mark speaking so the local barge-in guard can suppress echo")
	}
	h.handleEvent(context.Background(), []byte(`{"type":"response.done"}`))
	waitUntil(t, func() bool { return !audio.dropCapture() }, "response.done did not clear the VPIO barge-in guard")
	if audio.dropCapture() {
		t.Error("response.done must clear the VPIO barge-in guard")
	}
}

func TestSessionConfigUsesSemanticVADByDefault(t *testing.T) {
	cfg := sessionConfig("persona", "marin", false)
	raw, _ := json.Marshal(cfg)
	s := string(raw)

	for _, want := range []string{
		`"transcription":{"model":"gpt-4o-mini-transcribe"}`,
		`"turn_detection"`,
		`"type":"semantic_vad"`,
		`"eagerness":"low"`,
		`"create_response":true`,
		`"interrupt_response":false`,
		`"noise_reduction":{"type":"far_field"}`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("sessionConfig missing %s in %s", want, s)
		}
	}
	if strings.Contains(s, `"create_response":false`) {
		t.Fatalf("sessionConfig must not gate responses (create_response must be true): %s", s)
	}
}

func TestSessionConfigCanUseServerVAD(t *testing.T) {
	t.Setenv("KOE_TURN_DETECTION", "server_vad")
	cfg := sessionConfig("persona", "marin", true)
	raw, _ := json.Marshal(cfg)
	s := string(raw)

	for _, want := range []string{
		`"type":"server_vad"`,
		`"threshold":0.5`,
		`"silence_duration_ms":900`,
		`"create_response":true`,
		`"interrupt_response":false`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("sessionConfig missing %s in %s", want, s)
		}
	}
}

func TestSessionConfigKeepsInterruptDisabledForVPIOByDefault(t *testing.T) {
	cfg := sessionConfig("persona", "marin", true)
	raw, _ := json.Marshal(cfg)
	s := string(raw)

	for _, want := range []string{
		`"create_response":true`,
		`"interrupt_response":false`,
		`"type":"semantic_vad"`,
		`"eagerness":"low"`,
		`"noise_reduction":{"type":"far_field"}`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("sessionConfig missing %s in %s", want, s)
		}
	}
}

func TestSessionConfigCanEnableInterruptForBargeInExperiment(t *testing.T) {
	t.Setenv("KOE_INTERRUPT_RESPONSE", "1")
	cfg := sessionConfig("persona", "marin", true)
	raw, _ := json.Marshal(cfg)
	if !strings.Contains(string(raw), `"interrupt_response":true`) {
		t.Fatalf("KOE_INTERRUPT_RESPONSE=1 should enable interruption for VPIO experiments: %s", raw)
	}
}

func TestSessionConfigCanDisableNoiseReduction(t *testing.T) {
	t.Setenv("KOE_NOISE_REDUCTION", "off")
	cfg := sessionConfig("persona", "marin", true)
	raw, _ := json.Marshal(cfg)
	if strings.Contains(string(raw), `"noise_reduction"`) {
		t.Fatalf("KOE_NOISE_REDUCTION=off should remove noise_reduction: %s", raw)
	}
}

func TestTranscriptCompletedDoesNotCreateResponse(t *testing.T) {
	state := NewCallState("burst-x", "")
	disp := NewDispatcher(NewDaemonClient(""), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	cap := &captureSender{}
	h := newEventHandler(disp, state, nil, cap.send)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.runResponseSender(ctx)
	// Under create_response:true the SERVER auto-creates the response; the transcript
	// handler is diagnostic only and must NOT also fire response.create (double-reply).
	h.handleEvent(ctx, []byte(`{"type":"conversation.item.input_audio_transcription.completed","transcript":"帮我查一下明天的天气"}`))
	time.Sleep(150 * time.Millisecond) // the sender would have flushed by now if anything were queued
	if cap.sentContains("response.create") {
		t.Fatal("transcript.completed must not create a response under create_response:true")
	}
}

func TestLocalCommitFallbackCommitsWhenServerVADMisses(t *testing.T) {
	t.Setenv("KOE_LOCAL_COMMIT_FALLBACK_MS", "1")
	state := NewCallState("burst-x", "")
	disp := NewDispatcher(NewDaemonClient(""), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	cap := &captureSender{}
	h := newEventHandler(disp, state, nil, cap.send)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.runResponseSender(ctx)

	h.observeLocalSpeechStarted()
	h.observeLocalSpeechEnded(ctx)

	waitUntil(t, func() bool { return cap.countType("input_audio_buffer.commit") == 1 }, "local fallback did not commit input audio")
	waitUntil(t, func() bool { return cap.countType("response.create") == 1 }, "local fallback did not request a response")
}

func TestLocalCommitFallbackSkipsWhenServerAlreadyCommitted(t *testing.T) {
	t.Setenv("KOE_LOCAL_COMMIT_FALLBACK_MS", "1")
	state := NewCallState("burst-x", "")
	disp := NewDispatcher(NewDaemonClient(""), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	cap := &captureSender{}
	h := newEventHandler(disp, state, nil, cap.send)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.runResponseSender(ctx)

	h.observeLocalSpeechStarted()
	h.handleEvent(ctx, []byte(`{"type":"input_audio_buffer.committed"}`))
	h.observeLocalSpeechEnded(ctx)

	time.Sleep(50 * time.Millisecond)
	if got := cap.countType("input_audio_buffer.commit"); got != 0 {
		t.Fatalf("server-committed speech must not be committed again, got %d commits", got)
	}
	if got := cap.countType("response.create"); got != 0 {
		t.Fatalf("server-committed speech must not request a duplicate response, got %d creates", got)
	}
}

func TestLocalCommitFallbackSkipsWhenServerAlreadyResponded(t *testing.T) {
	t.Setenv("KOE_LOCAL_COMMIT_FALLBACK_MS", "1")
	state := NewCallState("burst-x", "")
	disp := NewDispatcher(NewDaemonClient(""), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	cap := &captureSender{}
	h := newEventHandler(disp, state, nil, cap.send)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.runResponseSender(ctx)

	h.observeLocalSpeechStarted()
	h.handleEvent(ctx, []byte(`{"type":"response.created"}`))
	h.observeLocalSpeechEnded(ctx)

	time.Sleep(50 * time.Millisecond)
	if got := cap.countType("input_audio_buffer.commit"); got != 0 {
		t.Fatalf("server-responded speech must not be committed again, got %d commits", got)
	}
	if got := cap.countType("response.create"); got != 0 {
		t.Fatalf("server-responded speech must not request a duplicate response, got %d creates", got)
	}
}

func TestLocalCommitFallbackSkipsWhileTaskPending(t *testing.T) {
	t.Setenv("KOE_LOCAL_COMMIT_FALLBACK_MS", "1")
	state := NewCallState("burst-x", "")
	disp := NewDispatcher(NewDaemonClient(""), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	cap := &captureSender{}
	h := newEventHandler(disp, state, nil, cap.send)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.runResponseSender(ctx)

	h.asyncTaskPending.Store(true)
	h.observeLocalSpeechStarted()
	h.observeLocalSpeechEnded(ctx)

	time.Sleep(50 * time.Millisecond)
	if got := cap.countType("input_audio_buffer.commit"); got != 0 {
		t.Fatalf("pending do_task must not be committed over by local fallback, got %d commits", got)
	}
	if got := cap.countType("response.create"); got != 0 {
		t.Fatalf("pending do_task must not get a premature fallback response, got %d creates", got)
	}
}

// TestResponseSenderRetriesOnActiveResponseRejection pins the core robustness of the
// serialized sender: when GA rejects a response.create with
// conversation_already_has_active_response, the sender retries instead of silently
// dropping the turn.
func TestResponseSenderRetriesOnActiveResponseRejection(t *testing.T) {
	state := NewCallState("burst-x", "")
	disp := NewDispatcher(NewDaemonClient(""), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	cap := &captureSender{}
	h := newEventHandler(disp, state, nil, cap.send)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.runResponseSender(ctx)

	waitUntil := func(cond func() bool, msg string) {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal(msg)
	}

	h.requestResponse()
	waitUntil(func() bool { return cap.countType("response.create") >= 1 }, "first response.create never sent")
	if instr := cap.responseCreateInstructions(); len(instr) != 1 || instr[0] != "" {
		t.Fatalf("plain requestResponse must not add per-response instructions, got %#v", instr)
	}

	// Reject it → the sender must retry with a second response.create.
	h.handleEvent(ctx, []byte(`{"type":"error","error":{"code":"conversation_already_has_active_response"}}`))
	waitUntil(func() bool { return cap.countType("response.create") >= 2 }, "rejection did not trigger a retry")

	// Accept the retry; no further creates after that.
	h.handleEvent(ctx, []byte(`{"type":"response.created"}`))
	time.Sleep(200 * time.Millisecond)
	if n := cap.countType("response.create"); n != 2 {
		t.Errorf("expected exactly 2 response.create (1 + 1 retry), got %d", n)
	}
}

// TestHandleEventVoiceStateSequence pins the precise state machine (D1w): the
// WebRTC output_audio_buffer.started/stopped markers drive SPEAKING/IDLE, and
// input_audio_buffer.speech_started surfaces the reactive listening moment. A
// rename of any of these GA event names would silently break the Island sprite —
// this test catches it.
func TestHandleEventVoiceStateSequence(t *testing.T) {
	t.Setenv("KOE_SPEAKING_TAIL_MS", "1")
	audio, _ := NewAudioIO()
	state := NewCallState("burst-seq", "")
	disp := NewDispatcher(NewDaemonClient(""), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	h := newEventHandler(disp, state, audio, func(any) error { return nil })
	var statesMu sync.Mutex
	var states []string
	h.onVoiceState = func(s string) {
		statesMu.Lock()
		defer statesMu.Unlock()
		states = append(states, s)
	}

	for _, e := range []string{
		`{"type":"input_audio_buffer.speech_started"}`, // user talking → listening
		`{"type":"response.created"}`,                  // thinking (no voice_state)
		`{"type":"output_audio_buffer.started"}`,       // reply audio begins → speaking
		`{"type":"output_audio_buffer.stopped"}`,       // reply drained → listening
		`{"type":"response.done"}`,                     // turn done → listening
	} {
		h.handleEvent(context.Background(), []byte(e))
	}
	waitUntil(t, func() bool {
		statesMu.Lock()
		defer statesMu.Unlock()
		return len(states) >= 3
	}, "voice state tail release did not fire")
	statesMu.Lock()
	gotStates := append([]string(nil), states...)
	statesMu.Unlock()
	want := []string{"listening", "speaking", "listening"}
	if len(gotStates) != len(want) {
		t.Fatalf("voice states = %v, want %v", gotStates, want)
	}
	for i := range want {
		if gotStates[i] != want[i] {
			t.Fatalf("voice state[%d] = %q, want %q (full: %v)", i, gotStates[i], want[i], gotStates)
		}
	}

	// The precise WebRTC markers must also drive the mic gate.
	h2 := newEventHandler(disp, state, audio, func(any) error { return nil })
	h2.handleEvent(context.Background(), []byte(`{"type":"output_audio_buffer.started"}`))
	if !audio.dropCapture() {
		t.Error("output_audio_buffer.started must gate the mic")
	}
	h2.handleEvent(context.Background(), []byte(`{"type":"output_audio_buffer.stopped"}`))
	waitUntil(t, func() bool { return !audio.dropCapture() }, "output_audio_buffer.stopped did not ungate the mic")
	if audio.dropCapture() {
		t.Error("output_audio_buffer.stopped must ungate the mic")
	}
}

func waitUntil(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
