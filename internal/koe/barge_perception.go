package koe

import (
	"math"
	"time"
)

const (
	// The XVF3800 still emits isolated speech_detected pulses for residual speaker
	// echo even after its AEC reports converged. Requiring three consecutive 100 ms
	// samples rejects those pulses while keeping a normal interruption sub-second.
	defaultBargePerceptionHits = 3
	// Once sustained near-end speech opens the gate, keep it open long enough for
	// Realtime server VAD to observe the utterance. The XVF DOA endpoint can briefly
	// report no sample during double-talk; that transport gap must not chop an
	// already-authorized speaker back into isolated tail fragments.
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
	hits            int
	authorized      bool
	authorizedUntil time.Time
}

func NewBargePerceptionGate() *BargePerceptionGate { return &BargePerceptionGate{} }

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
	if !snapshot.DOA.Available || !snapshot.DOA.Fresh {
		g.hits = 0
		if g.authorized && !g.authorizedUntil.IsZero() && now.Before(g.authorizedUntil) {
			return BargePerceptionDecision{Authorized: true, Reason: "holding_doa_gap"}
		}
		return g.reset("doa_unavailable")
	}

	front := math.Abs(snapshot.DOA.Angle-math.Pi/2) <= defaultBargeFrontHalfAngle
	if snapshot.DOA.SpeechDetected && front {
		g.hits++
	} else {
		g.hits = 0
	}
	if g.hits >= defaultBargePerceptionHits {
		g.authorizedUntil = now.Add(defaultBargePerceptionHold)
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
	return BargePerceptionDecision{Authorized: authorized, Changed: changed, Reason: reason, Hits: g.hits}
}

func (g *BargePerceptionGate) reset(reason string) BargePerceptionDecision {
	changed := g.authorized
	g.hits = 0
	g.authorized = false
	g.authorizedUntil = time.Time{}
	return BargePerceptionDecision{Changed: changed, Reason: reason}
}
