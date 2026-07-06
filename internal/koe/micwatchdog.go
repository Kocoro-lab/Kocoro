package koe

import "time"

// MicSilenceAction is the signal the silent-input watchdog wants surfaced to
// Desktop after observing one sample.
type MicSilenceAction int

const (
	// MicSilenceNone: nothing to report this sample.
	MicSilenceNone MicSilenceAction = iota
	// MicSilenceSilent: the bound mic has produced sub-floor input for the whole
	// window while capture was expected — it is effectively dead (clamshell
	// built-in mic with the lid shut, a covered/muted device, an unplugged input).
	MicSilenceSilent
	// MicSilenceRecovered: real input returned after a prior MicSilenceSilent.
	MicSilenceRecovered
)

// MicSilenceState is the pure decision core of the silent-input watchdog. It is
// deliberately side-effect-free (no clock, no audio, no emit) so it unit-tests
// without real hardware or wall-clock: the driver in cmd/koe.go samples the live
// input level on a ticker and feeds each observation in.
//
// Why this exists: in clamshell mode the OS default input stays the built-in mic,
// which is physically covered and delivers pure silence. VPIO starts fine and the
// capture loop forwards ~0-RMS frames forever, so the call looks live but the
// realtime model's VAD never fires and Kocoro never responds. Nothing else in the
// pipeline notices. This watchdog notices and tells the user (see EmitMicStatus).
type MicSilenceState struct {
	silentSince time.Time // zero = not currently in a sub-floor run
	warned      bool      // a MicSilenceSilent was already emitted for this run
}

// Observe advances the state for one sample and returns the signal to emit, if any.
//   - capturing: the call is active AND the mic is expected to hear real input
//     (not user-muted, not gated by Kocoro speaking). When false the silence timer
//     is held reset — a legitimately-muted mic must never trip the warning.
//   - level: the latest captured-frame RMS (0..1).
//   - floor: the RMS below which input counts as "no signal".
//   - window: how long input must stay sub-floor before warning.
func (m *MicSilenceState) Observe(now time.Time, capturing bool, level, floor float64, window time.Duration) MicSilenceAction {
	if !capturing {
		m.silentSince = time.Time{}
		return MicSilenceNone
	}
	if level >= floor {
		// Heard something — reset, and clear a prior warning so the UI can dismiss.
		m.silentSince = time.Time{}
		if m.warned {
			m.warned = false
			return MicSilenceRecovered
		}
		return MicSilenceNone
	}
	// Sub-floor.
	if m.silentSince.IsZero() {
		m.silentSince = now
		return MicSilenceNone
	}
	if !m.warned && now.Sub(m.silentSince) >= window {
		m.warned = true
		return MicSilenceSilent
	}
	return MicSilenceNone
}

// Reset clears the watchdog between calls (a new AudioIO means a fresh call).
func (m *MicSilenceState) Reset() {
	m.silentSince = time.Time{}
	m.warned = false
}

// MicSilenceFloor is the RMS below which input counts as "no signal". Default is
// ~-48 dBFS: ambient room noise on a working mic exceeds it, a covered/dead mic
// does not. Env-tunable for field diagnosis.
func MicSilenceFloor() float64 { return koeEnvFloat("KOE_MIC_SILENCE_FLOOR", 0.004) }

// MicSilenceWindow is how long input must stay sub-floor before the watchdog warns.
func MicSilenceWindow() time.Duration {
	return time.Duration(koeEnvInt("KOE_MIC_SILENCE_MS", 10000)) * time.Millisecond
}
