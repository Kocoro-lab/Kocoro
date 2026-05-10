package memory

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSpawner records spawn count and lets the test control whether each
// spawn cycle reports failure or success at the WaitReady step.
type fakeSpawner struct {
	failsRemaining atomic.Int32 // first N Spawn or WaitReady calls fail
	spawned        atomic.Int32
	waitReadyErr   error
	waitErr        error
	onReadyHit     atomic.Int32 // observed via the Supervisor's onReady callback
}

func (f *fakeSpawner) Spawn(ctx context.Context) error {
	f.spawned.Add(1)
	if f.failsRemaining.Load() > 0 {
		f.failsRemaining.Add(-1)
		return errors.New("simulated spawn failure")
	}
	return nil
}

func (f *fakeSpawner) WaitReady(ctx context.Context, _ time.Duration) error {
	return f.waitReadyErr
}

func (f *fakeSpawner) Wait() error {
	return f.waitErr
}

func TestSupervisor_BackoffAndDegradedAfterBudget(t *testing.T) {
	sp := &fakeSpawner{}
	sp.failsRemaining.Store(10) // always fail → spawn error
	var gotReason string
	var gotAttempts int
	sup := NewSupervisor(sp, 3, func() { sp.onReadyHit.Add(1) })
	sup.SetOnDegraded(func(reason string, attempts int) {
		gotReason = reason
		gotAttempts = attempts
	})
	sup.testBackoff = func(int) time.Duration { return 1 * time.Millisecond }
	final := sup.Run(context.Background())
	if final != StateDegraded {
		t.Fatalf("final=%v want Degraded", final)
	}
	if got := sp.spawned.Load(); got < 3 {
		t.Fatalf("spawned=%d want >=3", got)
	}
	if sp.onReadyHit.Load() != 0 {
		t.Fatal("onReady should not fire when sidecar never becomes ready")
	}
	if gotReason != "tlm_exec_error" {
		t.Fatalf("reason=%q want tlm_exec_error", gotReason)
	}
	if gotAttempts != 3 {
		t.Fatalf("attempts=%d want 3", gotAttempts)
	}
}

func TestSupervisor_OnDegraded_StartupTimeout(t *testing.T) {
	sp := &fakeSpawner{
		waitReadyErr: ErrReadyCeilingExceeded,
		waitErr:      errors.New("unused"),
	}
	var gotReason string
	sup := NewSupervisor(sp, 2, nil)
	sup.SetOnDegraded(func(reason string, _ int) { gotReason = reason })
	sup.testBackoff = func(int) time.Duration { return 1 * time.Millisecond }
	final := sup.Run(context.Background())
	if final != StateDegraded {
		t.Fatalf("final=%v want Degraded", final)
	}
	if gotReason != "startup_timeout" {
		t.Fatalf("reason=%q want startup_timeout", gotReason)
	}
}

func TestSupervisor_OnDegraded_RepeatedCrash(t *testing.T) {
	// Sidecar becomes ready then immediately exits — repeated_crash.
	sp := &fakeSpawner{
		waitErr: errors.New("simulated exit"),
	}
	var gotReason string
	sup := NewSupervisor(sp, 2, func() { sp.onReadyHit.Add(1) })
	sup.SetOnDegraded(func(reason string, _ int) { gotReason = reason })
	sup.testBackoff = func(int) time.Duration { return 1 * time.Millisecond }
	final := sup.Run(context.Background())
	if final != StateDegraded {
		t.Fatalf("final=%v want Degraded", final)
	}
	if sp.onReadyHit.Load() == 0 {
		t.Fatal("onReady should have fired at least once")
	}
	if gotReason != "repeated_crash" {
		t.Fatalf("reason=%q want repeated_crash", gotReason)
	}
}

func TestSupervisor_OnDegraded_NotCalledOnCtxCancel(t *testing.T) {
	sp := &fakeSpawner{}
	sp.failsRemaining.Store(100)
	called := false
	sup := NewSupervisor(sp, 100, nil)
	sup.SetOnDegraded(func(string, int) { called = true })
	sup.testBackoff = func(int) time.Duration { return 50 * time.Millisecond }
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	final := sup.Run(ctx)
	if final != StateStopped {
		t.Fatalf("final=%v want Stopped", final)
	}
	if called {
		t.Fatal("onDegraded must not fire on ctx cancel")
	}
}

func TestSupervisor_RecoversFromColdStartFailure(t *testing.T) {
	// Spec acceptance #53: first WaitReady failure must be recoverable.
	sp := &fakeSpawner{}
	sp.failsRemaining.Store(2)                 // first 2 Spawns fail; 3rd succeeds
	sp.waitErr = errors.New("simulated crash") // child exits after becoming ready
	sup := NewSupervisor(sp, 4, func() { sp.onReadyHit.Add(1) })
	sup.testBackoff = func(int) time.Duration { return 1 * time.Millisecond }
	final := sup.Run(context.Background())
	if sp.onReadyHit.Load() == 0 {
		t.Fatal("onReady should have fired at least once")
	}
	if final != StateDegraded {
		// Eventually budget should run out because Wait keeps returning err.
		t.Fatalf("final=%v want Degraded", final)
	}
}

func TestSupervisor_CtxCancelExitsCleanly(t *testing.T) {
	sp := &fakeSpawner{}
	sp.failsRemaining.Store(100)
	sup := NewSupervisor(sp, 100, nil)
	sup.testBackoff = func(int) time.Duration { return 50 * time.Millisecond }
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	final := sup.Run(ctx)
	if final != StateStopped {
		t.Fatalf("final=%v want Stopped", final)
	}
}
