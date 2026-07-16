package koe

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// Open modes — how a Wireless conversation is opened. This is a product setting
// (the Desktop robot card exposes it), not a developer knob: "trigger" is the
// shipped default and keeps the existing manual/gaze behaviour, while "standby"
// is the explicit opt-in resident-listen mode. Only reachy_wireless honours it.
const (
	OpenModeTrigger = "trigger"
	OpenModeStandby = "standby"
)

const (
	defaultStandbyVADHits    = 2
	defaultStandbyCooldown   = 5 * time.Second
	defaultStandbyIdleHangup = 25 * time.Second
)

// ParseOpenMode validates an open-mode token fail-loud. Empty resolves to the
// shipped default so an unset environment is byte-identical to today.
func ParseOpenMode(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return OpenModeTrigger, nil
	case OpenModeTrigger:
		return OpenModeTrigger, nil
	case OpenModeStandby:
		return OpenModeStandby, nil
	default:
		return "", fmt.Errorf("koe[standby]: KOE_OPEN_MODE must be %q or %q", OpenModeTrigger, OpenModeStandby)
	}
}

// OpenModeFromEnv resolves KOE_OPEN_MODE. The caller injects the result into the
// carrier profile, which is what actually gates the mode (non-Wireless carriers
// stay on trigger regardless).
func OpenModeFromEnv() (string, error) { return ParseOpenMode(os.Getenv("KOE_OPEN_MODE")) }

// StandbyConfig is the resident-listen policy. Enabled is derived from the
// carrier's open mode by the caller; the sensor constants stay engineering-owned
// and are only overridable for field tuning.
type StandbyConfig struct {
	Enabled        bool
	VADHits        int
	FrontHalfAngle float64
	Cooldown       time.Duration
	IdleHangup     time.Duration
}

func DefaultStandbyConfig() StandbyConfig {
	return StandbyConfig{
		Enabled:        false,
		VADHits:        defaultStandbyVADHits,
		FrontHalfAngle: math.Pi / 3,
		Cooldown:       defaultStandbyCooldown,
		IdleHangup:     defaultStandbyIdleHangup,
	}
}

// StandbyConfigFromEnv resolves the bounded overrides. Enabled is NOT read here:
// the mode comes from the carrier profile, so a stray KOE_STANDBY_* value can
// never switch a trigger-mode robot into resident listening.
func StandbyConfigFromEnv() (StandbyConfig, error) {
	return standbyConfigFromLookup(os.LookupEnv)
}

func standbyConfigFromLookup(lookup func(string) (string, bool)) (StandbyConfig, error) {
	cfg := DefaultStandbyConfig()
	if raw, ok := lookup("KOE_STANDBY_VAD_HITS"); ok {
		v, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || v < 1 || v > 20 {
			return StandbyConfig{}, fmt.Errorf("koe[standby]: KOE_STANDBY_VAD_HITS must be an integer in 1..20")
		}
		cfg.VADHits = v
	}
	if raw, ok := lookup("KOE_STANDBY_FRONT_DEG"); ok {
		v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil || !finite(v) || v < 1 || v > 89 {
			return StandbyConfig{}, fmt.Errorf("koe[standby]: KOE_STANDBY_FRONT_DEG must be a number in 1..89")
		}
		cfg.FrontHalfAngle = v * math.Pi / 180
	}
	if raw, ok := lookup("KOE_STANDBY_COOLDOWN_MS"); ok {
		v, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || v < 100 || v > 60_000 {
			return StandbyConfig{}, fmt.Errorf("koe[standby]: KOE_STANDBY_COOLDOWN_MS must be an integer in 100..60000")
		}
		cfg.Cooldown = time.Duration(v) * time.Millisecond
	}
	if raw, ok := lookup("KOE_STANDBY_IDLE_HANGUP_S"); ok {
		v, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || v < 5 || v > 600 {
			return StandbyConfig{}, fmt.Errorf("koe[standby]: KOE_STANDBY_IDLE_HANGUP_S must be an integer in 5..600")
		}
		cfg.IdleHangup = time.Duration(v) * time.Second
	}
	if err := cfg.validate(); err != nil {
		return StandbyConfig{}, err
	}
	return cfg, nil
}

func (c StandbyConfig) validate() error {
	if c.VADHits < 1 || c.VADHits > 20 {
		return fmt.Errorf("koe[standby]: vad hit count must be in 1..20")
	}
	if c.Cooldown <= 0 || c.IdleHangup <= 0 {
		return fmt.Errorf("koe[standby]: durations must be positive")
	}
	if !finite(c.FrontHalfAngle) || c.FrontHalfAngle <= 0 || c.FrontHalfAngle >= math.Pi/2 {
		return fmt.Errorf("koe[standby]: front half-angle must be between 0 and pi/2")
	}
	return nil
}

type StandbyState string

const (
	StandbyDisabled StandbyState = "disabled"
	StandbyIdle     StandbyState = "idle"
	StandbyActive   StandbyState = "active"
	StandbyCooldown StandbyState = "cooldown"
	StandbyDegraded StandbyState = "degraded"
)

type StandbyActionKind string

// StandbyActivate asks the runtime to open a call. Standby, like the gaze gate,
// only ever OPENS a call from idle; hang-up is owned by the idle gate, the
// end_call tool, or Desktop.
const StandbyActivate StandbyActionKind = "activate"

type StandbyAction struct{ Kind StandbyActionKind }

type StandbyDecision struct {
	State   StandbyState
	Reason  string
	Changed bool
	Actions []StandbyAction
}

type StandbyInput struct {
	Now        time.Time
	Snapshot   PerceptionSnapshot
	CallActive bool
}

// StandbyGate is the pure resident-listen trigger policy: speaking toward the
// robot's front opens the conversation. Unlike the gaze gate it has no face
// condition and no preparation phase — standby keeps the Realtime session hot,
// so activation is a single step. It owns no I/O; the resident runtime feeds it
// the same robot-local perception stream and executes the returned actions.
//
// The room is never uploaded while idle: this gate reads only the robot-local
// XVF DOA/VAD snapshot, and call audio capture starts inside startCall.
type StandbyGate struct {
	cfg   StandbyConfig
	state StandbyState

	vadHits        int
	cooldownUntil  time.Time
	activateIssued bool
	wasCallActive  bool
}

func NewStandbyGate(cfg StandbyConfig) (*StandbyGate, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	state := StandbyIdle
	if !cfg.Enabled {
		state = StandbyDisabled
	}
	return &StandbyGate{cfg: cfg, state: state}, nil
}

func (g *StandbyGate) State() StandbyState { return g.state }

func (g *StandbyGate) Update(in StandbyInput) StandbyDecision {
	now := in.Now
	if now.IsZero() {
		now = in.Snapshot.ObservedAt
	}
	if now.IsZero() {
		now = time.Now()
	}

	if !g.cfg.Enabled {
		return g.transition(StandbyDisabled, "disabled")
	}

	// A live conversation outranks sensor state: standby never re-triggers into
	// its own call, and never owns hang-up.
	if in.CallActive {
		g.wasCallActive = true
		g.resetEvidence()
		return g.transition(StandbyActive, "call_active")
	}
	if g.wasCallActive {
		// Post-call cooldown: the XVF keeps reporting speech_detected for the tail
		// of Kocoro's own goodbye and room reverb. Re-opening on that echo would
		// make the robot un-dismissable.
		g.wasCallActive = false
		g.cooldownUntil = now.Add(g.cfg.Cooldown)
		g.resetEvidence()
		return g.transition(StandbyCooldown, "call_ended")
	}

	if reason, ok := standbyPerceptionUnusable(in.Snapshot); ok {
		// Silent degrade: without trustworthy DOA there is no evidence to act on,
		// so fail closed and wait. Recovery re-arms on the next healthy sample.
		g.resetEvidence()
		return g.transition(StandbyDegraded, reason)
	}
	if g.state == StandbyDegraded {
		g.resetEvidence()
		return g.transition(StandbyIdle, "perception_recovered")
	}

	if g.state == StandbyCooldown {
		if now.Before(g.cooldownUntil) {
			return StandbyDecision{State: g.state}
		}
		g.resetEvidence()
		return g.transition(StandbyIdle, "cooldown_complete")
	}

	front := math.Abs(in.Snapshot.DOA.Angle-math.Pi/2) <= g.cfg.FrontHalfAngle
	if in.Snapshot.DOA.SpeechDetected && front {
		g.vadHits++
	} else {
		g.vadHits = 0
		// A clean no-speech sample re-arms the gate, so a failed activation (e.g.
		// audio start error) does not wedge standby until the next call.
		g.activateIssued = false
	}
	if g.vadHits >= g.cfg.VADHits && !g.activateIssued {
		g.activateIssued = true
		return StandbyDecision{State: g.state, Reason: "speech_front", Actions: []StandbyAction{{Kind: StandbyActivate}}}
	}
	return StandbyDecision{State: g.state}
}

// standbyPerceptionUnusable reports whether the snapshot lacks trustworthy DOA.
// Standby deliberately tolerates a stale/absent FACE sample — face is a gaze-gate
// input, and resident listening must keep working with the camera unavailable.
func standbyPerceptionUnusable(s PerceptionSnapshot) (string, bool) {
	switch s.Health {
	case PerceptionDaemonUnreachable, PerceptionInvalidPayload:
		return string(s.Health), true
	}
	if !s.DOA.Available || !s.DOA.Fresh {
		return string(PerceptionDOAUnavailable), true
	}
	return "", false
}

func (g *StandbyGate) resetEvidence() {
	g.vadHits = 0
	g.activateIssued = false
}

func (g *StandbyGate) transition(state StandbyState, reason string, actions ...StandbyAction) StandbyDecision {
	changed := g.state != state
	g.state = state
	return StandbyDecision{State: state, Reason: reason, Changed: changed, Actions: actions}
}

// StandbyIdleInput is one sample of conversation liveness. VoiceEventSeq is a
// monotonic counter bumped on every emitted voice_state transition — the runtime
// owns it, so the gate stays free of Realtime plumbing.
type StandbyIdleInput struct {
	Now               time.Time
	CallActive        bool
	StandbyOwned      bool
	TaskPending       bool
	AssistantSpeaking bool
	VoiceEventSeq     uint64
}

// StandbyIdleGate hangs up a standby-opened conversation that has gone quiet, so
// a robot in resident-listen mode returns to standby instead of holding a live
// session forever after the user walks away. It only ever acts on calls standby
// itself opened: a manual trigger-key call is the user's to end.
//
// Any of a voice_state transition, an in-flight task, or Kocoro speaking counts
// as liveness and rewinds the timer — a long tool run must never be cut off.
type StandbyIdleGate struct {
	timeout time.Duration

	tracking     bool
	lastActivity time.Time
	lastSeq      uint64
	fired        bool
}

func NewStandbyIdleGate(timeout time.Duration) *StandbyIdleGate {
	return &StandbyIdleGate{timeout: timeout}
}

// Update reports whether the call should be hung up now. It latches, so one idle
// window produces exactly one hang-up regardless of poll cadence.
func (g *StandbyIdleGate) Update(in StandbyIdleInput) bool {
	if g.timeout <= 0 || !in.CallActive || !in.StandbyOwned {
		g.tracking = false
		g.fired = false
		return false
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	if !g.tracking {
		g.tracking = true
		g.lastSeq = in.VoiceEventSeq
		g.lastActivity = now
		g.fired = false
		return false
	}
	live := in.TaskPending || in.AssistantSpeaking || in.VoiceEventSeq != g.lastSeq
	g.lastSeq = in.VoiceEventSeq
	if live {
		g.lastActivity = now
		g.fired = false
		return false
	}
	if g.fired || now.Sub(g.lastActivity) < g.timeout {
		return false
	}
	g.fired = true
	return true
}

// IdleFor exposes the current quiet duration for diagnostics.
func (g *StandbyIdleGate) IdleFor(now time.Time) time.Duration {
	if !g.tracking {
		return 0
	}
	return now.Sub(g.lastActivity)
}
