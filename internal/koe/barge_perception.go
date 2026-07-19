package koe

import (
	"math"
	"time"
)

const (
	// The XVF3800 still emits isolated speech_detected pulses for residual speaker
	// echo after AEC. Two front-directed samples within a sub-second window reject
	// those isolated pulses while opening quickly enough for normal talk-over.
	// Wall-time evidence tolerates an occasional no-speech poll between real hits.
	defaultBargePerceptionHits   = 2
	defaultBargePerceptionWindow = 800 * time.Millisecond
	// Once sustained near-end speech opens the gate, keep it open long enough for
	// the local energy gate and Realtime server VAD to observe the utterance.
	defaultBargePerceptionHold = 3 * time.Second
	defaultBargeFrontHalfAngle = math.Pi / 3
)

// BargePerceptionDecision is the latest authorization applied to the Wireless
// capture path. Hits is diagnostic evidence, not a user-facing confidence score.
type BargePerceptionDecision struct {
	Authorized bool
	Changed    bool
	Reason     string
	Hits       int
}

// BargePerceptionGate is the pure time-series policy that distinguishes sustained
// near-end speech from the XVF3800's short residual-echo VAD pulses. It owns no I/O;
// the resident runtime feeds it the same robot-local DOA stream as the IDLE gaze
// gate and applies Authorized to the current AudioIO.
type BargePerceptionGate struct {
	hitTimes        []time.Time
	authorized      bool
	authorizedUntil time.Time
	requiredHits    int
	window          time.Duration
	hold            time.Duration
}

func NewBargePerceptionGate() *BargePerceptionGate {
	hits := koeEnvInt("KOE_BARGE_PERCEPTION_HITS", defaultBargePerceptionHits)
	if hits < 1 {
		hits = 1
	}
	window := time.Duration(koeEnvInt("KOE_BARGE_PERCEPTION_WINDOW_MS", int(defaultBargePerceptionWindow/time.Millisecond))) * time.Millisecond
	if window < time.Millisecond {
		window = time.Millisecond
	}
	hold := time.Duration(koeEnvInt("KOE_BARGE_PERCEPTION_HOLD_MS", int(defaultBargePerceptionHold/time.Millisecond))) * time.Millisecond
	if hold < time.Millisecond {
		hold = time.Millisecond
	}
	return &BargePerceptionGate{
		requiredHits: hits,
		window:       window,
		hold:         hold,
	}
}

func (g *BargePerceptionGate) Update(now time.Time, callActive, speaking bool, snapshot PerceptionSnapshot) BargePerceptionDecision {
	if now.IsZero() {
		now = snapshot.ObservedAt
	}
	if now.IsZero() {
		now = time.Now()
	}

	if !callActive || !speaking {
		reason := "call_inactive"
		if callActive {
			reason = "not_speaking"
		}
		return g.reset(reason)
	}
	g.pruneHits(now)
	if !snapshot.DOA.Available || !snapshot.DOA.Fresh {
		if g.authorized && !g.authorizedUntil.IsZero() && now.Before(g.authorizedUntil) {
			return BargePerceptionDecision{Authorized: true, Reason: "holding_doa_gap", Hits: len(g.hitTimes)}
		}
		changed := g.authorized
		g.authorized = false
		g.authorizedUntil = time.Time{}
		return BargePerceptionDecision{Changed: changed, Reason: "doa_unavailable", Hits: len(g.hitTimes)}
	}

	front := math.Abs(snapshot.DOA.Angle-math.Pi/2) <= defaultBargeFrontHalfAngle
	if snapshot.DOA.SpeechDetected && front {
		g.hitTimes = append(g.hitTimes, now)
	} else if snapshot.DOA.SpeechDetected {
		// A positive sample from outside the front cone is conflicting evidence,
		// not a transport/VAD gap. Start a fresh candidate utterance.
		g.hitTimes = nil
	}
	if len(g.hitTimes) >= g.requiredHits {
		g.authorizedUntil = now.Add(g.hold)
	}
	authorized := !g.authorizedUntil.IsZero() && now.Before(g.authorizedUntil)
	reason := "collecting"
	if authorized {
		reason = "speech_front_sustained"
	} else if !front {
		reason = "outside_front"
	} else if !snapshot.DOA.SpeechDetected {
		reason = "no_speech"
	}
	changed := authorized != g.authorized
	g.authorized = authorized
	return BargePerceptionDecision{Authorized: authorized, Changed: changed, Reason: reason, Hits: len(g.hitTimes)}
}

func (g *BargePerceptionGate) pruneHits(now time.Time) {
	cutoff := now.Add(-g.window)
	keep := 0
	for keep < len(g.hitTimes) && g.hitTimes[keep].Before(cutoff) {
		keep++
	}
	if keep > 0 {
		g.hitTimes = append(g.hitTimes[:0], g.hitTimes[keep:]...)
	}
}

func (g *BargePerceptionGate) reset(reason string) BargePerceptionDecision {
	changed := g.authorized
	g.hitTimes = nil
	g.authorized = false
	g.authorizedUntil = time.Time{}
	return BargePerceptionDecision{Changed: changed, Reason: reason}
}
