package koe

import (
	"sync"
	"testing"
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

// A typed-nil host implementation must normalize to "no audio attached" rather
// than becoming a live-looking interface over a nil pointer.
func TestSessionNormalizesTypedNilAudio(t *testing.T) {
	var typedNil *fakeExternalAudio
	s := NewSession(SessionConfig{BurstID: "burst-typed-nil", Audio: typedNil})
	// Would panic on first audio call if the normalization were missing.
	s.HandleEvent([]byte(`{"type":"response.created","response":{"id":"resp-1"}}`))
}
