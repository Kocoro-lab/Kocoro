package koe

// Headless voice E2E for C-minimal. Instead of a live mic + ears, it speaks a
// pre-recorded WAV through the real koe send path (Opus → pion track → OpenAI
// Realtime) and asserts the back-brain (a mock daemon) receives the do_task the
// model derived from the spoken words. This proves the KOE SIDE end-to-end:
// mint → WebRTC → Opus encode → OpenAI ASR → function-call routing → do_task
// dispatch. It does NOT exercise the real daemon agent loop (mock) nor OpenAI
// speaking the answer aloud (the one irreducibly-audible bit that stays manual).
//
// Gated: needs KOE_E2E=1 + a valid OPENAI_API_KEY (C-minimal mints the dev key
// directly; the daemon mint relay is wave-2). macOS-only (say/afconvert).
//
//	KOE_E2E=1 OPENAI_API_KEY=sk-... go test -tags '' ./internal/koe/ \
//	    -run TestKoeVoiceE2E -v -count=1 -timeout=120s
//	(prefix PKG_CONFIG_PATH=/opt/homebrew/lib/pkgconfig — the package is cgo.)

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

const e2eModel = "gpt-realtime-mini-2025-12-15"

// the spoken sentence is chosen to clearly demand real work (→ do_task) and to
// carry words the mock can match back, proving ASR understanding end-to-end.
const e2eSpoken = "Add a reminder to call mom at six."

// e2ePersona instructs the front brain to delegate real work — passed to the
// production sessionConfig so "auto" tool_choice reliably picks do_task here.
const e2ePersona = "You are a voice assistant. For any request that is real work, call do_task with the user's words."

func TestKoeVoiceE2E(t *testing.T) {
	if os.Getenv("KOE_E2E") != "1" {
		t.Skip("headless voice E2E: set KOE_E2E=1 + OPENAI_API_KEY (live OpenAI, ~$0.01)")
	}
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		t.Skip("OPENAI_API_KEY required for the live voice E2E")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// 1) Synthesize the "user's voice" as a 48k mono S16 WAV — quiet (-o file,
	// no playback), then verify the format afconvert produced (readWavS16 trusts
	// it blindly).
	pcm := synthSpokenWAV(t, e2eSpoken)
	t.Logf("[wav] %d samples (%.2fs @ 48k mono)", len(pcm), float64(len(pcm))/48000)

	// 2) Mock daemon back-brain: capture the do_task POST /message, reply done.
	gotTask := make(chan string, 1)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/message" {
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Text   string `json:"text"`
				Source string `json:"source"`
			}
			_ = json.Unmarshal(body, &req)
			t.Logf("[mock daemon] POST /message source=%q text=%q", req.Source, req.Text)
			select {
			case gotTask <- req.Text:
			default:
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"reply": "Done, I added the reminder."})
	}))
	defer mock.Close()

	// 3) Plan B wiring pointed at the mock back-brain.
	audio, err := NewAudioIO() // codec only; we feed audio.frames directly (no device)
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	client := NewDaemonClient(mock.URL)
	state := NewCallState("burst-e2e", "")
	disp := NewDispatcher(client, NewAgentResolver(nil, NoopSemanticMatcher{}), state, nil)

	ek, err := mintEphemeral(ctx, apiKey, e2eModel) // DEV-KEY: direct mint (relay is wave-2)
	if err != nil {
		t.Fatalf("mintEphemeral: %v", err)
	}
	t.Log("[mint] ok")

	// 4) Expand Connect INLINE so we can log every oai-event type. The unit tests
	// call handleFunctionCall directly, bypassing handleEvent's switch — so the
	// real event-name routing (function_call_arguments.done, output_audio.delta)
	// is FIRST verified against the live API here. If the mock never gets do_task,
	// the event log shows whether it's a wrong event name vs the model.
	rc, err := newPeerConnection(audio)
	if err != nil {
		t.Fatalf("newPeerConnection: %v", err)
	}
	defer rc.Close()
	h := newEventHandler(disp, state, audio, func(v any) error {
		b, _ := json.Marshal(v)
		return rc.dc.SendText(string(b))
	})

	var (
		mu       sync.Mutex
		eventLog []string
	)
	connected := make(chan struct{})
	configured := make(chan struct{}) // closed on session.updated — config must apply BEFORE audio
	var once, cfgOnce sync.Once
	rc.pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		t.Logf("[conn] %s", s)
		if s == webrtc.PeerConnectionStateConnected {
			once.Do(func() { close(connected) })
		}
	})
	rc.dc.OnOpen(func() {
		// Use the PRODUCTION sessionConfig so this E2E verifies the REAL config
		// (GA schema: output_modalities + audio.output.voice + auto tool_choice),
		// not a parallel copy — a regression to the beta shape would fail here.
		b, _ := json.Marshal(sessionConfig(e2ePersona, "marin"))
		_ = rc.dc.SendText(string(b))
	})
	rc.dc.OnMessage(func(m webrtc.DataChannelMessage) {
		var ev struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(m.Data, &ev)
		mu.Lock()
		eventLog = append(eventLog, ev.Type)
		mu.Unlock()
		// Gate the audio feed on session.updated (config must precede the audio,
		// else the VAD auto response snapshots the default tools:[] config).
		if ev.Type == "session.updated" {
			cfgOnce.Do(func() { close(configured) })
		}
		if strings.Contains(ev.Type, "error") { // surface any rejection on failure
			t.Logf("[event %s] %s", ev.Type, string(m.Data))
		}
		h.handleEvent(ctx, m.Data)
	})
	if err := rc.dialOpenAI(ctx, ek); err != nil {
		t.Fatalf("dialOpenAI: %v", err)
	}
	go rc.pumpSendTrack(ctx)

	// 5) Feed the WAV ONLY after the peer connection is established (frames pushed
	// before ICE connects are lost / burst-sent, which breaks server-VAD).
	go func() {
		select {
		case <-connected:
		case <-ctx.Done():
			return
		}
		// Wait for session.updated so tools + tool_choice + audio modality are in
		// effect BEFORE the audio (and the VAD-auto response it triggers). Without
		// this, the auto response snapshots the default config (tools:[], auto).
		select {
		case <-configured:
		case <-ctx.Done():
			return
		}
		feedWAV(ctx, audio, pcm)
		t.Log("[send] speech + trailing silence streamed")
	}()

	// 6) Wait for the back-brain to receive the delegated task.
	select {
	case task := <-gotTask:
		low := strings.ToLower(task)
		if !strings.Contains(low, "remind") && !strings.Contains(low, "mom") && !strings.Contains(low, "call") {
			t.Errorf("do_task arrived but task text %q doesn't reflect the spoken words %q", task, e2eSpoken)
		}
		t.Logf("VERDICT: PASS — koe side end-to-end. do_task reached the back-brain: %q", task)
	case <-ctx.Done():
		mu.Lock()
		log := eventLog
		mu.Unlock()
		t.Fatalf("VERDICT: FAIL — no do_task reached the mock daemon within timeout.\noai-events seen: %v\n"+
			"(if function_call_arguments.done is absent, OpenAI never invoked the tool; if present but no POST, the do_task→daemon dispatch broke)", log)
	}
}

// feedWAV streams pcm into audio.frames at the 20ms frame cadence, then ~1.2s of
// trailing silence so server-VAD marks end-of-turn. Call from a goroutine AFTER
// the connection is established AND session.updated has landed.
func feedWAV(ctx context.Context, audio *AudioIO, pcm []int16) {
	ticker := time.NewTicker(audioFrameMs * time.Millisecond)
	defer ticker.Stop()
	for off := 0; off+audioFrameSize <= len(pcm); off += audioFrameSize {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		audio.frames <- append([]int16(nil), pcm[off:off+audioFrameSize]...)
	}
	silence := make([]int16, audioFrameSize)
	for range 60 { // ~1.2s
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		audio.frames <- append([]int16(nil), silence...)
	}
}

// TestKoeSayAndAskE2E de-risks the production say-and-ask do_task voicing against
// live GA Realtime: after the model calls do_task, feed the RESULT back as the
// single function_call_output for that call_id + response.create, and assert the
// model SPEAKS it. Mirrors realtime.go handleFunctionCall — no placeholder fast-ack
// or assistant-message inject (that extra voiced turn made the model improvise).
func TestKoeSayAndAskE2E(t *testing.T) {
	if os.Getenv("KOE_E2E") != "1" {
		t.Skip("say-and-ask do_task de-risk: set KOE_E2E=1 + OPENAI_API_KEY")
	}
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		t.Skip("OPENAI_API_KEY required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pcm := synthSpokenWAV(t, e2eSpoken)
	ek, err := mintEphemeral(ctx, apiKey, e2eModel)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	audio, _ := NewAudioIO()
	rc, err := newPeerConnection(audio)
	if err != nil {
		t.Fatalf("pc: %v", err)
	}
	defer rc.Close()
	send := func(v any) { b, _ := json.Marshal(v); _ = rc.dc.SendText(string(b)) }

	const injectedText = "Done — I added a reminder to call mom at six."
	var (
		mu           sync.Mutex
		transcripts  []string
		responseDone int
		spokeResult  bool
	)
	connected := make(chan struct{})
	configured := make(chan struct{})
	var once, cfgOnce sync.Once
	rc.pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			once.Do(func() { close(connected) })
		}
	})
	rc.dc.OnOpen(func() { send(sessionConfig(e2ePersona, "marin")) })
	rc.dc.OnMessage(func(m webrtc.DataChannelMessage) {
		var ev struct {
			Type       string `json:"type"`
			CallID     string `json:"call_id"`
			Transcript string `json:"transcript"`
		}
		_ = json.Unmarshal(m.Data, &ev)
		mu.Lock()
		defer mu.Unlock()
		switch ev.Type {
		case "session.updated":
			cfgOnce.Do(func() { close(configured) })
		case "response.function_call_arguments.done":
			// reachy say-and-ask (production flow, realtime.go handleFunctionCall): the
			// do_task RESULT is the SINGLE function_call_output for this call_id; a
			// response.create then voices it. No placeholder fast-ack + separate
			// assistant-message inject (that extra voiced turn made the model improvise).
			out, _ := json.Marshal(map[string]any{"say": injectedText, "status": "ok"})
			send(map[string]any{"type": "conversation.item.create", "item": map[string]any{
				"type": "function_call_output", "call_id": ev.CallID, "output": string(out)}})
			send(map[string]any{"type": "response.create"})
		case "response.output_audio_transcript.done":
			t.Logf("[spoke] %q", ev.Transcript)
			transcripts = append(transcripts, ev.Transcript)
			if strings.Contains(strings.ToLower(ev.Transcript), "mom") || strings.Contains(strings.ToLower(ev.Transcript), "reminder") {
				spokeResult = true
			}
		case "response.done":
			responseDone++
		case "error", "response.failed":
			t.Logf("[event %s] %s", ev.Type, string(m.Data))
		}
	})
	if err := rc.dialOpenAI(ctx, ek); err != nil {
		t.Fatalf("dial: %v", err)
	}
	go rc.pumpSendTrack(ctx)
	go func() {
		select {
		case <-connected:
		case <-ctx.Done():
			return
		}
		select {
		case <-configured:
		case <-ctx.Done():
			return
		}
		feedWAV(ctx, audio, pcm)
	}()

	deadline := time.After(80 * time.Second)
	for {
		select {
		case <-deadline:
			mu.Lock()
			t.Fatalf("say-and-ask: the model did not speak the do_task result. transcripts=%v", transcripts)
			mu.Unlock()
		case <-time.After(1 * time.Second):
			mu.Lock()
			ok := spokeResult
			done := responseDone
			mu.Unlock()
			if ok {
				t.Logf("say-and-ask VERIFIED: result-as-function_call_output + response.create made the model speak the result")
				return
			}
			if done >= 2 && !ok {
				mu.Lock()
				t.Fatalf("two responses but the do_task result was not spoken. transcripts=%v", transcripts)
				mu.Unlock()
			}
		}
	}
}

// synthSpokenWAV wraps synthSpeech (macOS say + afconvert → 48k mono S16) with
// test semantics: skip if the tools are unavailable, fatal if unusably short.
func synthSpokenWAV(t *testing.T, text string) []int16 {
	t.Helper()
	pcm, err := synthSpeech(text)
	if err != nil {
		t.Skipf("speech synth unavailable: %v", err)
	}
	if len(pcm) < audioFrameSize {
		t.Fatalf("synth WAV too short: %d samples", len(pcm))
	}
	return pcm
}
