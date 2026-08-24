package koebind

import (
	"sync"
	"testing"
	"time"
)

type fakeCallHost struct{ ended chan struct{} }

func (f *fakeCallHost) EndCall() { f.ended <- struct{}{} }

// The brain's hang-up must cross the binding: a Swift host that supplies a
// CallHost gets EndCall when the model calls the end_call voice tool. Without
// this the brain enters its local terminal and the iOS call runs forever.
func TestBridgeEndCallReachesCallHost(t *testing.T) {
	host := &fakeCallHost{ended: make(chan struct{}, 1)}
	b := NewBridge("burst-bind-end", "", "", nil, nil, nil, nil, host)

	b.HandleEvent([]byte(`{"type":"input_audio_buffer.committed"}`))
	b.HandleEvent([]byte(`{"type":"response.created","response":{"id":"end-response"}}`))
	b.HandleEvent([]byte(`{"type":"response.function_call_arguments.done","response_id":"end-response","name":"end_call","call_id":"c1","arguments":"{}"}`))

	select {
	case <-host.ended:
	case <-time.After(2 * time.Second):
		t.Fatal("end_call never crossed the binding to CallHost.EndCall")
	}
}

type recordingSink struct {
	mu     sync.Mutex
	calls  []string
	speaks bool
}

func (r *recordingSink) record(c string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, c)
}
func (r *recordingSink) has(c string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, got := range r.calls {
		if got == c {
			return true
		}
	}
	return false
}
func (r *recordingSink) SetSpeaking(s bool) {
	r.mu.Lock()
	r.speaks = s
	r.mu.Unlock()
	r.record("SetSpeaking")
}
func (r *recordingSink) SetPlaybackEnabled(bool) { r.record("SetPlaybackEnabled") }
func (r *recordingSink) SetPlaybackPaused(p bool) {
	if p {
		r.record("SetPlaybackPaused(true)")
	} else {
		r.record("SetPlaybackPaused(false)")
	}
}
func (r *recordingSink) DropCapture() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.speaks
}
func (r *recordingSink) UserMicOff() bool     { return false }
func (r *recordingSink) SetUserMicOff(bool)   {}
func (r *recordingSink) UserMicSticky() bool  { return false }
func (r *recordingSink) PlaybackIdle() bool   { return false }

// Talk-over must cross the binding as a native-floor pause: with barge-in
// enabled, the user speaking over Kocoro pauses playback (a resumable hold for
// the floor judge) instead of being ignored. This is the iOS barge-in wiring —
// SetBargeIn arms the same env gate cmd/koe.go's --barge-in arms, and NewBridge
// must declare the host full-duplex or the floor gate stays cold.
func TestBargeInPausesPlaybackAcrossBinding(t *testing.T) {
	SetBargeIn(true)
	t.Cleanup(func() { SetBargeIn(false) })

	sink := &recordingSink{}
	b := NewBridge("burst-bind-floor", "", "", sink, nil, nil, nil, nil)

	b.HandleEvent([]byte(`{"type":"response.created","response":{"id":"resp-floor"}}`))
	b.HandleEvent([]byte(`{"type":"output_audio_buffer.started","response_id":"resp-floor"}`))
	b.HandleEvent([]byte(`{"type":"input_audio_buffer.speech_started"}`))

	if !sink.has("SetPlaybackPaused(true)") {
		t.Fatalf("talk-over with barge-in on must pause playback for the floor judge; sink saw %v", sink.calls)
	}
}

// Without SetBargeIn the binding must keep the half-duplex default: talk-over
// while speaking is impossible (mic gated), so a stray speech_started must not
// pause anything.
func TestBargeInOffKeepsHalfDuplex(t *testing.T) {
	SetBargeIn(false)

	sink := &recordingSink{}
	b := NewBridge("burst-bind-halfduplex", "", "", sink, nil, nil, nil, nil)

	b.HandleEvent([]byte(`{"type":"response.created","response":{"id":"resp-hd"}}`))
	b.HandleEvent([]byte(`{"type":"output_audio_buffer.started","response_id":"resp-hd"}`))
	b.HandleEvent([]byte(`{"type":"input_audio_buffer.speech_started"}`))

	if sink.has("SetPlaybackPaused(true)") {
		t.Fatalf("barge-in off must not pause playback on speech_started; sink saw %v", sink.calls)
	}
}
