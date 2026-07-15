package koe

import (
	"math"
	"testing"
	"time"
)

func bargeSnapshot(now time.Time, speech bool, angle float64) PerceptionSnapshot {
	return PerceptionSnapshot{
		ObservedAt: now,
		DOA: DOASample{
			Available:      true,
			Fresh:          true,
			Angle:          angle,
			SpeechDetected: speech,
		},
	}
}

func TestBargePerceptionGateRejectsResidualEchoPulses(t *testing.T) {
	g := NewBargePerceptionGate()
	now := time.Unix(100, 0)
	for cycle := 0; cycle < 4; cycle++ {
		for hit := 0; hit < defaultBargePerceptionHits-1; hit++ {
			d := g.Update(now, true, true, bargeSnapshot(now, true, math.Pi/2))
			if d.Authorized {
				t.Fatalf("cycle %d hit %d authorized isolated echo", cycle, hit)
			}
			now = now.Add(100 * time.Millisecond)
		}
		d := g.Update(now, true, true, bargeSnapshot(now, false, math.Pi/2))
		if d.Authorized {
			t.Fatalf("cycle %d authorized after evidence broke", cycle)
		}
		// Isolated two-hit echo clusters must age out before the next cluster.
		now = now.Add(defaultBargePerceptionWindow + time.Millisecond)
	}
}

func TestBargePerceptionGateClearsCandidateOnOutsideSpeech(t *testing.T) {
	g := NewBargePerceptionGate()
	now := time.Unix(150, 0)
	for range defaultBargePerceptionHits - 1 {
		if d := g.Update(now, true, true, bargeSnapshot(now, true, math.Pi/2)); d.Authorized {
			t.Fatal("two front hits must not authorize")
		}
		now = now.Add(100 * time.Millisecond)
	}
	if d := g.Update(now, true, true, bargeSnapshot(now, true, 0)); d.Authorized || d.Hits != 0 {
		t.Fatalf("outside speech decision=%+v, want cleared candidate", d)
	}
	now = now.Add(100 * time.Millisecond)
	if d := g.Update(now, true, true, bargeSnapshot(now, true, math.Pi/2)); d.Authorized {
		t.Fatal("one front hit after conflicting direction must not authorize")
	}
}

func TestBargePerceptionGateAuthorizesSustainedFrontSpeechAndHolds(t *testing.T) {
	g := NewBargePerceptionGate()
	now := time.Unix(200, 0)
	for hit := 1; hit <= defaultBargePerceptionHits; hit++ {
		d := g.Update(now, true, true, bargeSnapshot(now, true, math.Pi/2))
		if got, want := d.Authorized, hit == defaultBargePerceptionHits; got != want {
			t.Fatalf("hit %d authorized=%v, want %v", hit, got, want)
		}
		now = now.Add(100 * time.Millisecond)
	}
	d := g.Update(now, true, true, bargeSnapshot(now, false, math.Pi/2))
	if !d.Authorized {
		t.Fatal("brief DOA gap must not chop an authorized utterance")
	}
	now = now.Add(100 * time.Millisecond)
	d = g.Update(now, true, true, PerceptionSnapshot{ObservedAt: now})
	if !d.Authorized || d.Changed || d.Reason != "holding_doa_gap" {
		t.Fatalf("unavailable DOA during hold decision=%+v, want held authorization", d)
	}
	now = now.Add(defaultBargePerceptionHold + time.Millisecond)
	d = g.Update(now, true, true, bargeSnapshot(now, false, math.Pi/2))
	if d.Authorized || !d.Changed {
		t.Fatalf("expired decision=%+v, want closed transition", d)
	}
}

func TestBargePerceptionGateAuthorizesSparseFrontPulsesWithinWindow(t *testing.T) {
	g := NewBargePerceptionGate()
	now := time.Unix(250, 0)

	// Live Wireless A/B on a loaded CM4 produced exactly this shape for a
	// front speaker: speech, speech, one no-speech sample, then speech. The
	// three positive samples spanned 1.823s; requiring consecutive REST
	// samples falsely rejected the talk-over.
	for i, step := range []struct {
		after  time.Duration
		speech bool
	}{
		{speech: true},
		{after: 654 * time.Millisecond, speech: true},
		{after: 570 * time.Millisecond, speech: false},
		{after: 599 * time.Millisecond, speech: true},
	} {
		now = now.Add(step.after)
		d := g.Update(now, true, true, bargeSnapshot(now, step.speech, math.Pi/2))
		if got, want := d.Authorized, i == 3; got != want {
			t.Fatalf("sample %d authorized=%v, want %v (decision=%+v)", i, got, want, d)
		}
	}
}

func TestBargePerceptionGateHoldsThroughServerVADLatency(t *testing.T) {
	g := NewBargePerceptionGate()
	now := time.Unix(275, 0)
	for range defaultBargePerceptionHits {
		g.Update(now, true, true, bargeSnapshot(now, true, math.Pi/2))
		now = now.Add(100 * time.Millisecond)
	}

	// Wireless live evidence showed roughly 3s from hardware authorization to
	// Realtime speech_started. Four seconds must remain inside the authorized
	// hold so ordinary polling/VAD jitter cannot close capture at that boundary.
	now = now.Add(4 * time.Second)
	if d := g.Update(now, true, true, bargeSnapshot(now, false, math.Pi/2)); !d.Authorized {
		t.Fatalf("decision=%+v, want authorization held through server VAD latency", d)
	}
}

func TestBargePerceptionGateExpiresCandidateAcrossLongDOAGap(t *testing.T) {
	g := NewBargePerceptionGate()
	now := time.Unix(290, 0)
	for range defaultBargePerceptionHits - 1 {
		g.Update(now, true, true, bargeSnapshot(now, true, math.Pi/2))
		now = now.Add(100 * time.Millisecond)
	}
	now = now.Add(defaultBargePerceptionWindow + time.Millisecond)
	if d := g.Update(now, true, true, PerceptionSnapshot{ObservedAt: now}); d.Authorized || d.Hits != 0 {
		t.Fatalf("long DOA gap decision=%+v, want expired fail-closed candidate", d)
	}
	if d := g.Update(now, true, true, bargeSnapshot(now, true, math.Pi/2)); d.Authorized || d.Hits != 1 {
		t.Fatalf("fresh hit decision=%+v, want a new one-hit candidate", d)
	}
}

func TestBargePerceptionGateFailsClosed(t *testing.T) {
	g := NewBargePerceptionGate()
	now := time.Unix(300, 0)
	if d := g.Update(now, true, true, PerceptionSnapshot{ObservedAt: now}); d.Authorized {
		t.Fatalf("unavailable DOA before authorization decision=%+v, want closed", d)
	}
	for range defaultBargePerceptionHits {
		g.Update(now, true, true, bargeSnapshot(now, true, math.Pi/2))
		now = now.Add(100 * time.Millisecond)
	}
	if d := g.Update(now, false, true, bargeSnapshot(now, true, math.Pi/2)); d.Authorized {
		t.Fatalf("inactive call decision=%+v, want closed", d)
	}
	for range defaultBargePerceptionHits {
		d := g.Update(now, true, true, bargeSnapshot(now, true, 0))
		if d.Authorized {
			t.Fatal("rear/side speech must not authorize front talk-over")
		}
		now = now.Add(100 * time.Millisecond)
	}
}
