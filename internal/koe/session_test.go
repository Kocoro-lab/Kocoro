package koe

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeExternalAudio stands in for the host audio layer (Swift on iOS). It records
// what the brain asked for, so these tests can assert the brain drives the audio
// gates without a real device.
type fakeExternalAudio struct {
	mu       sync.Mutex
	calls    []string
	speaking bool
}

func (f *fakeExternalAudio) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeExternalAudio) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeExternalAudio) SetSpeaking(s bool) {
	f.mu.Lock()
	f.speaking = s
	f.mu.Unlock()
	f.record("SetSpeaking")
}
func (f *fakeExternalAudio) SetPlaybackEnabled(bool) { f.record("SetPlaybackEnabled") }
func (f *fakeExternalAudio) SetPlaybackPaused(bool)  { f.record("SetPlaybackPaused") }
func (f *fakeExternalAudio) DropCapture() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.speaking
}
func (f *fakeExternalAudio) SetUserMicOff(bool)  { f.record("SetUserMicOff") }
func (f *fakeExternalAudio) UserMicOff() bool    { return false }
func (f *fakeExternalAudio) UserMicSticky() bool { return false }
func (f *fakeExternalAudio) PlaybackIdle() bool  { return true }

func contains(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}

// The façade must actually reach the brain: a response starting has to gate the
// microphone, or the model hears its own voice and answers itself.
func TestSessionDrivesHostAudioOnResponseStart(t *testing.T) {
	audio := &fakeExternalAudio{}
	s := NewSession(SessionConfig{BurstID: "burst-facade", Audio: audio})

	s.HandleEvent([]byte(`{"type":"response.created","response":{"id":"resp-1"}}`))

	calls := audio.snapshot()
	if !contains(calls, "SetSpeaking") {
		t.Fatalf("response.created must gate the mic; host saw %v", calls)
	}
	if !contains(calls, "SetPlaybackEnabled") {
		t.Fatalf("response.created must open playback; host saw %v", calls)
	}
}

// A host that supplies no audio layer must not crash the brain — the same
// contract the nil audio path has had since before AudioController existed.
func TestSessionWithoutAudioIsInert(t *testing.T) {
	s := NewSession(SessionConfig{BurstID: "burst-no-audio"})
	s.HandleEvent([]byte(`{"type":"response.created","response":{"id":"resp-1"}}`))
	s.HandleEvent([]byte(`{"type":"response.done","response":{"id":"resp-1","status":"completed"}}`))
}

// Malformed transport payloads must be tolerated, not fatal: the brain sits
// directly on a network-fed data channel.
func TestSessionToleratesGarbageEvents(t *testing.T) {
	audio := &fakeExternalAudio{}
	s := NewSession(SessionConfig{BurstID: "burst-garbage", Audio: audio})

	s.HandleEvent([]byte(`not json at all`))
	s.HandleEvent([]byte(`{}`))
	s.HandleEvent([]byte(`{"type":"totally.unknown.event"}`))
	s.HandleEvent(nil)

	// Still functional afterwards.
	s.HandleEvent([]byte(`{"type":"response.created","response":{"id":"resp-2"}}`))
	if !contains(audio.snapshot(), "SetSpeaking") {
		t.Fatal("brain stopped responding after malformed input")
	}
}

// BurstID scopes the task ledger; the host reads it back to correlate work.
func TestSessionExposesBurstID(t *testing.T) {
	s := NewSession(SessionConfig{BurstID: "burst-xyz", Audio: &fakeExternalAudio{}})
	if got := s.BurstID(); got != "burst-xyz" {
		t.Fatalf("BurstID() = %q, want %q", got, "burst-xyz")
	}
}

type scriptedTaskBackend struct{ response string }

func (b scriptedTaskBackend) DoTask(string) (string, error) { return b.response, nil }
func (b scriptedTaskBackend) Cancel(string) error           { return nil }
func (b scriptedTaskBackend) ListAgents() (string, error)   { return `{"agents":[]}`, nil }

// A completed do_task must reach the model through the façade: macOS starts the
// mailbox delivery worker in Connect, so the façade owns starting it here. The
// 2026-08-21 device log showed the failure mode when it is missing — the result
// sits in the mailbox forever, and the model keeps answering "no news yet"
// about work the daemon finished minutes ago.
func TestSessionDeliversCompletedTaskResult(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	s := NewSession(SessionConfig{
		BurstID: "burst-deliver",
		Audio:   &fakeExternalAudio{},
		Send: func(payload string) error {
			mu.Lock()
			sent = append(sent, payload)
			mu.Unlock()
			return nil
		},
		TaskBackend: scriptedTaskBackend{response: `{"reply":"Haidian is 31C","session_id":"sess-1"}`},
	})

	s.HandleEvent([]byte(`{"type":"input_audio_buffer.committed","item_id":"item-user-1"}`))
	s.HandleEvent([]byte(`{"type":"response.created","response":{"id":"resp-1"}}`))
	s.HandleEvent([]byte(`{"type":"response.function_call_arguments.done","response_id":"resp-1","call_id":"call-1","name":"do_task","arguments":"{\"task\":\"check the weather\"}"}`))
	s.HandleEvent([]byte(`{"type":"response.done","response":{"id":"resp-1","status":"completed"}}`))

	deadline := time.Now().Add(5 * time.Second)
	injected, continued := false, false
	acked := 0
	for time.Now().Before(deadline) {
		mu.Lock()
		injected, continued = false, false
		creates := 0
		for _, payload := range sent {
			if strings.Contains(payload, `"type":"response.create"`) {
				creates++
			}
			if strings.Contains(payload, "conversation.item.create") && strings.Contains(payload, "Haidian is 31C") {
				injected = true
				continue
			}
			if injected && strings.Contains(payload, `"type":"response.create"`) {
				continued = true
			}
		}
		mu.Unlock()
		if continued {
			break
		}
		// Acknowledge each outbound response.create the way the provider would:
		// created, then a completed done, so the response slot frees up again.
		for acked < creates {
			acked++
			id := fmt.Sprintf("resp-synth-%d", acked)
			s.HandleEvent([]byte(`{"type":"response.created","response":{"id":"` + id + `"}}`))
			s.HandleEvent([]byte(`{"type":"response.done","response":{"id":"` + id + `","status":"completed"}}`))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !injected {
		t.Fatal("completed task result was never injected into the conversation")
	}
	if !continued {
		t.Fatal("no response.create followed the injected task result — the model was never asked to speak it")
	}
}

// A typed-nil host implementation must normalize to "no audio attached" rather
// than becoming a live-looking interface over a nil pointer.
func TestSessionNormalizesTypedNilAudio(t *testing.T) {
	var typedNil *fakeExternalAudio
	s := NewSession(SessionConfig{BurstID: "burst-typed-nil", Audio: typedNil})
	// Would panic on first audio call if the normalization were missing.
	s.HandleEvent([]byte(`{"type":"response.created","response":{"id":"resp-1"}}`))
}

// end_call must reach the host through the façade: on iOS the host's teardown
// IS the hang-up. macOS wires onEndCall in cmd/koe.go (goodbye earcon + close);
// a façade host with no hook gets a brain that enters its local terminal while
// the call's transport and audio keep running forever — "再见" then silence,
// with the call screen still up (2026-08-21 assessment, outstanding item #2).
// The same hook also arms the ASR dismiss-phrase backstop, which is skipped
// entirely while onEndCall is nil.
func TestSessionEndCallReachesHost(t *testing.T) {
	ended := make(chan struct{}, 1)
	s := NewSession(SessionConfig{
		BurstID:   "burst-end-host",
		Audio:     &fakeExternalAudio{},
		OnEndCall: func() { ended <- struct{}{} },
	})

	s.HandleEvent([]byte(`{"type":"input_audio_buffer.committed"}`))
	s.HandleEvent([]byte(`{"type":"response.created","response":{"id":"end-response"}}`))
	s.HandleEvent([]byte(`{"type":"response.function_call_arguments.done","response_id":"end-response","name":"end_call","call_id":"c1","arguments":"{}"}`))

	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("end_call never reached the host's OnEndCall")
	}
}

// The façade must arm the runtime floor gate from its config. Only the macOS
// path (webrtc.go) ever set handler.fullDuplexAEC, so an iOS host that enabled
// barge-in via env still had nativeFloorEnabled() == false — speech_started
// during playback fell through to the legacy interrupt path instead of the
// pause-and-judge floor (2026-08-21 assessment, outstanding item #3).
func TestSessionFullDuplexAECArmsNativeFloor(t *testing.T) {
	t.Setenv("KOE_VPIO_BARGE_IN", "1")

	on := NewSession(SessionConfig{
		BurstID:       "burst-floor-on",
		Audio:         &fakeExternalAudio{},
		FullDuplexAEC: true,
	})
	if !on.handler.nativeFloorEnabled() {
		t.Fatal("FullDuplexAEC: true must arm the native floor gate")
	}

	off := NewSession(SessionConfig{
		BurstID: "burst-floor-off",
		Audio:   &fakeExternalAudio{},
	})
	if off.handler.nativeFloorEnabled() {
		t.Fatal("floor gate must stay off for a half-duplex host")
	}
}
