package koe

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultGazeFaceHits       = 2
	defaultGazeVADHits        = 2
	defaultGazeFaceHold       = 1500 * time.Millisecond
	defaultGazeArmTimeout     = 8 * time.Second
	defaultGazeRearmCooldown  = 2 * time.Second
	defaultGazeEncounterReset = 2 * time.Second
	defaultGazeHealthGrace    = 1500 * time.Millisecond
)

type GazeState string

const (
	GazeDisabled GazeState = "disabled"
	GazeIdle     GazeState = "idle"
	GazeArming   GazeState = "arming"
	GazeArmed    GazeState = "armed"
	GazeActive   GazeState = "active"
	GazeCooldown GazeState = "cooldown"
	GazeDegraded GazeState = "degraded"
)

type GazeActionKind string

const (
	GazePrepare           GazeActionKind = "prepare"
	GazeActivate          GazeActionKind = "activate"
	GazeCancelPreparation GazeActionKind = "cancel_preparation"
)

type GazeAction struct{ Kind GazeActionKind }

type GazeDecision struct {
	State   GazeState
	Reason  string
	Changed bool
	Actions []GazeAction
}

type GazeConfig struct {
	Enabled        bool
	FaceHits       int
	VADHits        int
	FaceHold       time.Duration
	ArmTimeout     time.Duration
	RearmCooldown  time.Duration
	EncounterReset time.Duration
	HealthGrace    time.Duration
	FrontHalfAngle float64
}

func DefaultGazeConfig() GazeConfig {
	return GazeConfig{
		Enabled:        true,
		FaceHits:       defaultGazeFaceHits,
		VADHits:        defaultGazeVADHits,
		FaceHold:       defaultGazeFaceHold,
		ArmTimeout:     defaultGazeArmTimeout,
		RearmCooldown:  defaultGazeRearmCooldown,
		EncounterReset: defaultGazeEncounterReset,
		HealthGrace:    defaultGazeHealthGrace,
		FrontHalfAngle: math.Pi / 3,
	}
}

// GazeConfigFromEnv resolves bounded developer/rollback overrides. These are
// intentionally not user settings; the product surface eventually selects only
// enabled/disabled while the sensor constants stay engineering-owned.
func GazeConfigFromEnv() (GazeConfig, error) {
	return gazeConfigFromLookup(os.LookupEnv)
}

func gazeConfigFromLookup(lookup func(string) (string, bool)) (GazeConfig, error) {
	cfg := DefaultGazeConfig()
	if raw, ok := lookup("KOE_GAZE_GATE"); ok {
		v, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			return GazeConfig{}, fmt.Errorf("koe[gaze]: KOE_GAZE_GATE must be a boolean")
		}
		cfg.Enabled = v
	}
	ints := []struct {
		name string
		dst  *int
		min  int
		max  int
	}{
		{"KOE_GAZE_FACE_HITS", &cfg.FaceHits, 1, 20},
		{"KOE_GAZE_VAD_HITS", &cfg.VADHits, 1, 20},
	}
	for _, item := range ints {
		if raw, ok := lookup(item.name); ok {
			v, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil || v < item.min || v > item.max {
				return GazeConfig{}, fmt.Errorf("koe[gaze]: %s must be an integer in %d..%d", item.name, item.min, item.max)
			}
			*item.dst = v
		}
	}
	durations := []struct {
		name string
		dst  *time.Duration
		min  int
		max  int
	}{
		{"KOE_GAZE_FACE_HOLD_MS", &cfg.FaceHold, 100, 10_000},
		{"KOE_GAZE_ARM_TIMEOUT_MS", &cfg.ArmTimeout, 1_000, 60_000},
		{"KOE_GAZE_REARM_COOLDOWN_MS", &cfg.RearmCooldown, 100, 10_000},
		{"KOE_GAZE_ENCOUNTER_RESET_MS", &cfg.EncounterReset, 100, 10_000},
		{"KOE_GAZE_HEALTH_GRACE_MS", &cfg.HealthGrace, 100, 10_000},
	}
	for _, item := range durations {
		if raw, ok := lookup(item.name); ok {
			v, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil || v < item.min || v > item.max {
				return GazeConfig{}, fmt.Errorf("koe[gaze]: %s must be an integer in %d..%d", item.name, item.min, item.max)
			}
			*item.dst = time.Duration(v) * time.Millisecond
		}
	}
	if raw, ok := lookup("KOE_GAZE_FRONT_DEG"); ok {
		v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil || !finite(v) || v < 1 || v > 89 {
			return GazeConfig{}, fmt.Errorf("koe[gaze]: KOE_GAZE_FRONT_DEG must be a number in 1..89")
		}
		cfg.FrontHalfAngle = v * math.Pi / 180
	}
	if err := cfg.validate(); err != nil {
		return GazeConfig{}, err
	}
	return cfg, nil
}

func (c GazeConfig) validate() error {
	if c.FaceHits < 1 || c.FaceHits > 20 || c.VADHits < 1 || c.VADHits > 20 {
		return fmt.Errorf("koe[gaze]: face/vad hit counts must be in 1..20")
	}
	if c.FaceHold <= 0 || c.ArmTimeout <= 0 || c.RearmCooldown <= 0 || c.EncounterReset <= 0 || c.HealthGrace <= 0 {
		return fmt.Errorf("koe[gaze]: durations must be positive")
	}
	if !finite(c.FrontHalfAngle) || c.FrontHalfAngle <= 0 || c.FrontHalfAngle >= math.Pi/2 {
		return fmt.Errorf("koe[gaze]: front half-angle must be between 0 and pi/2")
	}
	return nil
}

type GazeInput struct {
	Now        time.Time
	Snapshot   PerceptionSnapshot
	Prepared   bool
	CallActive bool
}

// GazeGate is the pure Wireless IDLE trigger policy. It owns no I/O: callers
// execute the returned prepare/activate/cancel actions and feed the resulting
// runtime state back on the next sample.
type GazeGate struct {
	cfg   GazeConfig
	state GazeState

	faceHits         int
	vadHits          int
	lastFaceSeen     time.Time
	absentSince      time.Time
	armDeadline      time.Time
	cooldownUntil    time.Time
	encounterBlocked bool
	activateIssued   bool
	wasCallActive    bool
	unhealthySince   time.Time
}

func NewGazeGate(cfg GazeConfig) (*GazeGate, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	state := GazeIdle
	if !cfg.Enabled {
		state = GazeDisabled
	}
	return &GazeGate{cfg: cfg, state: state}, nil
}

func (g *GazeGate) State() GazeState { return g.state }

func (g *GazeGate) Update(in GazeInput) GazeDecision {
	now := in.Now
	if now.IsZero() {
		now = in.Snapshot.ObservedAt
	}
	if now.IsZero() {
		now = time.Now()
	}

	if !g.cfg.Enabled {
		actions := g.cancelIfPreparing()
		return g.transition(GazeDisabled, "disabled", actions...)
	}

	// Active conversation state outranks sensor health. Perception only opens an
	// IDLE call; it never owns hang-up or mute after activation.
	if in.CallActive {
		g.wasCallActive = true
		g.activateIssued = false
		return g.transition(GazeActive, "call_active")
	}
	if g.wasCallActive {
		g.wasCallActive = false
		g.cooldownUntil = now.Add(g.cfg.RearmCooldown)
		g.encounterBlocked = true
		g.resetEvidence()
		return g.transition(GazeCooldown, "call_ended")
	}

	if !in.Snapshot.Healthy() {
		// A single loaded-daemon HTTP timeout must not tear down a session that
		// was just prepared for the same face. During this bounded grace period
		// all speech evidence is discarded, so stale DOA can never activate a
		// call. Other health failures already represent sustained/invalid sensor
		// state and continue to fail closed immediately.
		if in.Snapshot.Health == PerceptionDaemonUnreachable &&
			(g.state == GazeArming || g.state == GazeArmed) {
			if g.unhealthySince.IsZero() {
				g.unhealthySince = now
			}
			g.vadHits = 0
			if now.Sub(g.unhealthySince) < g.cfg.HealthGrace {
				return GazeDecision{State: g.state, Reason: "perception_gap"}
			}
			g.encounterBlocked = true
		}
		actions := g.cancelIfPreparing()
		g.resetEvidence()
		return g.transition(GazeDegraded, string(in.Snapshot.Health), actions...)
	}
	g.unhealthySince = time.Time{}
	if g.state == GazeDegraded {
		g.resetEvidence()
		return g.transition(GazeIdle, "perception_recovered")
	}

	g.updateEncounter(now, in.Snapshot.Face.Detected)
	if in.Snapshot.Face.Detected {
		g.faceHits++
		g.lastFaceSeen = now
	} else {
		g.faceHits = 0
	}

	if g.state == GazeCooldown {
		if now.Before(g.cooldownUntil) {
			return GazeDecision{State: g.state}
		}
		return g.transition(GazeIdle, "cooldown_complete")
	}

	if g.state == GazeIdle {
		if g.encounterBlocked || g.faceHits < g.cfg.FaceHits {
			return GazeDecision{State: g.state}
		}
		g.armDeadline = now.Add(g.cfg.ArmTimeout)
		g.activateIssued = false
		g.vadHits = 0
		return g.transition(GazeArming, "face_latched", GazeAction{Kind: GazePrepare})
	}

	if g.state != GazeArming && g.state != GazeArmed {
		return GazeDecision{State: g.state}
	}
	if !g.armDeadline.IsZero() && now.After(g.armDeadline) {
		g.encounterBlocked = true
		g.resetEvidence()
		return g.transition(GazeIdle, "arm_timeout", GazeAction{Kind: GazeCancelPreparation})
	}
	if g.lastFaceSeen.IsZero() || now.Sub(g.lastFaceSeen) > g.cfg.FaceHold {
		// Treat a failed preparation as one consumed encounter. YuNet can flicker
		// around the detection threshold; without this latch the same stationary
		// face immediately collects FaceHits again and repeatedly mints warm
		// Realtime sessions. A continuous EncounterReset absence clears the latch
		// in updateEncounter, so a person who genuinely leaves and returns can try
		// again.
		g.encounterBlocked = true
		g.resetEvidence()
		return g.transition(GazeIdle, "face_lost", GazeAction{Kind: GazeCancelPreparation})
	}
	armedNow := in.Prepared && g.state == GazeArming
	if armedNow {
		g.state = GazeArmed
	}

	front := math.Abs(in.Snapshot.DOA.Angle-math.Pi/2) <= g.cfg.FrontHalfAngle
	if in.Snapshot.DOA.SpeechDetected && front {
		g.vadHits++
	} else {
		g.vadHits = 0
	}
	if g.vadHits >= g.cfg.VADHits && !g.activateIssued {
		g.activateIssued = true
		return GazeDecision{State: g.state, Reason: "speech_front", Changed: armedNow, Actions: []GazeAction{{Kind: GazeActivate}}}
	}
	if armedNow {
		return GazeDecision{State: g.state, Reason: "session_prepared", Changed: true}
	}
	return GazeDecision{State: g.state}
}

func (g *GazeGate) updateEncounter(now time.Time, detected bool) {
	if detected {
		g.absentSince = time.Time{}
		return
	}
	if g.absentSince.IsZero() {
		g.absentSince = now
		return
	}
	if g.encounterBlocked && now.Sub(g.absentSince) >= g.cfg.EncounterReset {
		g.encounterBlocked = false
	}
}

func (g *GazeGate) resetEvidence() {
	g.faceHits = 0
	g.vadHits = 0
	g.lastFaceSeen = time.Time{}
	g.armDeadline = time.Time{}
	g.activateIssued = false
	g.unhealthySince = time.Time{}
}

func (g *GazeGate) cancelIfPreparing() []GazeAction {
	if g.state == GazeArming || g.state == GazeArmed {
		return []GazeAction{{Kind: GazeCancelPreparation}}
	}
	return nil
}

func (g *GazeGate) transition(state GazeState, reason string, actions ...GazeAction) GazeDecision {
	changed := g.state != state
	g.state = state
	return GazeDecision{State: state, Reason: reason, Changed: changed, Actions: actions}
}
