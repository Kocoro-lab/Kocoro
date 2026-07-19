package koe

const (
	// Soft duck is the immediate, reversible "I hear you" response to strong
	// robot-local speech evidence. Thirty-five percent is clearly audible as a
	// level change without making a false positive feel like playback vanished.
	defaultBargeSoftDuckGain = 0.35
	// Deep duck follows independent server-VAD confirmation. It yields the sound
	// field while the auxiliary transcript decides between a real interruption,
	// a backchannel, and residual echo. Five percent behaved like an abrupt mute
	// at volume 100; fifteen percent keeps continuity until the hard flush.
	defaultBargeDuckGain = 0.15
)

func clampPlaybackGain(gain float64) float64 {
	if gain < 0 {
		return 0
	}
	if gain > 1 {
		return 1
	}
	return gain
}

func scalePCM(pcm []int16, gain float64) []int16 {
	gain = clampPlaybackGain(gain)
	if gain >= 1 {
		return pcm
	}
	scaled := make([]int16, len(pcm))
	for i, sample := range pcm {
		scaled[i] = int16(float64(sample) * gain)
	}
	return scaled
}
