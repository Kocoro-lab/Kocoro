package koe

import (
	"math"
	"testing"
	"time"
)

func standbySnapshot(now time.Time, speech bool, angle float64) PerceptionSnapshot {
	return PerceptionSnapshot{
		ObservedAt: now,
		Health:     PerceptionOK,
		Face:       FaceSample{Available: true, Fresh: true},
		DOA:        DOASample{Available: true, Fresh: true, Angle: angle, SpeechDetected: speech},
	}
}

func standbyActionKinds(d StandbyDecision) []StandbyActionKind {
	out := make([]StandbyActionKind, len(d.Actions))
	for i, a := range d.Actions {
		out[i] = a.Kind
	}
	return out
}

func enabledStandbyConfig() StandbyConfig {
	cfg := DefaultStandbyConfig()
	cfg.Enabled = true
	return cfg
}

func newTestStandbyGate(t *testing.T, cfg StandbyConfig) *StandbyGate {
	t.Helper()
	g, err := NewStandbyGate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestParseOpenMode(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", OpenModeTrigger},
		{"trigger", OpenModeTrigger},
		{" Standby ", OpenModeStandby},
	} {
		got, err := ParseOpenMode(tc.in)
		if err != nil || got != tc.want {
			t.Fatalf("ParseOpenMode(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	if _, err := ParseOpenMode("always"); err == nil {
		t.Fatal("expected an unknown open mode to fail loud")
	}
}

// Front speech is the only condition: no face is required, and exactly one
// activation is issued per utterance.
func TestStandbyGateActivatesOnFrontSpeechWithoutFace(t *testing.T) {
	now := time.Unix(100, 0)
	g := newTestStandbyGate(t, enabledStandbyConfig())

	snap := standbySnapshot(now, true, math.Pi/2)
	snap.Face = FaceSample{Available: true, Fresh: true, Detected: false}
	if d := g.Update(StandbyInput{Now: now, Snapshot: snap}); len(d.Actions) != 0 {
		t.Fatalf("first speech sample actions = %v", standbyActionKinds(d))
	}

	now = now.Add(100 * time.Millisecond)
	snap = standbySnapshot(now, true, math.Pi/2)
	d := g.Update(StandbyInput{Now: now, Snapshot: snap})
	if len(d.Actions) != 1 || d.Actions[0].Kind != StandbyActivate || d.Reason != "speech_front" {
		t.Fatalf("second speech sample decision = %+v", d)
	}

	// Still speaking, still no call yet: the gate must not re-issue.
	now = now.Add(100 * time.Millisecond)
	if d := g.Update(StandbyInput{Now: now, Snapshot: standbySnapshot(now, true, math.Pi/2)}); len(d.Actions) != 0 {
		t.Fatalf("repeat activation = %v", standbyActionKinds(d))
	}
}

// Speech from the side (outside the front half-angle) is room noise, not a
// conversation opener.
func TestStandbyGateIgnoresOffAxisSpeech(t *testing.T) {
	now := time.Unix(200, 0)
	g := newTestStandbyGate(t, enabledStandbyConfig())
	for i := 0; i < 5; i++ {
		now = now.Add(100 * time.Millisecond)
		if d := g.Update(StandbyInput{Now: now, Snapshot: standbySnapshot(now, true, 0)}); len(d.Actions) != 0 {
			t.Fatalf("off-axis speech activated: %v", standbyActionKinds(d))
		}
	}
}

// After a call ends, the cooldown absorbs the XVF's echo tail so the goodbye
// cannot immediately re-open the conversation.
func TestStandbyGateCooldownBlocksEchoRetrigger(t *testing.T) {
	now := time.Unix(300, 0)
	cfg := enabledStandbyConfig()
	g := newTestStandbyGate(t, cfg)

	if d := g.Update(StandbyInput{Now: now, CallActive: true, Snapshot: standbySnapshot(now, true, math.Pi/2)}); d.State != StandbyActive {
		t.Fatalf("call active state = %+v", d)
	}
	now = now.Add(100 * time.Millisecond)
	d := g.Update(StandbyInput{Now: now, Snapshot: standbySnapshot(now, false, math.Pi/2)})
	if d.State != StandbyCooldown || d.Reason != "call_ended" {
		t.Fatalf("post-call decision = %+v", d)
	}

	// Echo tail during cooldown: plenty of front speech, zero activations.
	for i := 0; i < 10; i++ {
		now = now.Add(100 * time.Millisecond)
		if d := g.Update(StandbyInput{Now: now, Snapshot: standbySnapshot(now, true, math.Pi/2)}); len(d.Actions) != 0 {
			t.Fatalf("activated during cooldown: %v", standbyActionKinds(d))
		}
	}

	now = now.Add(cfg.Cooldown)
	if d := g.Update(StandbyInput{Now: now, Snapshot: standbySnapshot(now, false, math.Pi/2)}); d.State != StandbyIdle || d.Reason != "cooldown_complete" {
		t.Fatalf("cooldown completion = %+v", d)
	}
	// Evidence collected during cooldown must not carry over.
	now = now.Add(100 * time.Millisecond)
	if d := g.Update(StandbyInput{Now: now, Snapshot: standbySnapshot(now, true, math.Pi/2)}); len(d.Actions) != 0 {
		t.Fatalf("cooldown evidence leaked into idle: %v", standbyActionKinds(d))
	}
	now = now.Add(100 * time.Millisecond)
	if d := g.Update(StandbyInput{Now: now, Snapshot: standbySnapshot(now, true, math.Pi/2)}); len(d.Actions) != 1 {
		t.Fatalf("re-arm after cooldown = %+v", d)
	}
}

func TestStandbyGateDegradesAndRecovers(t *testing.T) {
	now := time.Unix(400, 0)
	g := newTestStandbyGate(t, enabledStandbyConfig())

	now = now.Add(100 * time.Millisecond)
	g.Update(StandbyInput{Now: now, Snapshot: standbySnapshot(now, true, math.Pi/2)}) // one hit banked

	now = now.Add(100 * time.Millisecond)
	bad := standbySnapshot(now, true, math.Pi/2)
	bad.Health = PerceptionDaemonUnreachable
	d := g.Update(StandbyInput{Now: now, Snapshot: bad})
	if d.State != StandbyDegraded || len(d.Actions) != 0 {
		t.Fatalf("unreachable decision = %+v", d)
	}

	// Recovery re-arms from zero: stale evidence from before the gap must not
	// combine with one fresh sample into an activation.
	now = now.Add(100 * time.Millisecond)
	if d := g.Update(StandbyInput{Now: now, Snapshot: standbySnapshot(now, true, math.Pi/2)}); d.State != StandbyIdle || d.Reason != "perception_recovered" || len(d.Actions) != 0 {
		t.Fatalf("recovery decision = %+v", d)
	}
	now = now.Add(100 * time.Millisecond)
	if d := g.Update(StandbyInput{Now: now, Snapshot: standbySnapshot(now, true, math.Pi/2)}); len(d.Actions) != 0 {
		t.Fatalf("first post-recovery hit activated: %v", standbyActionKinds(d))
	}
	now = now.Add(100 * time.Millisecond)
	if d := g.Update(StandbyInput{Now: now, Snapshot: standbySnapshot(now, true, math.Pi/2)}); len(d.Actions) != 1 {
		t.Fatalf("post-recovery activation = %+v", d)
	}
}

// A missing camera must not disable resident listening — face is a gaze input.
func TestStandbyGateToleratesStaleFace(t *testing.T) {
	now := time.Unix(500, 0)
	g := newTestStandbyGate(t, enabledStandbyConfig())
	for i := 0; i < 2; i++ {
		now = now.Add(100 * time.Millisecond)
		snap := standbySnapshot(now, true, math.Pi/2)
		snap.Health = PerceptionFaceStale
		snap.Face = FaceSample{}
		if d := g.Update(StandbyInput{Now: now, Snapshot: snap}); d.State == StandbyDegraded {
			t.Fatalf("face-stale snapshot degraded standby: %+v", d)
		}
	}
	if g.State() != StandbyIdle {
		t.Fatalf("state = %s", g.State())
	}
}

func TestStandbyGateDisabledNeverActivates(t *testing.T) {
	now := time.Unix(600, 0)
	g := newTestStandbyGate(t, DefaultStandbyConfig()) // Enabled defaults to false (trigger mode)
	for i := 0; i < 5; i++ {
		now = now.Add(100 * time.Millisecond)
		d := g.Update(StandbyInput{Now: now, Snapshot: standbySnapshot(now, true, math.Pi/2)})
		if d.State != StandbyDisabled || len(d.Actions) != 0 {
			t.Fatalf("disabled gate decision = %+v", d)
		}
	}
}

func TestStandbyConfigFromEnv(t *testing.T) {
	cfg, err := standbyConfigFromLookup(func(k string) (string, bool) {
		switch k {
		case "KOE_STANDBY_IDLE_HANGUP_S":
			return "40", true
		case "KOE_STANDBY_COOLDOWN_MS":
			return "9000", true
		case "KOE_STANDBY_VAD_HITS":
			return "3", true
		case "KOE_STANDBY_FRONT_DEG":
			return "45", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IdleHangup != 40*time.Second || cfg.Cooldown != 9*time.Second || cfg.VADHits != 3 {
		t.Fatalf("cfg = %+v", cfg)
	}
	if math.Abs(cfg.FrontHalfAngle-math.Pi/4) > 1e-9 {
		t.Fatalf("front half-angle = %v", cfg.FrontHalfAngle)
	}
	// The mode itself is never env-derived here — a stray tuning value must not
	// switch a trigger robot into resident listening.
	if cfg.Enabled {
		t.Fatal("standby config from env must not enable the mode")
	}

	for _, k := range []string{"KOE_STANDBY_IDLE_HANGUP_S", "KOE_STANDBY_COOLDOWN_MS", "KOE_STANDBY_VAD_HITS", "KOE_STANDBY_FRONT_DEG"} {
		key := k
		if _, err := standbyConfigFromLookup(func(name string) (string, bool) {
			if name == key {
				return "0", true
			}
			return "", false
		}); err == nil {
			t.Fatalf("%s = 0 should be rejected", key)
		}
	}
}

func idleInput(now time.Time, seq uint64) StandbyIdleInput {
	return StandbyIdleInput{Now: now, CallActive: true, StandbyOwned: true, VoiceEventSeq: seq}
}

func TestStandbyIdleGateFiresOnceAfterSilence(t *testing.T) {
	now := time.Unix(700, 0)
	g := NewStandbyIdleGate(25 * time.Second)

	if g.Update(idleInput(now, 1)) {
		t.Fatal("fired on the first sample")
	}
	now = now.Add(24 * time.Second)
	if g.Update(idleInput(now, 1)) {
		t.Fatal("fired before the timeout")
	}
	now = now.Add(1 * time.Second)
	if !g.Update(idleInput(now, 1)) {
		t.Fatal("did not fire at the timeout")
	}
	// Latched: one idle window yields exactly one hang-up regardless of cadence.
	now = now.Add(1 * time.Second)
	if g.Update(idleInput(now, 1)) {
		t.Fatal("fired twice for one idle window")
	}
}

func TestStandbyIdleGateResetsOnLiveness(t *testing.T) {
	base := time.Unix(800, 0)
	timeout := 25 * time.Second

	cases := []struct {
		name string
		live func(in StandbyIdleInput) StandbyIdleInput
	}{
		{"voice_state transition", func(in StandbyIdleInput) StandbyIdleInput { in.VoiceEventSeq = 99; return in }},
		{"task pending", func(in StandbyIdleInput) StandbyIdleInput { in.TaskPending = true; return in }},
		{"assistant speaking", func(in StandbyIdleInput) StandbyIdleInput { in.AssistantSpeaking = true; return in }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := base
			g := NewStandbyIdleGate(timeout)
			g.Update(idleInput(now, 1))

			now = now.Add(24 * time.Second)
			if g.Update(tc.live(idleInput(now, 1))) {
				t.Fatal("fired on a live sample")
			}
			// The timer restarted: the original deadline must pass without a hang-up.
			now = now.Add(2 * time.Second)
			if g.Update(idleInput(now, 99)) {
				t.Fatal("fired at the pre-reset deadline")
			}
			now = now.Add(timeout)
			if !g.Update(idleInput(now, 99)) {
				t.Fatal("did not fire after the rewound window elapsed")
			}
		})
	}
}

// A long-running tool call is not silence: the timer must not cut it off, and it
// must resume from the moment the task drains.
func TestStandbyIdleGateNeverCutsOffALongTask(t *testing.T) {
	now := time.Unix(900, 0)
	timeout := 25 * time.Second
	g := NewStandbyIdleGate(timeout)
	g.Update(idleInput(now, 1))

	for i := 0; i < 100; i++ { // 100 * 1s = 4x the timeout, all with a task in flight
		now = now.Add(time.Second)
		in := idleInput(now, 1)
		in.TaskPending = true
		if g.Update(in) {
			t.Fatal("hung up while a task was in flight")
		}
	}
	now = now.Add(timeout)
	if !g.Update(idleInput(now, 1)) {
		t.Fatal("did not resume the idle window after the task drained")
	}
}

func TestStandbyIdleGateIgnoresManualCalls(t *testing.T) {
	now := time.Unix(1000, 0)
	g := NewStandbyIdleGate(25 * time.Second)
	in := idleInput(now, 1)
	in.StandbyOwned = false // trigger-key call: only the user ends it
	g.Update(in)

	now = now.Add(10 * time.Minute)
	in = idleInput(now, 1)
	in.StandbyOwned = false
	if g.Update(in) {
		t.Fatal("auto-dismissed a manually opened call")
	}
}

// Ending a call clears the tracker, so the next standby call gets a full window.
func TestStandbyIdleGateRearmsAfterCallEnds(t *testing.T) {
	now := time.Unix(1100, 0)
	timeout := 25 * time.Second
	g := NewStandbyIdleGate(timeout)
	g.Update(idleInput(now, 1))
	now = now.Add(timeout)
	if !g.Update(idleInput(now, 1)) {
		t.Fatal("first window did not fire")
	}

	now = now.Add(time.Second)
	if g.Update(StandbyIdleInput{Now: now, CallActive: false, StandbyOwned: true, VoiceEventSeq: 1}) {
		t.Fatal("fired with no active call")
	}
	now = now.Add(time.Second)
	if g.Update(idleInput(now, 2)) {
		t.Fatal("new call fired immediately instead of starting a fresh window")
	}
	now = now.Add(timeout - time.Second)
	if g.Update(idleInput(now, 2)) {
		t.Fatal("new call fired before its own full window elapsed")
	}
	now = now.Add(time.Second)
	if !g.Update(idleInput(now, 2)) {
		t.Fatal("new call did not fire after its window")
	}
}

func TestStandbyIdleGateDisabledTimeout(t *testing.T) {
	now := time.Unix(1200, 0)
	g := NewStandbyIdleGate(0)
	g.Update(idleInput(now, 1))
	now = now.Add(time.Hour)
	if g.Update(idleInput(now, 1)) {
		t.Fatal("fired with the timeout disabled")
	}
}
