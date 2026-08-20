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
	if a.captureSuppressed() {
		t.Fatal("Wireless product barge-in must not require DOA evidence")
	}
	t.Setenv("KOE_BARGE_PERCEPTION_GATE", "1")
	if !a.captureSuppressed() {
		t.Fatal("an explicitly enabled perception gate must fail closed before evidence")
	}
	a.SetBargeInAuthorized(true)
	if a.captureSuppressed() {
		t.Fatal("sustained front speech must authorize talk-over capture")
	}
	a.SetUserMicOff(true)
	if !a.captureSuppressed() {
		t.Fatal("explicit user mic-off must outrank Wireless barge-in")
	}
}

func TestWirelessBargeInPerceptionGateHasExplicitFieldBypass(t *testing.T) {
	a := &AudioIO{}
	a.SetSpeaking(true)
	t.Setenv("KOE_VPIO_BARGE_IN", "1")
	t.Setenv("KOE_BARGE_PERCEPTION_GATE", "0")
	if a.captureSuppressed() {
		t.Fatal("field bypass must keep capture live without perception evidence")
	}
}

func TestWirelessBargeInNeverOverridesEarconCaptureMute(t *testing.T) {
	a := &AudioIO{}
	t.Setenv("KOE_VPIO_BARGE_IN", "1")
	a.SetSpeaking(true)
	a.earconCaptureMute.Store(true)
	if !a.captureSuppressed() {
		t.Fatal("earcon capture mute must outrank full-duplex barge-in")
	}
	a.earconCaptureMute.Store(false)
	if a.captureSuppressed() {
		t.Fatal("ordinary assistant speech must remain interruptible after the cue")
	}
}

func TestWirelessQwenPlaybackTailSuppressesCapture(t *testing.T) {
	a := &AudioIO{}
	t.Setenv("KOE_VPIO_BARGE_IN", "1")
	a.SetRealtimeProvider(ProviderQwen)
	if got := a.currentRealtimeProvider(); got != string(ProviderQwen) {
		t.Fatalf("realtime provider = %q, want %q", got, ProviderQwen)
	}
	a.SetPlaybackTailProtected(true)
	if !a.captureSuppressed() {
		t.Fatal("Qwen playback tail must suppress Wireless capture")
	}
	a.SetSpeaking(false)
	if a.captureSuppressed() {
		t.Fatal("releasing speaking must release Wireless playback-tail protection")
	}
}

func TestWirelessDisablingPlaybackClearsFloorPause(t *testing.T) {
	a := &AudioIO{playBuf: make(chan []int16, 1)}
	a.playback.Store(true)
	a.SetPlaybackPaused(true)
	a.SetPlaybackEnabled(false)
	if a.PlaybackPaused() {
		t.Fatal("disabling playback must clear the reversible floor pause")
	}
}

func TestWirelessQueuedCaptureIsRegatedAtSendTime(t *testing.T) {
	a := &AudioIO{}
	t.Setenv("KOE_VPIO_BARGE_IN", "1")
	t.Setenv("KOE_BARGE_PERCEPTION_GATE", "1")
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

func TestWirelessPlaybackLevelReturnsIdleAfterFramesDrain(t *testing.T) {
	koeConn, carrierConn := net.Pipe()
	defer carrierConn.Close()
	a, err := NewAudioIO()
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	a.conn = koeConn
	a.wg.Add(1)
	go a.spkPump()

	readDone := make(chan struct{})
	go func() {
		_, _, _ = audiobridge.ReadFrame(carrierConn)
		close(readDone)
	}()
	frame := make([]int16, audioFrameSize)
	for i := range frame {
		frame[i] = 8000
	}
	a.Play(frame)
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for wireless speaker frame")
	}
	if a.PlaybackIdle() {
		t.Fatal("non-silent speaker frame must mark playback active")
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for !a.PlaybackIdle() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !a.PlaybackIdle() {
		t.Fatal("drained wireless playback retained its final RMS level")
	}
	a.Stop()
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

func TestWirelessRepeatedTeardownSendsOneCarrierFlush(t *testing.T) {
	koeConn, carrierConn := net.Pipe()
	defer koeConn.Close()
	defer carrierConn.Close()
	a, err := NewAudioIO()
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	a.conn = koeConn
	a.SetSpeaking(true)
	h := newEventHandler(nil, nil, a, func(any) error { return nil })
	h.respBusy.Store(true)
	h.outputBufferActive.Store(true)

	done := make(chan struct{})
	go func() {
		h.interruptOutput()
		h.interruptOutput()
		close(done)
	}()
	if _, err := readControl(carrierConn); err != nil {
		t.Fatalf("read first carrier flush: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("repeated teardown did not complete")
	}
	_ = carrierConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if body, err := readControl(carrierConn); err == nil {
		t.Fatalf("repeated teardown sent a duplicate carrier flush: %s", body)
	}
}

func TestWirelessPlaybackGainUsesCarrierControl(t *testing.T) {
	koeConn, carrierConn := net.Pipe()
	defer koeConn.Close()
	defer carrierConn.Close()
	a := &AudioIO{conn: koeConn}
	a.playbackGain.Store(math.Float64bits(1))

	got := make(chan []byte, 1)
	go func() {
		body, err := readControl(carrierConn)
		if err == nil {
			got <- body
		}
	}()
	a.SetPlaybackGain(0.35)

	select {
	case body := <-got:
		var msg playbackGainControl
		if err := json.Unmarshal(body, &msg); err != nil {
			t.Fatalf("decode control: %v", err)
		}
		if msg.Type != "playback_gain" || msg.Gain != 0.35 {
			t.Fatalf("playback gain control = %#v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for carrier playback_gain control")
	}
}

func TestWirelessSpeakerWireIsNotDoubleDucked(t *testing.T) {
	koeConn, carrierConn := net.Pipe()
	defer carrierConn.Close()
	a, err := NewAudioIO()
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	a.SetPlaybackGain(defaultBargeSoftDuckGain) // no conn yet: stores target only
	a.conn = koeConn
	a.wg.Add(1)
	go a.spkPump()

	frame := make([]int16, audioFrameSize)
	for i := range frame {
		frame[i] = 12000
	}
	a.Play(frame)
	hdr, payload, err := audiobridge.ReadFrame(carrierConn)
	if err != nil {
		t.Fatalf("read speaker frame: %v", err)
	}
	if hdr.Magic != audiobridge.MagicSpk {
		t.Fatalf("frame magic = %x, want speaker", hdr.Magic)
	}
	peak := int16(0)
	for i := 0; i+1 < len(payload); i += 2 {
		sample := int16(uint16(payload[i]) | uint16(payload[i+1])<<8)
		if sample > peak {
			peak = sample
		}
	}
	if peak < 5000 {
		t.Fatalf("wire peak = %d; Koe appears to have pre-ducked PCM before carrier", peak)
	}
	a.Stop()
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

func TestWirelessMicUsesOfficialProcessedChannel(t *testing.T) {
	stereo := []float32{
		0.80, -0.80,
		0.25, 0.75,
	}
	payload := make([]byte, 0, len(stereo)*4)
	for _, sample := range stereo {
		bits := math.Float32bits(sample)
		payload = append(payload, byte(bits), byte(bits>>8), byte(bits>>16), byte(bits>>24))
	}
	got := decodeToMono(audiobridge.Header{
		Format:   audiobridge.FormatF32LE,
		Channels: 2,
		NSamples: 2,
	}, payload)
	want := []float64{0.80, 0.25}
	if len(got) != len(want) {
		t.Fatalf("decodeToMono len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if math.Abs(got[i]-want[i]) > 1e-6 {
			t.Fatalf("decodeToMono[%d] = %.3f, want channel-0 %.3f", i, got[i], want[i])
		}
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
