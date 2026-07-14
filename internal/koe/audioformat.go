package koe

import "math"

// Platform-neutral audio format constants and pure DSP. Extracted from audio.go
// (darwin-only, cgo) so the audio-adjacent logic (wav decode, noise gate) and the
// upcoming linux UDS-PCM backend can share them without pulling in the CoreAudio
// backend. These are format definitions and pure math — no device/backend coupling.
const (
	audioSampleRate = 48000                                 // WebRTC/Opus path (NOT the 24k WS path)
	audioChannels   = 1                                     // mono capture/playback
	audioFrameMs    = 20                                    // 20 ms frames
	audioFrameSize  = audioSampleRate / 1000 * audioFrameMs // 960 samples
)

// prerollFrames is the playback jitter-buffer pre-roll depth. 8 frames = ~160 ms,
// the low end of typical voice jitter buffers. OVERRIDE: raise if a slow link
// still underruns.
const prerollFrames = 8

// rmsLevel returns the RMS amplitude of a PCM frame normalized to 0..1.
func rmsLevel(pcm []int16) float64 {
	if len(pcm) == 0 {
		return 0
	}
	var sumSq float64
	for _, s := range pcm {
		v := float64(s)
		sumSq += v * v
	}
	return math.Sqrt(sumSq/float64(len(pcm))) / 32768.0
}
