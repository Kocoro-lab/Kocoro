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
	failsRemaining   atomic.Int32 // first N Spawn or WaitReady calls fail
	spawned          atomic.Int32
	shutdownCalls    atomic.Int32
	waitReadyErr     error
	waitReadyErrFn   func() error // optional: overrides waitReadyErr per-call
	waitErr          error
	onReadyHit       atomic.Int32 // observed via the Supervisor's onReady callback
	shutdownObserved []string     // ordered log of "spawn"/"shutdown" calls for sequencing assertions
}

func (f *fakeSpawner) Spawn(ctx context.Context) error {
	f.spawned.Add(1)
	f.shutdownObserved = append(f.shutdownObserved, "spawn")
	if f.failsRemaining.Load() > 0 {
		f.failsRemaining.Add(-1)
		return errors.New("simulated spawn failure")
	}
	return nil
}

func (f *fakeSpawner) WaitReady(ctx context.Context, _ time.Duration) error {
	if f.waitReadyErrFn != nil {
		return f.waitReadyErrFn()
	}
	return f.waitReadyErr
}

func (f *fakeSpawner) Wait() error {
	return f.waitErr
}

func (f *fakeSpawner) Shutdown(_ time.Duration) error {
	f.shutdownCalls.Add(1)
	f.shutdownObserved = append(f.shutdownObserved, "shutdown")
	return nil
}

func TestSupervisor_BackoffAndDegradedAfterBudget(t *testing.T) {
	sp := &fakeSpawner{}
	sp.failsRemaining.Store(10) // always fail → spawn error
	var gotReason string
	var gotAttempts int
	sup := NewSupervisor(sp, 3, func() { sp.onReadyHit.Add(1) })
	sup.SetOnDegraded(func(reason string, attempts int, _ map[string]any) {
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
	sup.SetOnDegraded(func(reason string, _ int, _ map[string]any) { gotReason = reason })
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
	sup.SetOnDegraded(func(reason string, _ int, _ map[string]any) { gotReason = reason })
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
	sup.SetOnDegraded(func(string, int, map[string]any) { called = true })
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

// TestSupervisor_ShutdownBeforeRespawn asserts that the supervisor calls
// Spawner.Shutdown on a child that failed WaitReady before re-Spawn. Production
// 2026-05-22 left 5 zombie tlm processes because this cleanup didn't happen.
func TestSupervisor_ShutdownBeforeRespawn(t *testing.T) {
	sp := &fakeSpawner{
		waitReadyErr: ErrReadyCeilingExceeded,
	}
	sup := NewSupervisor(sp, 3, nil)
	sup.SetOnDegraded(func(string, int, map[string]any) {})
	sup.testBackoff = func(int) time.Duration { return 1 * time.Millisecond }
	_ = sup.Run(context.Background())

	if sp.spawned.Load() < 3 {
		t.Fatalf("spawned=%d, want >=3", sp.spawned.Load())
	}
	if sp.shutdownCalls.Load() < int32(sp.spawned.Load()) {
		t.Fatalf("shutdownCalls=%d, want >= spawned (%d) — every failed-startup child must be Shutdown",
			sp.shutdownCalls.Load(), sp.spawned.Load())
	}
	// Every spawn (except possibly the very first) must be preceded by a shutdown.
	for i, op := range sp.shutdownObserved {
		if op == "spawn" && i > 0 && sp.shutdownObserved[i-1] != "shutdown" {
			t.Fatalf("ordering violation at index %d: spawn without preceding shutdown — %v",
				i, sp.shutdownObserved)
		}
	}
}

// TestSupervisor_BundleSchemaMismatchReason asserts that a sustained
// incompatible_bundle/no_manifest WaitReady error surfaces as
// ReasonBundleSchemaMismatch with detail{compatibility,sub_code,bundle_version},
// rather than the generic startup_timeout.
func TestSupervisor_BundleSchemaMismatchReason(t *testing.T) {
	sp := &fakeSpawner{
		waitReadyErrFn: func() error {
			return &ReadyTimeoutError{
				LastCompatibility: "incompatible",
				LastSubCode:       "no_manifest",
				LastBundleVersion: "",
			}
		},
	}
	var gotReason string
	var gotDetail map[string]any
	sup := NewSupervisor(sp, 5, nil)
	sup.SetOnDegraded(func(reason string, _ int, detail map[string]any) {
		gotReason = reason
		gotDetail = detail
	})
	sup.testBackoff = func(int) time.Duration { return 1 * time.Millisecond }
	_ = sup.Run(context.Background())

	if gotReason != ReasonBundleSchemaMismatch {
		t.Fatalf("reason=%q, want %q", gotReason, ReasonBundleSchemaMismatch)
	}
	if gotDetail["compatibility"] != "incompatible" {
		t.Fatalf("detail.compatibility=%v, want 'incompatible'", gotDetail["compatibility"])
	}
	if gotDetail["sub_code"] != "no_manifest" {
		t.Fatalf("detail.sub_code=%v, want 'no_manifest'", gotDetail["sub_code"])
	}
}

// TestSupervisor_OnIncompatibleFiresOnceThenShortCircuits asserts the one-shot
// self-heal hook + short-circuit semantics. Five-attempt budget but the
// supervisor must give up after exactly 2 spawns (initial + retry after hook
// fires once) when the schema mismatch persists.
func TestSupervisor_OnIncompatibleFiresOnceThenShortCircuits(t *testing.T) {
	sp := &fakeSpawner{
		waitReadyErrFn: func() error {
			return &ReadyTimeoutError{
				LastCompatibility: "incompatible",
				LastSubCode:       "no_manifest",
			}
		},
	}
	var healCalls int
	var gotReason string
	sup := NewSupervisor(sp, 5, nil)
	sup.SetOnIncompatible(func() { healCalls++ })
	sup.SetOnDegraded(func(reason string, _ int, _ map[string]any) { gotReason = reason })
	sup.testBackoff = func(int) time.Duration { return 1 * time.Millisecond }
	_ = sup.Run(context.Background())

	if healCalls != 1 {
		t.Fatalf("onIncompatible called %d times, want 1", healCalls)
	}
	if sp.spawned.Load() != 2 {
		t.Fatalf("spawned %d times, want 2 (initial + retry after self-heal)", sp.spawned.Load())
	}
	if gotReason != ReasonBundleSchemaMismatch {
		t.Fatalf("reason=%q, want %q", gotReason, ReasonBundleSchemaMismatch)
	}
}

// TestSupervisor_OnIncompatibleWithoutHookShortCircuitsImmediately asserts
// that when SetOnIncompatible is not registered (e.g. local-mode Service has
// no puller), the supervisor still short-circuits on confirmed mismatch
// instead of burning the full budget.
func TestSupervisor_OnIncompatibleWithoutHookShortCircuitsImmediately(t *testing.T) {
	sp := &fakeSpawner{
		waitReadyErrFn: func() error {
			return &ReadyTimeoutError{LastCompatibility: "incompatible", LastSubCode: "version_out_of_range"}
		},
	}
	sup := NewSupervisor(sp, 5, nil)
	sup.SetOnDegraded(func(string, int, map[string]any) {})
	sup.testBackoff = func(int) time.Duration { return 1 * time.Millisecond }
	_ = sup.Run(context.Background())

	if sp.spawned.Load() != 1 {
		t.Fatalf("spawned %d times, want 1 (no hook → short-circuit on first detection)", sp.spawned.Load())
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
