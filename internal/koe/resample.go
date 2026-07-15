package koe

import "math"

// resamplerTaps is the FIR low-pass length. 127 taps @ 48 kHz work rate gives a
// ~1.3 ms group delay and a transition band narrow enough to hold the 7.6 kHz
// cutoff clear of the 8 kHz Nyquist while pushing the 12 kHz alias test tone deep
// into the Hamming stopband (~-53 dB). WORKLOAD: 20 ms carrier frames, one voice
// stream. SYMPTOM if too small: aliasing leaks / passband droops. OVERRIDE: none
// (fixed 48k↔16k legs); revisit only if the carrier wire rate changes.
const resamplerTaps = 127

// Resampler is a stateful anti-aliasing sample-rate converter for the carrier
// legs (spec §9-b fidelity). It replaces the naive linear resampleLinear, which
// down-rated 48k→16k with no anti-alias low-pass — content above the 8 kHz output
// Nyquist folded back into the voice band as audible aliasing. Kept tagless (pure
// math, no device/cgo coupling) so it unit-tests on any GOOS.
//
// It is a rational L/M converter (upsample by L → FIR low-pass → decimate by M)
// implemented as a running convolution over a retained input history, so calling
// Process once per frame produces the same samples as one call over the whole
// stream (no per-frame boundary transients).
type Resampler struct {
	l, m    int       // upsample factor L, decimate factor M (reduced)
	taps    []float64 // FIR low-pass at L*srcRate, DC-normalized then gain-compensated ×L
	history []float64 // retained input tail, history[0] is global input index baseIn
	baseIn  int       // global input index of history[0]
	outPos  int       // next global output-sample index (phase = outPos*M)
}

// NewResampler builds a resampler from srcRate to dstRate. srcRate==dstRate is a
// pass-through (Process returns its input unchanged).
func NewResampler(srcRate, dstRate int) *Resampler {
	g := gcdInt(srcRate, dstRate)
	r := &Resampler{l: dstRate / g, m: srcRate / g}
	if r.l == 1 && r.m == 1 {
		return r
	}
	fsWork := float64(r.l * srcRate)
	nyq := math.Min(float64(srcRate), float64(dstRate)) / 2
	fc := 0.95 * nyq // hold the cutoff just below the min-Nyquist
	r.taps = lowpassTaps(resamplerTaps, fc/fsWork, r.l)
	return r
}

// Process resamples one block of mono float samples, retaining filter state so
// consecutive calls stream seamlessly.
func (r *Resampler) Process(in []float64) []float64 {
	if r.l == 1 && r.m == 1 {
		return in
	}
	r.history = append(r.history, in...)
	n := len(r.taps)
	var out []float64
	for {
		p := r.outPos * r.m         // work-rate position of this output sample
		hiIn := p / r.l             // highest input index its convolution touches
		if hiIn-r.baseIn >= len(r.history) {
			break // not enough input buffered yet
		}
		var acc float64
		for k := range n {
			pk := p - k
			if pk < 0 {
				break
			}
			if pk%r.l != 0 {
				continue // upsample inserts zeros between input samples
			}
			hi := pk/r.l - r.baseIn
			if hi >= 0 && hi < len(r.history) {
				acc += r.taps[k] * r.history[hi]
			}
		}
		out = append(out, acc)
		r.outPos++
	}
	// Drop history no future output can reach: y[outPos] needs input down to
	// (outPos*M-(N-1))/L. Keep everything from there on.
	if keepFrom := (r.outPos*r.m - (n - 1)) / r.l; keepFrom > r.baseIn {
		drop := min(keepFrom-r.baseIn, len(r.history))
		r.history = r.history[drop:]
		r.baseIn += drop
	}
	return out
}

// lowpassTaps builds a Hamming-windowed sinc low-pass of numTaps taps with cutoff
// fcNorm (cycles/sample at the work rate, 0..0.5). The taps are DC-normalized to
// unity passband gain, then scaled by l to compensate the energy lost to the
// upsample zero-stuffing.
func lowpassTaps(numTaps int, fcNorm float64, l int) []float64 {
	taps := make([]float64, numTaps)
	c := float64(numTaps-1) / 2
	var sum float64
	for i := range taps {
		x := float64(i) - c
		var s float64
		if x == 0 {
			s = 2 * fcNorm
		} else {
			s = math.Sin(2*math.Pi*fcNorm*x) / (math.Pi * x)
		}
		w := 0.54 - 0.46*math.Cos(2*math.Pi*float64(i)/float64(numTaps-1)) // Hamming
		taps[i] = s * w
		sum += taps[i]
	}
	scale := float64(l) / sum
	for i := range taps {
		taps[i] *= scale
	}
	return taps
}

// Reset clears retained filter state so the next Process starts a fresh stream.
// Call between sessions (PrepareForCall) to drop the previous call's tail.
func (r *Resampler) Reset() {
	r.history = r.history[:0]
	r.baseIn = 0
	r.outPos = 0
}

func gcdInt(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	if a < 0 {
		return -a
	}
	return a
}
