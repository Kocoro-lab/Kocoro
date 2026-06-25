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
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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
	var once sync.Once
	rc.pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		t.Logf("[conn] %s", s)
		if s == webrtc.PeerConnectionStateConnected {
			once.Do(func() { close(connected) })
		}
	})
	rc.dc.OnOpen(func() {
		// TEST-ONLY session.update: force tool_choice to do_task so the assertion
		// is deterministic (production sessionConfig uses "auto", which the mini
		// model satisfies nondeterministically). The forced choice still proves
		// ASR — OpenAI fills the task arg from the spoken words.
		su := map[string]any{
			"type": "session.update",
			"session": map[string]any{
				"type":         "realtime",
				"instructions": "You are a voice assistant. For any request that is real work, call do_task with the user's words.",
				"tools":        ToolDefs(),
				"tool_choice":  map[string]any{"type": "function", "name": "do_task"},
			},
		}
		b, _ := json.Marshal(su)
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
		time.Sleep(300 * time.Millisecond) // let dc open + session.update land
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
		// trailing silence so server-VAD marks end-of-turn.
		silence := make([]int16, audioFrameSize)
		for i := 0; i < 60; i++ { // ~1.2s
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			audio.frames <- append([]int16(nil), silence...)
		}
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

// synthSpokenWAV uses macOS say (quiet, -o file) + afconvert to make a 48k mono
// S16 WAV of text, verifies the format with afinfo, and returns its PCM samples.
func synthSpokenWAV(t *testing.T, text string) []int16 {
	t.Helper()
	dir := t.TempDir()
	aiff := filepath.Join(dir, "spoken.aiff")
	wav := filepath.Join(dir, "spoken.wav")
	if out, err := exec.Command("say", text, "-o", aiff).CombinedOutput(); err != nil {
		t.Skipf("`say` unavailable (%v): %s", err, out)
	}
	// 48 kHz, mono, little-endian signed 16-bit — exactly what the Opus/WebRTC path expects.
	if out, err := exec.Command("afconvert", "-f", "WAVE", "-d", "LEI16@48000", "-c", "1", aiff, wav).CombinedOutput(); err != nil {
		t.Skipf("`afconvert` failed (%v): %s", err, out)
	}
	if info, err := exec.Command("afinfo", wav).CombinedOutput(); err == nil {
		s := string(info)
		if !strings.Contains(s, "48000") || !strings.Contains(s, "1 ch") {
			t.Fatalf("afconvert did not produce 48k mono: %s", s)
		}
	}
	pcm, err := readWavS16(wav)
	if err != nil {
		t.Fatalf("readWavS16: %v", err)
	}
	if len(pcm) < audioFrameSize {
		t.Fatalf("WAV too short: %d samples", len(pcm))
	}
	return pcm
}

// readWavS16 reads a PCM16 WAV, locating the data chunk (header not assumed 44 bytes).
func readWavS16(path string) ([]int16, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, errNotWave
	}
	for i := 12; i+8 <= len(b); {
		id := string(b[i : i+4])
		sz := int(binary.LittleEndian.Uint32(b[i+4 : i+8]))
		if id == "data" {
			start, end := i+8, i+8+sz
			if end > len(b) {
				end = len(b)
			}
			d := b[start:end]
			out := make([]int16, len(d)/2)
			for j := range out {
				out[j] = int16(binary.LittleEndian.Uint16(d[2*j:]))
			}
			return out, nil
		}
		i += 8 + sz + (sz & 1)
	}
	return nil, errNoDataChunk
}

var (
	errNotWave     = &wavErr{"not a RIFF/WAVE file"}
	errNoDataChunk = &wavErr{"no data chunk"}
)

type wavErr struct{ s string }

func (e *wavErr) Error() string { return e.s }
