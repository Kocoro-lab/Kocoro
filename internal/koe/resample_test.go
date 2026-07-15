package koe

import (
	"math"
	"testing"
)

// goertzelMag returns the amplitude of a single frequency component in x,
// normalized so a pure cosine of amplitude A reads back ~A. Used to probe for
// aliasing (energy at a fold-back frequency that should not exist) and passband
// fidelity (energy at an input tone that must survive).
func goertzelMag(x []float64, freq float64, fs int) float64 {
	if len(x) == 0 {
		return 0
	}
	w := 2 * math.Pi * freq / float64(fs)
	cw := math.Cos(w)
	sw := math.Sin(w)
	coeff := 2 * cw
	var s0, s1, s2 float64
	for _, v := range x {
		s0 = v + coeff*s1 - s2
		s2 = s1
		s1 = s0
	}
	re := s1 - s2*cw
	im := s2 * sw
	return 2 * math.Hypot(re, im) / float64(len(x))
}

func sineWave(freq float64, amp float64, fs, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = amp * math.Sin(2*math.Pi*freq*float64(i)/float64(fs))
	}
	return out
}

// A 12 kHz tone at 48k is above the 8 kHz output Nyquist; a correct downsampler
// low-passes it away. The naive linear resampler instead lets it fold back to
// |16k-12k| = 4 kHz at full amplitude — audible aliasing in the voice band. The
// resampler must suppress that 4 kHz fold-back product.
func TestResamplerSuppressesDownsampleAliasing(t *testing.T) {
	const src, dst = 48000, 16000
	in := sineWave(12000, 1.0, src, src) // 1 s of a 12 kHz tone
	out := NewResampler(src, dst).Process(in)
	alias := goertzelMag(out, 4000, dst) // fold-back frequency
	if alias > 0.05 {
		t.Fatalf("12kHz@48k → 16k: 4kHz alias amplitude = %.4f, want < 0.05 "+
			"(naive folds the tone back near 1.0)", alias)
	}
}

// A 1 kHz voice-band tone must survive the 48k→16k leg at its input level: the
// anti-alias filter must not droop the passband it is meant to preserve.
func TestResamplerPreservesPassbandLevel(t *testing.T) {
	const src, dst = 48000, 16000
	in := sineWave(1000, 0.5, src, src)
	out := NewResampler(src, dst).Process(in)
	mag := goertzelMag(out, 1000, dst)
	if mag < 0.47 || mag > 0.53 {
		t.Fatalf("1kHz@48k → 16k passband amplitude = %.4f, want ~0.5 (±0.6 dB)", mag)
	}
}

// The mic leg up-rates 16k→48k. Upsampling replicates the spectrum; the FIR must
// remove the image so a 1 kHz tone does not spawn a 15 kHz (16k-1k) image, while
// the 1 kHz passband itself survives.
func TestResamplerUpsampleSuppressesImaging(t *testing.T) {
	const src, dst = 16000, 48000
	in := sineWave(1000, 0.5, src, src)
	out := NewResampler(src, dst).Process(in)
	if mag := goertzelMag(out, 1000, dst); mag < 0.47 || mag > 0.53 {
		t.Fatalf("1kHz@16k → 48k passband amplitude = %.4f, want ~0.5", mag)
	}
	if image := goertzelMag(out, 15000, dst); image > 0.05 {
		t.Fatalf("1kHz@16k → 48k: 15kHz image amplitude = %.4f, want < 0.05", image)
	}
}

// Real use calls Process once per ~20 ms frame. Feeding the same stream in chunks
// must yield exactly the samples one whole-stream call produces — otherwise the
// retained filter state has a frame-boundary bug.
func TestResamplerStreamingMatchesWholeStream(t *testing.T) {
	const src, dst = 48000, 16000
	in := sineWave(1000, 0.5, src, src/2) // 0.5 s
	whole := NewResampler(src, dst).Process(append([]float64(nil), in...))

	r := NewResampler(src, dst)
	var streamed []float64
	for i := 0; i < len(in); i += 320 {
		end := min(i+320, len(in))
		streamed = append(streamed, r.Process(in[i:end])...)
	}
	if len(whole) != len(streamed) {
		t.Fatalf("streaming produced %d samples, whole-stream produced %d", len(streamed), len(whole))
	}
	var maxDiff float64
	for i := range whole {
		if d := math.Abs(whole[i] - streamed[i]); d > maxDiff {
			maxDiff = d
		}
	}
	if maxDiff > 1e-9 {
		t.Fatalf("streaming diverges from whole-stream: max sample diff = %.2e", maxDiff)
	}
}

// Reset must drop retained filter state so a reused resampler behaves like a fresh
// one — otherwise a new call inherits the previous call's tail (leading glitch).
func TestResamplerResetRestartsStream(t *testing.T) {
	const src, dst = 48000, 16000
	in := sineWave(1000, 0.5, src, src/4)
	r := NewResampler(src, dst)
	r.Process(in) // dirty the state with a prior stream
	r.Reset()
	after := r.Process(append([]float64(nil), in...))
	fresh := NewResampler(src, dst).Process(append([]float64(nil), in...))
	if len(after) != len(fresh) {
		t.Fatalf("after Reset produced %d samples, fresh resampler %d (state not cleared)", len(after), len(fresh))
	}
	for i := range fresh {
		if math.Abs(after[i]-fresh[i]) > 1e-12 {
			t.Fatalf("after Reset sample %d = %.6f, fresh = %.6f (state not cleared)", i, after[i], fresh[i])
		}
	}
}
