//go:build darwin && cgo

package koe

import (
	"path/filepath"
	"testing"
	"time"
)

// TestReadyEarconFramesDecode checks the embedded asset decodes into whole,
// non-silent audioFrameSize frames — the format Play() requires.
func TestReadyEarconFramesDecode(t *testing.T) {
	frames := readyEarconFrames()
	if len(frames) == 0 {
		t.Fatal("no frames decoded from embedded assets/ready.pcm")
	}
	var peak int16
	for i, f := range frames {
		if len(f) != audioFrameSize {
			t.Fatalf("frame %d has %d samples, want %d", i, len(f), audioFrameSize)
		}
		for _, s := range f {
			if s > peak {
				peak = s
			}
		}
	}
	if peak == 0 {
		t.Fatal("embedded earcon decoded to silence")
	}
	t.Logf("decoded %d frames (%dms), peak=%d", len(frames), len(frames)*audioFrameMs, peak)
}

// TestReadyEarconEnabledEnv pins the kill-switch semantics: default on, and the
// KOE_READY_EARCON env var toggles it.
func TestReadyEarconEnabledEnv(t *testing.T) {
	t.Setenv("KOE_READY_EARCON", "")
	if !ReadyEarconEnabled() {
		t.Error("default should be enabled")
	}
	t.Setenv("KOE_READY_EARCON", "0")
	if ReadyEarconEnabled() {
		t.Error("KOE_READY_EARCON=0 should disable")
	}
	t.Setenv("KOE_READY_EARCON", "on")
	if !ReadyEarconEnabled() {
		t.Error("KOE_READY_EARCON=on should enable")
	}
}

// TestPlayReadyEarconMutesMicAndPlays is the self-trigger-safety gate: while the
// earcon plays, capture MUST be suppressed (the mic can't hear the cue), the cue
// MUST be audible on the playback path, and suppression MUST release afterward.
// It drives the headless file backend (shared renderInto), no OpenAI/device.
func TestPlayReadyEarconMutesMicAndPlays(t *testing.T) {
	a, err := NewAudioIO()
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	// Drain the mic channel so the file backend's feed goroutine never blocks.
	go func() {
		for range a.Frames() {
		}
	}()

	out := filepath.Join(t.TempDir(), "earcon.wav")
	if err := a.StartFile(nil, out, audioFrameSize); err != nil {
		t.Fatalf("StartFile: %v", err)
	}

	if a.captureSuppressed() {
		t.Fatal("capture must not be suppressed before the earcon")
	}

	done := make(chan struct{})
	go func() {
		a.PlayReadyEarcon()
		close(done)
	}()

	// Mid-playback: the speaking gate must be suppressing capture. The earcon runs
	// ~1.1s, so 300ms lands squarely inside it.
	time.Sleep(300 * time.Millisecond)
	if !a.captureSuppressed() {
		t.Error("capture must be suppressed WHILE the earcon plays (self-trigger guard)")
	}

	<-done
	if a.captureSuppressed() {
		t.Error("capture suppression must be released after the earcon finishes")
	}

	m := a.CapturedMetrics() // read before Stop tears the backend down
	a.Stop()
	if m.RMS == 0 {
		t.Error("earcon playback captured as silence — the cue did not play")
	}
	t.Logf("earcon playback rms=%.4f samples=%d", m.RMS, m.Samples)
}
