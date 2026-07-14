package koe

import (
	"math"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/koe/audiobridge"
)

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

func TestToCarrierPCMDownratesToWireRate(t *testing.T) {
	// 48k → 16k is a 3:1 decimation: N input samples → N/3 output samples.
	in := make([]int16, audioFrameSize) // 960 samples @ 48k = 20 ms
	got := toCarrierPCM(in)
	want := audioFrameSize * carrierWireRate / audioSampleRate // 960*16000/48000 = 320
	if len(got) != want {
		t.Fatalf("toCarrierPCM len = %d, want %d (48k→16k on a 20ms frame)", len(got), want)
	}
}

func TestToCarrierPCMPreservesDCLevel(t *testing.T) {
	// A constant (DC) signal must survive resampling unchanged — catches a stub
	// that drops the resample or corrupts amplitude.
	const level int16 = 8000
	in := make([]int16, audioFrameSize)
	for i := range in {
		in[i] = level
	}
	got := toCarrierPCM(in)
	for i, v := range got {
		if math.Abs(float64(v)-float64(level)) > 2 { // ±2 for float round-trip
			t.Fatalf("sample %d = %d, want ~%d (DC must survive down-rate)", i, v, level)
		}
	}
}

// A frame framed after toCarrierPCM must declare the wire rate in its header so the
// carrier and the audiobridge PayloadLen check agree.
func TestSpkFrameHeaderDeclaresWireRate(t *testing.T) {
	pcm := toCarrierPCM(make([]int16, audioFrameSize))
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
