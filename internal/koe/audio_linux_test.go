package koe

import (
	"encoding/json"
	"math"
	"net"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/koe/audiobridge"
)

func TestWirelessBargeInForwardsCaptureWhileSpeaking(t *testing.T) {
	a := &AudioIO{}
	a.SetSpeaking(true)
	t.Setenv("KOE_VPIO_BARGE_IN", "")
	if !a.captureSuppressed() {
		t.Fatal("Wireless must retain half-duplex rollback when barge-in is off")
	}
	t.Setenv("KOE_VPIO_BARGE_IN", "1")
	if !a.captureSuppressed() {
		t.Fatal("Wireless barge-in must fail closed before sustained perception evidence")
	}
	a.SetBargeInAuthorized(true)
	if a.captureSuppressed() {
		t.Fatal("sustained front speech must authorize Wireless talk-over capture")
	}
	a.SetUserMicOff(true)
	if !a.captureSuppressed() {
		t.Fatal("explicit user mic-off must outrank Wireless barge-in")
	}
}

func TestWirelessBargeInPerceptionGateHasExplicitTestBypass(t *testing.T) {
	a := &AudioIO{}
	a.SetSpeaking(true)
	t.Setenv("KOE_VPIO_BARGE_IN", "1")
	t.Setenv("KOE_BARGE_PERCEPTION_GATE", "0")
	if a.captureSuppressed() {
		t.Fatal("deterministic injection seam must be able to bypass perception")
	}
}

func TestWirelessQueuedCaptureIsRegatedAtSendTime(t *testing.T) {
	a := &AudioIO{}
	t.Setenv("KOE_VPIO_BARGE_IN", "1")
	a.SetSpeaking(true)
	raw := []int16{123, -456}
	if got := a.captureFrameForSend(raw); len(got) != audioFrameSize || got[0] != 0 {
		t.Fatalf("unauthorized queued frame = %v, want a full silence frame", got)
	}
	a.SetBargeInAuthorized(true)
	if got := a.captureFrameForSend(raw); len(got) != len(raw) || got[0] != raw[0] {
		t.Fatalf("authorized queued frame = %v, want %v", got, raw)
	}
}

func TestWirelessInterruptPlaybackSendsCarrierFlush(t *testing.T) {
	koeConn, carrierConn := net.Pipe()
	defer koeConn.Close()
	defer carrierConn.Close()
	a := &AudioIO{conn: koeConn, playBuf: make(chan []int16, 2)}
	a.playback.Store(true)
	a.Play([]int16{1, 2, 3})

	got := make(chan []byte, 1)
	go func() {
		body, err := readControl(carrierConn)
		if err == nil {
			got <- body
		}
	}()
	a.InterruptPlayback()

	select {
	case body := <-got:
		var msg map[string]string
		if err := json.Unmarshal(body, &msg); err != nil {
			t.Fatalf("decode control: %v", err)
		}
		if msg["type"] != "barge_in" {
			t.Fatalf("control type = %q, want barge_in", msg["type"])
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for carrier barge_in control")
	}
	if got := len(a.playBuf); got != 0 {
		t.Fatalf("Koe playback queue retained %d frame(s)", got)
	}
	if a.playback.Load() {
		t.Fatal("playback must be disabled after interruption")
	}
}

func TestWirelessNativeEarconSendsClosedCueControl(t *testing.T) {
	koeConn, carrierConn := net.Pipe()
	defer koeConn.Close()
	defer carrierConn.Close()
	a := &AudioIO{conn: koeConn}

	got := make(chan []byte, 1)
	go func() {
		body, err := readControl(carrierConn)
		if err == nil {
			got <- body
		}
	}()
	if !a.playNativeEarcon("ready") {
		t.Fatal("native cue control send failed")
	}

	select {
	case body := <-got:
		var msg map[string]string
		if err := json.Unmarshal(body, &msg); err != nil {
			t.Fatalf("decode control: %v", err)
		}
		if msg["type"] != "play_cue" || msg["name"] != "ready" {
			t.Fatalf("native cue control = %#v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for play_cue control")
	}
}

// spkTestIO builds a minimal AudioIO carrying only the spk-leg resampler, so the
// toCarrierPCM tests exercise the down-rate path without constructing the cgo Opus
// codec that NewAudioIO would.
func spkTestIO() *AudioIO {
	return &AudioIO{spkRS: NewResampler(audioSampleRate, carrierWireRate)}
}

// The carrier is a thin no-DSP relay: koe owns the transcode on BOTH legs
// (spec §9-b.1). On the spk uplink koe must down-rate its 48k codec audio to the
// carrier wire rate (16k, daemon-native) before framing, so the carrier only does
// S16→F32. These tests pin that down-rate.

func TestCarrierWireRateIs16k(t *testing.T) {
	// The daemon SDK AudioBase.SAMPLE_RATE is 16000 and its appsrc caps pin 16k
	// with no resampling (recon 2026-07-14). The uplink wire rate must match.
	if carrierWireRate != 16000 {
		t.Fatalf("carrierWireRate = %d, want 16000 (daemon-native, §9-b.1)", carrierWireRate)
	}
}

func TestSpeakerRingFramesDefaultIs100ms(t *testing.T) {
	t.Setenv("KOE_SPK_RING_FRAMES", "")
	got, err := speakerRingFramesFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if got != 5 {
		t.Fatalf("speakerRingFramesFromEnv = %d, want 5 (100 ms)", got)
	}
}

func TestSpeakerRingFramesOverride(t *testing.T) {
	t.Setenv("KOE_SPK_RING_FRAMES", "10")
	got, err := speakerRingFramesFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if got != 10 {
		t.Fatalf("speakerRingFramesFromEnv = %d, want 10", got)
	}
}

func TestSpeakerRingFramesInvalidFailsLoud(t *testing.T) {
	for _, value := range []string{"bad", "0", "51"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("KOE_SPK_RING_FRAMES", value)
			if _, err := speakerRingFramesFromEnv(); err == nil {
				t.Fatalf("speakerRingFramesFromEnv(%q) succeeded, want error", value)
			}
		})
	}
}

func TestWirelessEarconPacingPreservesEveryFrame(t *testing.T) {
	koeConn, carrierConn := net.Pipe()
	a := &AudioIO{
		playBuf:       make(chan []int16, 256),
		conn:          koeConn,
		done:          make(chan struct{}),
		spkRS:         NewResampler(audioSampleRate, carrierWireRate),
		spkRingFrames: 5,
	}
	a.playback.Store(true)
	a.wg.Add(1)
	go a.spkPump()

	const frameCount = 24
	received := make(chan int, 1)
	go func() {
		count := 0
		for count < frameCount {
			hdr, _, err := audiobridge.ReadFrame(carrierConn)
			if err != nil {
				break
			}
			if hdr.Magic == audiobridge.MagicSpk {
				count++
			}
		}
		received <- count
	}()

	frames := make([][]int16, frameCount)
	for i := range frames {
		frames[i] = make([]int16, audioFrameSize)
		frames[i][0] = int16(i + 1)
	}
	a.queueEarconFrames(frames)

	select {
	case got := <-received:
		if got != frameCount {
			t.Fatalf("carrier received %d earcon frames, want %d", got, frameCount)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for paced earcon frames")
	}
	close(a.done)
	_ = koeConn.Close()
	_ = carrierConn.Close()
	a.wg.Wait()
}

func TestMicCaptureQueueDropsOldest(t *testing.T) {
	ch := make(chan []int16, 2)
	enqueueLatestPCM(ch, []int16{1})
	enqueueLatestPCM(ch, []int16{2})
	enqueueLatestPCM(ch, []int16{3})
	if got := (<-ch)[0]; got != 2 {
		t.Fatalf("oldest retained = %d, want 2", got)
	}
	if got := (<-ch)[0]; got != 3 {
		t.Fatalf("latest retained = %d, want 3", got)
	}
}

func TestToCarrierPCMDownratesToWireRate(t *testing.T) {
	// 48k → 16k is a 3:1 decimation: N input samples → N/3 output samples.
	in := make([]int16, audioFrameSize) // 960 samples @ 48k = 20 ms
	got := spkTestIO().toCarrierPCM(in)
	want := audioFrameSize * carrierWireRate / audioSampleRate // 960*16000/48000 = 320
	if len(got) != want {
		t.Fatalf("toCarrierPCM len = %d, want %d (48k→16k on a 20ms frame)", len(got), want)
	}
}

func TestToCarrierPCMPreservesDCLevel(t *testing.T) {
	// A constant (DC) signal must survive resampling unchanged. The anti-alias FIR
	// ramps up over its first ~N/M output samples (inherent to any windowed-sinc
	// resampler), so feed several frames through one stateful resampler and assert
	// DC survives on the settled tail rather than from sample 0.
	const level int16 = 8000
	in := make([]int16, audioFrameSize)
	for i := range in {
		in[i] = level
	}
	a := spkTestIO()
	var got []int16
	for range 4 {
		got = append(got, a.toCarrierPCM(in)...)
	}
	tail := got[len(got)-100:] // steady state, past the filter ramp-up
	for i, v := range tail {
		if math.Abs(float64(v)-float64(level)) > 20 { // ±20 for passband ripple + float round-trip
			t.Fatalf("steady-state sample %d = %d, want ~%d (DC must survive down-rate)", i, v, level)
		}
	}
}

// A frame framed after toCarrierPCM must declare the wire rate in its header so the
// carrier and the audiobridge PayloadLen check agree.
func TestSpkFrameHeaderDeclaresWireRate(t *testing.T) {
	pcm := spkTestIO().toCarrierPCM(make([]int16, audioFrameSize))
	h := audiobridge.Header{
		Magic:      audiobridge.MagicSpk,
		Format:     audiobridge.FormatS16LE,
		Channels:   audioChannels,
		SampleRate: carrierWireRate,
		NSamples:   uint32(len(pcm)),
	}
	if h.PayloadLen() != len(pcm)*2 {
		t.Fatalf("PayloadLen = %d, want %d", h.PayloadLen(), len(pcm)*2)
	}
	if h.SampleRate != 16000 {
		t.Fatalf("spk header SampleRate = %d, want 16000", h.SampleRate)
	}
}
