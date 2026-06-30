package koe

import (
	"math"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestStartVPIOHardwareCapturesAndPlays(t *testing.T) {
	if os.Getenv("KOE_VPIO_TEST") != "1" {
		t.Skip("set KOE_VPIO_TEST=1 to exercise the macOS VPIO hardware backend")
	}
	a, err := NewAudioIO()
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	if err := a.StartVPIO(); err != nil {
		t.Fatalf("StartVPIO: %v", err)
	}
	defer a.Stop()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 160; i++ {
			frame := make([]int16, audioFrameSize)
			for j := range frame {
				s := math.Sin(2 * math.Pi * 440 * float64(i*audioFrameSize+j) / audioSampleRate)
				frame[j] = int16(s * 800)
			}
			a.Play(frame)
			time.Sleep(audioFrameMs * time.Millisecond)
		}
	}()

	cmd := exec.Command("say", "kocoro v p i o capture test")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start say: %v", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	deadline := time.After(8 * time.Second)
	gotFrames := 0
	maxLevel := 0.0
	for gotFrames < 8 || maxLevel < 0.001 {
		select {
		case <-deadline:
			stats := a.vpioDebugStats()
			t.Fatalf("VPIO did not capture audible input: frames=%d max_level=%.5f stats=%+v", gotFrames, maxLevel, stats)
		case frame := <-a.Frames():
			gotFrames++
			if level := rmsLevel(frame); level > maxLevel {
				maxLevel = level
			}
		}
	}
	<-done
	stats := a.vpioDebugStats()
	if stats.InputCallbacks == 0 || stats.InputFrames == 0 {
		t.Fatalf("VPIO input callback did not run: %+v", stats)
	}
	if stats.OutputCallbacks == 0 || stats.OutputFrames == 0 {
		t.Fatalf("VPIO output callback did not run: %+v", stats)
	}
	t.Logf("VPIO hardware stats: %+v", stats)
}

func TestVPIOHardwareDropsCaptureWhileSpeaking(t *testing.T) {
	if os.Getenv("KOE_VPIO_TEST") != "1" {
		t.Skip("set KOE_VPIO_TEST=1 to exercise the macOS VPIO hardware backend")
	}
	a, err := NewAudioIO()
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	if err := a.StartVPIO(); err != nil {
		t.Fatalf("StartVPIO: %v", err)
	}
	defer a.Stop()

	drainCapturedFrames(a, 300*time.Millisecond)
	a.SetSpeaking(true)
	drainCapturedFrames(a, 120*time.Millisecond)
	before := a.vpioDebugStats()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			frame := make([]int16, audioFrameSize)
			for j := range frame {
				s := math.Sin(2 * math.Pi * 660 * float64(i*audioFrameSize+j) / audioSampleRate)
				frame[j] = int16(s * 4000)
			}
			a.Play(frame)
			time.Sleep(audioFrameMs * time.Millisecond)
		}
	}()

	deadline := time.After(4 * time.Second)
	for {
		select {
		case frame := <-a.Frames():
			t.Fatalf("captured frame forwarded while speaking; rms=%.5f", rmsLevel(frame))
		case <-done:
			after := a.vpioDebugStats()
			if after.GateDropped <= before.GateDropped {
				t.Fatalf("speaking gate did not drop any VPIO capture frames: before=%+v after=%+v", before, after)
			}
			if after.ForwardedFrames != before.ForwardedFrames {
				t.Fatalf("VPIO forwarded capture while speaking: before=%+v after=%+v", before, after)
			}
			if after.PlayUnderruns != before.PlayUnderruns {
				t.Fatalf("VPIO playback underrun while testing speaking gate: before=%+v after=%+v", before, after)
			}
			t.Logf("VPIO speaking-gate stats: before=%+v after=%+v", before, after)
			return
		case <-deadline:
			t.Fatalf("timed out waiting for speaking-gate playback; stats=%+v", a.vpioDebugStats())
		}
	}
}

func TestVPIOHardwareBurstPlaybackDoesNotOverwriteRing(t *testing.T) {
	if os.Getenv("KOE_VPIO_TEST") != "1" {
		t.Skip("set KOE_VPIO_TEST=1 to exercise the macOS VPIO hardware backend")
	}
	a, err := NewAudioIO()
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	if err := a.StartVPIO(); err != nil {
		t.Fatalf("StartVPIO: %v", err)
	}
	defer a.Stop()
	a.SetSpeaking(true)

	before := a.vpioDebugStats()
	for i := 0; i < 140; i++ {
		frame := make([]int16, audioFrameSize)
		for j := range frame {
			s := math.Sin(2 * math.Pi * 440 * float64(i*audioFrameSize+j) / audioSampleRate)
			frame[j] = int16(s * 2500)
		}
		a.Play(frame)
	}
	time.Sleep(700 * time.Millisecond)
	after := a.vpioDebugStats()
	if after.PlayOverwrites != before.PlayOverwrites {
		t.Fatalf("VPIO playback ring overwrote queued audio under burst input: before=%+v after=%+v", before, after)
	}
	if after.PlayBuffered > vpioPlaybackHighWaterSamples+audioFrameSize {
		t.Fatalf("VPIO playback buffered too much audio, latency risk: %+v", after)
	}
	t.Logf("VPIO burst playback stats: before=%+v after=%+v", before, after)
}

func drainCapturedFrames(a *AudioIO, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case <-a.Frames():
		case <-timer.C:
			return
		}
	}
}
