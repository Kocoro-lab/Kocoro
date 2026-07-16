package koe

import (
	"math"
	"testing"
	"time"
)

func healthyGazeSnapshot(now time.Time, face, speech bool, angle float64) PerceptionSnapshot {
	return PerceptionSnapshot{
		ObservedAt: now,
		Health:     PerceptionOK,
		Face: FaceSample{
			Available: true,
			Fresh:     true,
			Detected:  face,
		},
		DOA: DOASample{
			Available:      true,
			Fresh:          true,
			Angle:          angle,
			SpeechDetected: speech,
		},
	}
}

func actionKinds(d GazeDecision) []GazeActionKind {
	out := make([]GazeActionKind, len(d.Actions))
	for i, a := range d.Actions {
		out[i] = a.Kind
	}
	return out
}

func TestGazeGateHappyPathAndActiveCallImmunity(t *testing.T) {
	now := time.Unix(100, 0)
	g, err := NewGazeGate(DefaultGazeConfig())
	if err != nil {
		t.Fatal(err)
	}

	// One face sample is not enough; the second latches and starts preparation.
	if d := g.Update(GazeInput{Now: now, Snapshot: healthyGazeSnapshot(now, true, false, math.Pi/2)}); len(d.Actions) != 0 {
		t.Fatalf("first face actions = %v", actionKinds(d))
	}
	now = now.Add(100 * time.Millisecond)
	d := g.Update(GazeInput{Now: now, Snapshot: healthyGazeSnapshot(now, true, false, math.Pi/2)})
	if d.State != GazeArming || len(d.Actions) != 1 || d.Actions[0].Kind != GazePrepare {
		t.Fatalf("arming decision = %+v", d)
	}

	// Prepared session is visible, then two front-VAD samples activate exactly once.
	now = now.Add(100 * time.Millisecond)
	d = g.Update(GazeInput{Now: now, Prepared: true, Snapshot: healthyGazeSnapshot(now, true, true, math.Pi/2)})
	if d.State != GazeArmed || len(d.Actions) != 0 {
		t.Fatalf("first VAD decision = %+v", d)
	}
	now = now.Add(100 * time.Millisecond)
	d = g.Update(GazeInput{Now: now, Prepared: true, Snapshot: healthyGazeSnapshot(now, true, true, math.Pi/2)})
	if len(d.Actions) != 1 || d.Actions[0].Kind != GazeActivate {
		t.Fatalf("activation decision = %+v", d)
	}
	now = now.Add(100 * time.Millisecond)
	d = g.Update(GazeInput{Now: now, Prepared: true, Snapshot: healthyGazeSnapshot(now, true, true, math.Pi/2)})
	if len(d.Actions) != 0 {
		t.Fatalf("activation repeated: %+v", d)
	}

	// Once the runtime reports the call active, face loss cannot end it.
	now = now.Add(100 * time.Millisecond)
	d = g.Update(GazeInput{Now: now, CallActive: true, Snapshot: healthyGazeSnapshot(now, false, false, 0)})
	if d.State != GazeActive || len(d.Actions) != 0 {
		t.Fatalf("active decision = %+v", d)
	}
	now = now.Add(3 * time.Second)
	d = g.Update(GazeInput{Now: now, CallActive: true, Snapshot: healthyGazeSnapshot(now, false, false, 0)})
	if d.State != GazeActive || len(d.Actions) != 0 {
		t.Fatalf("face loss ended active call: %+v", d)
	}

	// Runtime call end enters cooldown and the same still-present face cannot rearm.
	now = now.Add(100 * time.Millisecond)
	d = g.Update(GazeInput{Now: now, Snapshot: healthyGazeSnapshot(now, true, false, math.Pi/2)})
	if d.State != GazeCooldown {
		t.Fatalf("call end decision = %+v", d)
	}
	now = now.Add(defaultGazeRearmCooldown + time.Millisecond)
	for i := 0; i < 3; i++ {
		d = g.Update(GazeInput{Now: now, Snapshot: healthyGazeSnapshot(now, true, false, math.Pi/2)})
		now = now.Add(100 * time.Millisecond)
	}
	if len(d.Actions) != 0 || d.State != GazeIdle {
		t.Fatalf("same face rearmed after cooldown: %+v", d)
	}

	// A real absence resets the encounter; a newly returning face may arm again.
	d = g.Update(GazeInput{Now: now, Snapshot: healthyGazeSnapshot(now, false, false, math.Pi/2)})
	now = now.Add(defaultGazeEncounterReset + time.Millisecond)
	d = g.Update(GazeInput{Now: now, Snapshot: healthyGazeSnapshot(now, false, false, math.Pi/2)})
	for i := 0; i < 2; i++ {
		now = now.Add(100 * time.Millisecond)
		d = g.Update(GazeInput{Now: now, Snapshot: healthyGazeSnapshot(now, true, false, math.Pi/2)})
	}
	if len(d.Actions) != 1 || d.Actions[0].Kind != GazePrepare {
		t.Fatalf("new encounter did not rearm: %+v", d)
	}
}

func TestGazeGateRequiresFrontDOA(t *testing.T) {
	now := time.Unix(100, 0)
	g, _ := NewGazeGate(DefaultGazeConfig())
	for i := 0; i < 2; i++ {
		g.Update(GazeInput{Now: now, Snapshot: healthyGazeSnapshot(now, true, false, math.Pi/2)})
		now = now.Add(100 * time.Millisecond)
	}
	for i := 0; i < 3; i++ {
		d := g.Update(GazeInput{Now: now, Prepared: true, Snapshot: healthyGazeSnapshot(now, true, true, 0)})
		if len(d.Actions) != 0 {
			t.Fatalf("side sound activated: %+v", d)
		}
		now = now.Add(100 * time.Millisecond)
	}
}

func TestGazeGateArmTimeoutCancelsAndBlocksSameEncounter(t *testing.T) {
	now := time.Unix(100, 0)
	g, _ := NewGazeGate(DefaultGazeConfig())
	for i := 0; i < 2; i++ {
		g.Update(GazeInput{Now: now, Snapshot: healthyGazeSnapshot(now, true, false, math.Pi/2)})
		now = now.Add(100 * time.Millisecond)
	}
	now = now.Add(defaultGazeArmTimeout + time.Millisecond)
	d := g.Update(GazeInput{Now: now, Prepared: true, Snapshot: healthyGazeSnapshot(now, true, false, math.Pi/2)})
	if len(d.Actions) != 1 || d.Actions[0].Kind != GazeCancelPreparation || d.Reason != "arm_timeout" {
		t.Fatalf("timeout decision = %+v", d)
	}
	for i := 0; i < 3; i++ {
		now = now.Add(100 * time.Millisecond)
		d = g.Update(GazeInput{Now: now, Snapshot: healthyGazeSnapshot(now, true, false, math.Pi/2)})
	}
	if len(d.Actions) != 0 {
		t.Fatalf("same encounter rearmed: %+v", d)
	}
}

func TestGazeGateFaceLossBlocksFlickeringSameEncounter(t *testing.T) {
	now := time.Unix(100, 0)
	g, _ := NewGazeGate(DefaultGazeConfig())
	for i := 0; i < 2; i++ {
		g.Update(GazeInput{Now: now, Snapshot: healthyGazeSnapshot(now, true, false, math.Pi/2)})
		now = now.Add(100 * time.Millisecond)
	}

	// A face miss held beyond FaceHold cancels this preparation and consumes the
	// encounter. Immediate detector flicker must not mint another session.
	now = now.Add(defaultGazeFaceHold + time.Millisecond)
	d := g.Update(GazeInput{Now: now, Prepared: true, Snapshot: healthyGazeSnapshot(now, false, false, math.Pi/2)})
	if len(d.Actions) != 1 || d.Actions[0].Kind != GazeCancelPreparation || d.Reason != "face_lost" {
		t.Fatalf("face-loss decision = %+v", d)
	}
	for i := 0; i < 4; i++ {
		now = now.Add(100 * time.Millisecond)
		d = g.Update(GazeInput{Now: now, Snapshot: healthyGazeSnapshot(now, true, false, math.Pi/2)})
	}
	if len(d.Actions) != 0 || d.State != GazeIdle {
		t.Fatalf("flickering face rearmed same encounter: %+v", d)
	}

	// A real absence clears the encounter latch; a returning face may prepare.
	d = g.Update(GazeInput{Now: now, Snapshot: healthyGazeSnapshot(now, false, false, math.Pi/2)})
	now = now.Add(defaultGazeEncounterReset + time.Millisecond)
	d = g.Update(GazeInput{Now: now, Snapshot: healthyGazeSnapshot(now, false, false, math.Pi/2)})
	for i := 0; i < 2; i++ {
		now = now.Add(100 * time.Millisecond)
		d = g.Update(GazeInput{Now: now, Snapshot: healthyGazeSnapshot(now, true, false, math.Pi/2)})
	}
	if len(d.Actions) != 1 || d.Actions[0].Kind != GazePrepare {
		t.Fatalf("new encounter did not rearm: %+v", d)
	}
}

func TestGazeGateKeepsPreparationAcrossOneTransportGap(t *testing.T) {
	now := time.Unix(100, 0)
	g, _ := NewGazeGate(DefaultGazeConfig())
	for i := 0; i < 2; i++ {
		g.Update(GazeInput{Now: now, Snapshot: healthyGazeSnapshot(now, true, false, math.Pi/2)})
		now = now.Add(100 * time.Millisecond)
	}
	s := healthyGazeSnapshot(now, true, true, math.Pi/2)
	s.Health = PerceptionDaemonUnreachable
	d := g.Update(GazeInput{Now: now, Prepared: true, Snapshot: s})
	if d.State != GazeArming || d.Reason != "perception_gap" || len(d.Actions) != 0 {
		t.Fatalf("transient gap decision = %+v", d)
	}

	now = now.Add(100 * time.Millisecond)
	d = g.Update(GazeInput{Now: now, Prepared: true, Snapshot: healthyGazeSnapshot(now, true, true, math.Pi/2)})
	if d.State != GazeArmed || len(d.Actions) != 0 {
		t.Fatalf("recovered decision = %+v", d)
	}
}

func TestGazeGatePersistentTransportFailureCancelsAndBlocksSameEncounter(t *testing.T) {
	now := time.Unix(100, 0)
	g, _ := NewGazeGate(DefaultGazeConfig())
	for i := 0; i < 2; i++ {
		g.Update(GazeInput{Now: now, Snapshot: healthyGazeSnapshot(now, true, false, math.Pi/2)})
		now = now.Add(100 * time.Millisecond)
	}
	s := healthyGazeSnapshot(now, true, true, math.Pi/2)
	s.Health = PerceptionDaemonUnreachable
	g.Update(GazeInput{Now: now, Prepared: true, Snapshot: s})
	now = now.Add(defaultGazeHealthGrace + time.Millisecond)
	d := g.Update(GazeInput{Now: now, Prepared: true, Snapshot: s})
	if d.State != GazeDegraded || len(d.Actions) != 1 || d.Actions[0].Kind != GazeCancelPreparation {
		t.Fatalf("persistent gap decision = %+v", d)
	}

	now = now.Add(100 * time.Millisecond)
	d = g.Update(GazeInput{Now: now, Snapshot: healthyGazeSnapshot(now, true, false, math.Pi/2)})
	if d.State != GazeIdle {
		t.Fatalf("recovery decision = %+v", d)
	}
	for i := 0; i < 4; i++ {
		now = now.Add(100 * time.Millisecond)
		d = g.Update(GazeInput{Now: now, Snapshot: healthyGazeSnapshot(now, true, false, math.Pi/2)})
	}
	if len(d.Actions) != 0 {
		t.Fatalf("same encounter rearmed after failure: %+v", d)
	}
}

func TestGazeGateInvalidSensorStateCancelsImmediately(t *testing.T) {
	now := time.Unix(100, 0)
	g, _ := NewGazeGate(DefaultGazeConfig())
	for i := 0; i < 2; i++ {
		g.Update(GazeInput{Now: now, Snapshot: healthyGazeSnapshot(now, true, false, math.Pi/2)})
		now = now.Add(100 * time.Millisecond)
	}
	s := healthyGazeSnapshot(now, true, true, math.Pi/2)
	s.Health = PerceptionFaceStale
	d := g.Update(GazeInput{Now: now, Prepared: true, Snapshot: s})
	if d.State != GazeDegraded || len(d.Actions) != 1 || d.Actions[0].Kind != GazeCancelPreparation {
		t.Fatalf("invalid sensor decision = %+v", d)
	}
}

func TestGazeConfigValidation(t *testing.T) {
	cfg := DefaultGazeConfig()
	cfg.FrontHalfAngle = math.Pi
	if _, err := NewGazeGate(cfg); err == nil {
		t.Fatal("invalid front cone should fail")
	}
}

func TestGazeConfigFromEnvOverridesAndRejectsInvalid(t *testing.T) {
	values := map[string]string{
		"KOE_GAZE_GATE":               "false",
		"KOE_GAZE_FACE_HITS":          "3",
		"KOE_GAZE_VAD_HITS":           "1",
		"KOE_GAZE_FACE_HOLD_MS":       "900",
		"KOE_GAZE_ARM_TIMEOUT_MS":     "12000",
		"KOE_GAZE_FRONT_DEG":          "45",
		"KOE_GAZE_REARM_COOLDOWN_MS":  "1500",
		"KOE_GAZE_ENCOUNTER_RESET_MS": "2500",
		"KOE_GAZE_HEALTH_GRACE_MS":    "1750",
	}
	lookup := func(k string) (string, bool) { v, ok := values[k]; return v, ok }
	cfg, err := gazeConfigFromLookup(lookup)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled || cfg.FaceHits != 3 || cfg.VADHits != 1 || cfg.FaceHold != 900*time.Millisecond ||
		cfg.ArmTimeout != 12*time.Second || cfg.RearmCooldown != 1500*time.Millisecond ||
		cfg.EncounterReset != 2500*time.Millisecond || cfg.HealthGrace != 1750*time.Millisecond ||
		math.Abs(cfg.FrontHalfAngle-math.Pi/4) > 1e-9 {
		t.Fatalf("overrides = %+v", cfg)
	}

	values["KOE_GAZE_FRONT_DEG"] = "90"
	if _, err := gazeConfigFromLookup(lookup); err == nil {
		t.Fatal("out-of-range front cone should fail")
	}
}
