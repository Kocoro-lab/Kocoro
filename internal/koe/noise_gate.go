package koe

import (
	"log"
	"math"
	"os"
)

const (
	// WORKLOAD: far-field laptop mic, quiet/long user utterances. A higher fixed
	// floor or short hangover made the local gate stop RTP before server_vad could
	// endpoint, so the user had to say a second phrase. OVERRIDE: KOE_MIC_GATE_*.
	defaultMicGateThreshold       = 0.010
	defaultMicGateSoftThreshold   = 0.006
	defaultMicGateNoiseMultiplier = 2.0
	defaultMicGateStartMS         = 160
	defaultMicGateSoftStartMS     = 480
	defaultMicGateHangoverMS      = 2000
	defaultMicGateEndpointMS      = 2000
	micGateHotEvidenceWeight      = 2
	micGateNoiseAlpha             = 0.04
)

type micGateStats struct {
	PassedFrames    uint64
	MutedFrames     uint64
	SpeechStarts    uint64
	MaxLevel        float64
	NoiseFloor      float64
	HotFramesMax    int
	SoftFramesMax   int
	StartScoreMax   int
	StartFrames     int
	SoftStartFrames int
}

type micNoiseGate struct {
	enabled bool

	threshold       float64
	softThreshold   float64
	noiseMultiplier float64
	startFrames     int
	softStartFrames int
	hangoverFrames  int
	endpointFrames  int

	noiseFloor float64
	maxLevel   float64
	hotFrames  int
	softFrames int
	startScore int
	hangover   int
	endpoint   int
	open       bool
	pending    [][]int16

	zero []int16

	stats micGateStats
}

func newMicNoiseGate() *micNoiseGate {
	return &micNoiseGate{
		enabled:         !koeEnvBool("KOE_MIC_GATE_OFF", false),
		threshold:       koeEnvFloat("KOE_MIC_GATE_THRESHOLD", defaultMicGateThreshold),
		softThreshold:   koeEnvFloat("KOE_MIC_GATE_SOFT_THRESHOLD", defaultMicGateSoftThreshold),
		noiseMultiplier: koeEnvFloat("KOE_MIC_GATE_NOISE_MULTIPLIER", defaultMicGateNoiseMultiplier),
		startFrames:     msToAudioFrames(koeEnvInt("KOE_MIC_GATE_START_MS", defaultMicGateStartMS)),
		softStartFrames: msToAudioFrames(koeEnvInt("KOE_MIC_GATE_SOFT_START_MS", defaultMicGateSoftStartMS)),
		hangoverFrames:  msToAudioFrames(koeEnvInt("KOE_MIC_GATE_HANGOVER_MS", defaultMicGateHangoverMS)),
		endpointFrames:  msToAudioFrames(koeEnvInt("KOE_MIC_GATE_ENDPOINT_MS", defaultMicGateEndpointMS)),
		zero:            make([]int16, audioFrameSize),
	}
}

func msToAudioFrames(ms int) int {
	frames := (ms + audioFrameMs - 1) / audioFrameMs
	if frames < 1 {
		return 1
	}
	return frames
}

func (g *micNoiseGate) process(frame []int16) [][]int16 {
	if !g.enabled {
		g.stats.PassedFrames++
		return [][]int16{frame}
	}
	level := rmsLevel(frame)
	if level > g.maxLevel {
		g.maxLevel = level
	}
	threshold := math.Max(g.threshold, g.noiseFloor*g.noiseMultiplier)
	softThreshold := math.Max(g.softThreshold, g.noiseFloor*g.noiseMultiplier)
	hot := level >= threshold
	soft := !hot && level >= softThreshold

	if g.open {
		if hot || soft {
			g.hangover = g.hangoverFrames
		} else {
			g.updateNoiseFloorIfAmbient(level)
			g.hangover--
			if g.hangover <= 0 {
				g.open = false
				g.hotFrames = 0
				g.softFrames = 0
				g.startScore = 0
				g.pending = g.pending[:0]
				g.endpoint = g.endpointFrames
			}
		}
		if g.open {
			g.stats.PassedFrames++
			return [][]int16{frame}
		}
	}

	// Real speech often has low-energy consonant gaps; score evidence lets those
	// gaps decay gradually instead of resetting the start window to zero.
	if hot {
		g.endpoint = 0
		g.hotFrames++
		g.softFrames = 0
		g.startScore += micGateHotEvidenceWeight
		if g.startScore > g.startFrames {
			g.startScore = g.startFrames
		}
		if g.hotFrames > g.stats.HotFramesMax {
			g.stats.HotFramesMax = g.hotFrames
		}
	} else if soft {
		// Low-volume speech gets a slower continuous path instead of lowering the
		// fixed hot threshold, which proved too eager for speaker bleed.
		g.endpoint = 0
		g.hotFrames = 0
		g.softFrames++
		if g.softFrames > g.stats.SoftFramesMax {
			g.stats.SoftFramesMax = g.softFrames
		}
		if g.startScore > 0 {
			g.startScore--
		}
	} else {
		g.hotFrames = 0
		g.softFrames = 0
		if g.startScore > 0 {
			g.startScore--
		}
		if g.startScore == 0 && g.softFrames == 0 {
			g.pending = g.pending[:0]
		}
		g.updateNoiseFloorIfAmbient(level)
	}
	if g.startScore > g.stats.StartScoreMax {
		g.stats.StartScoreMax = g.startScore
	}
	if g.startScore > 0 || g.softFrames > 0 {
		g.pending = append(g.pending, append([]int16(nil), frame...))
		pendingCap := g.startFrames
		if g.softStartFrames > pendingCap {
			pendingCap = g.softStartFrames
		}
		if len(g.pending) > pendingCap {
			g.pending = g.pending[len(g.pending)-pendingCap:]
		}
	}
	if g.startScore >= g.startFrames || g.softFrames >= g.softStartFrames {
		g.open = true
		g.hangover = g.hangoverFrames
		g.stats.SpeechStarts++
		out := append([][]int16(nil), g.pending...)
		g.pending = g.pending[:0]
		g.stats.PassedFrames += uint64(len(out))
		return out
	}

	g.stats.MutedFrames++
	if g.endpoint > 0 {
		g.endpoint--
		return [][]int16{g.zero}
	}
	return [][]int16{g.zero}
}

func (g *micNoiseGate) resetState() {
	g.hotFrames = 0
	g.softFrames = 0
	g.startScore = 0
	g.hangover = 0
	g.endpoint = 0
	g.open = false
	g.pending = g.pending[:0]
}

func (g *micNoiseGate) updateNoiseFloorIfAmbient(level float64) {
	if level >= g.threshold {
		return
	}
	g.updateNoiseFloor(level)
}

func (g *micNoiseGate) updateNoiseFloor(level float64) {
	if level <= 0 {
		return
	}
	if g.noiseFloor == 0 {
		g.noiseFloor = level
		return
	}
	g.noiseFloor = (1-micGateNoiseAlpha)*g.noiseFloor + micGateNoiseAlpha*level
}

func (g *micNoiseGate) logStats() {
	if os.Getenv("KOE_AUDIO_LOG") != "1" && os.Getenv("KOE_EVENT_LOG") != "1" {
		return
	}
	g.stats.MaxLevel = g.maxLevel
	g.stats.NoiseFloor = g.noiseFloor
	g.stats.StartFrames = g.startFrames
	g.stats.SoftStartFrames = g.softStartFrames
	log.Printf("koe[audio]: mic gate stats: %+v", g.stats)
}
