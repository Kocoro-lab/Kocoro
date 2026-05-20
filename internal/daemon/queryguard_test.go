package daemon

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestQueryGuard_HappyPath(t *testing.T) {
	g := NewQueryGuard()
	if g.State() != QGStateIdle {
		t.Fatalf("initial state: want idle, got %v", g.State())
	}

	gen, ok := g.Reserve()
	if !ok {
		t.Fatal("first reserve should succeed")
	}
	if g.State() != QGStateDispatching {
		t.Errorf("after reserve: want dispatching, got %v", g.State())
	}

	if !g.TryStart(gen) {
		t.Fatal("tryStart with valid gen should succeed")
	}
	if g.State() != QGStateRunning {
		t.Errorf("after tryStart: want running, got %v", g.State())
	}

	if !g.End(gen) {
		t.Fatal("end with valid gen should succeed")
	}
	if g.State() != QGStateIdle {
		t.Errorf("after end: want idle, got %v", g.State())
	}
}

func TestQueryGuard_ReserveBlocksConcurrent(t *testing.T) {
	g := NewQueryGuard()
	g.Reserve()
	_, ok := g.Reserve()
	if ok {
		t.Error("second reserve while dispatching should fail")
	}
}

func TestQueryGuard_TryStartRejectsStaleGen(t *testing.T) {
	g := NewQueryGuard()
	gen, _ := g.Reserve()
	g.ForceEnd() // bumps gen
	if g.TryStart(gen) {
		t.Error("TryStart with stale gen should fail")
	}
}

func TestQueryGuard_ForceEndInvalidatesGeneration(t *testing.T) {
	g := NewQueryGuard()
	oldGen, _ := g.Reserve()
	g.TryStart(oldGen)

	g.ForceEnd()

	if g.State() != QGStateIdle {
		t.Errorf("after forceEnd: want idle, got %v", g.State())
	}
	if g.End(oldGen) {
		t.Error("End with stale gen after ForceEnd should return false")
	}

	newGen, _ := g.Reserve()
	if newGen == oldGen {
		t.Error("generation should bump on ForceEnd")
	}
}

func TestQueryGuard_CancelReservation(t *testing.T) {
	g := NewQueryGuard()
	gen, _ := g.Reserve()
	if !g.CancelReservation(gen) {
		t.Fatal("cancelReservation with valid gen should succeed")
	}
	if g.State() != QGStateIdle {
		t.Errorf("after cancelReservation: want idle, got %v", g.State())
	}
}

func TestQueryGuard_CancelReservationRejectedFromRunning(t *testing.T) {
	g := NewQueryGuard()
	gen, _ := g.Reserve()
	g.TryStart(gen)
	if g.CancelReservation(gen) {
		t.Error("CancelReservation from Running state should fail")
	}
}

func TestQueryGuard_ConcurrentReserve(t *testing.T) {
	g := NewQueryGuard()
	const N = 50
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range N {
		wg.Go(func() {
			if _, ok := g.Reserve(); ok {
				wins.Add(1)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Errorf("concurrent reserve: want exactly 1 winner, got %d", wins.Load())
	}
}

func TestQueryGuard_IsActive(t *testing.T) {
	g := NewQueryGuard()
	if g.IsActive() {
		t.Error("idle should not be active")
	}
	gen, _ := g.Reserve()
	if !g.IsActive() {
		t.Error("dispatching should be active")
	}
	g.TryStart(gen)
	if !g.IsActive() {
		t.Error("running should be active")
	}
	g.End(gen)
	if g.IsActive() {
		t.Error("idle (after end) should not be active")
	}
}
