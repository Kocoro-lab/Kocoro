package koe

import (
	"math"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/koe/audiobridge"
)

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
