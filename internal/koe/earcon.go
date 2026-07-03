//go:build darwin && cgo

package koe

import (
	_ "embed"
	"encoding/binary"
	"log"
	"time"
)

// ready.pcm is the "ready" earcon played once when a Desktop call reaches the
// listening state — a short, soft brand cue (the "aurora" design) so the user
// hears that Kocoro is ready without a spoken line. Format: 48kHz mono 16-bit
// little-endian PCM, frame-aligned to audioFrameSize (960-sample / 20ms) frames,
// i.e. exactly the format Play() accepts.
//
//go:embed assets/ready.pcm
var readyEarconPCM []byte

// ReadyEarconEnabled reports whether the ready earcon should play. Default on;
// KOE_READY_EARCON=0 disables it for users who find the cue intrusive. WORKLOAD:
// Desktop voice calls. SYMPTOM if it binds: a user wants silence on connect.
// OVERRIDE: this env var (there is no config surface — it's a personal cue).
func ReadyEarconEnabled() bool { return koeEnvBool("KOE_READY_EARCON", true) }

// readyEarconFrames decodes the embedded PCM into audioFrameSize int16 frames.
// Each frame is a freshly allocated slice because Play() takes ownership of the
// slice it is handed without copying.
func readyEarconFrames() [][]int16 {
	n := len(readyEarconPCM) / 2
	if n < audioFrameSize {
		return nil
	}
	frames := make([][]int16, 0, n/audioFrameSize)
	for off := 0; off+audioFrameSize <= n; off += audioFrameSize {
		frame := make([]int16, audioFrameSize)
		for i := range audioFrameSize {
			frame[i] = int16(binary.LittleEndian.Uint16(readyEarconPCM[(off+i)*2:]))
		}
		frames = append(frames, frame)
	}
	return frames
}

// PlayReadyEarcon plays the embedded ready earcon through the playback path with
// the mic muted for the cue's full duration, then restores the prior gate state.
// It blocks until playback drains, so call it in a goroutine.
//
// Self-trigger safety: it raises the speaking gate (SetSpeaking), which BOTH
// backends honor — the oto half-duplex path drops capture while speaking, and the
// VPIO path's shouldForwardVPIOCapture drops capture while speaking too (default,
// barge-in off). So the cue is never fed to the server VAD and cannot make Koe
// "answer" its own sound. At the listening moment no reply is in flight
// (PrepareForCall zeroed the gates), so nothing is clobbered; the captured prior
// state is restored on return regardless.
func (a *AudioIO) PlayReadyEarcon() {
	frames := readyEarconFrames()
	if len(frames) == 0 {
		return
	}
	prevSpeaking := a.speaking.Load()
	prevPlayback := a.playback.Load()
	started := time.Now()

	a.SetPlaybackEnabled(true)
	a.SetSpeaking(true)

	// Queue every frame up front. renderInto only starts draining once
	// prerollFrames have accumulated, so drip-feeding one frame per tick would
	// never cross the pre-roll threshold and nothing would play. playBuf (cap 256)
	// holds the whole cue without overflow.
	for _, f := range frames {
		a.Play(f)
	}

	// Hold the speaking gate until playback has fully drained. Releasing at feed
	// time would unmute the mic while the tail is still audible on the speaker,
	// re-opening the self-trigger window. Wall time = pre-roll fill + one 20ms
	// device tick per frame, plus a small margin.
	playMs := (len(frames) + prerollFrames + 2) * audioFrameMs
	time.Sleep(time.Duration(playMs) * time.Millisecond)

	a.SetSpeaking(prevSpeaking)
	a.SetPlaybackEnabled(prevPlayback)
	log.Printf("koe[earcon]: ready earcon played frames=%d dur=%dms", len(frames), time.Since(started).Milliseconds())
}
