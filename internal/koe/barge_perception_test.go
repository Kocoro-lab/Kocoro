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
		now = now.Add(100 * time.Millisecond)
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
	now = now.Add(defaultBargePerceptionHold + time.Millisecond)
	d = g.Update(now, true, true, bargeSnapshot(now, false, math.Pi/2))
	if d.Authorized || !d.Changed {
		t.Fatalf("expired decision=%+v, want closed transition", d)
	}
}

func TestBargePerceptionGateFailsClosed(t *testing.T) {
	g := NewBargePerceptionGate()
	now := time.Unix(300, 0)
	for range defaultBargePerceptionHits {
		g.Update(now, true, true, bargeSnapshot(now, true, math.Pi/2))
		now = now.Add(100 * time.Millisecond)
	}
	if d := g.Update(now, true, true, PerceptionSnapshot{ObservedAt: now}); d.Authorized {
		t.Fatalf("unavailable DOA decision=%+v, want fail closed", d)
	}
	for range defaultBargePerceptionHits {
		d := g.Update(now, true, true, bargeSnapshot(now, true, 0))
		if d.Authorized {
			t.Fatal("rear/side speech must not authorize front talk-over")
		}
		now = now.Add(100 * time.Millisecond)
	}
	for range defaultBargePerceptionHits {
		g.Update(now, true, true, bargeSnapshot(now, true, math.Pi/2))
		now = now.Add(100 * time.Millisecond)
	}
	if d := g.Update(now, false, true, bargeSnapshot(now, true, math.Pi/2)); d.Authorized {
		t.Fatalf("inactive call decision=%+v, want closed", d)
	}
}
