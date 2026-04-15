package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// fastTick keeps tests quick — 5ms tick, 20ms soft, 60ms hard scaled down
// from production 1s / 90s / 600s defaults.
const fastTick = 5 * time.Millisecond

func TestWatchdog_SoftFiresInIdlePhase(t *testing.T) {
	tr := newPhaseTracker()
	tr.Enter(PhaseAwaitingLLM)

	var softCount atomic.Int32
	var softPhase atomic.Int32
	stop := runWatchdogWithTick(context.Background(), tr,
		20*time.Millisecond, 0, fastTick,
		func(p TurnPhase, _ time.Duration) {
			softCount.Add(1)
			softPhase.Store(int32(p))
		},
		nil, nil)
	defer stop()

	time.Sleep(80 * time.Millisecond)
	if n := softCount.Load(); n != 1 {
		t.Fatalf("want exactly 1 soft fire, got %d", n)
	}
	if TurnPhase(softPhase.Load()) != PhaseAwaitingLLM {
		t.Fatalf("want AwaitingLLM, got %s", TurnPhase(softPhase.Load()))
	}
}

func TestWatchdog_ForceStopIsIdleCounted(t *testing.T) {
	tr := newPhaseTracker()
	tr.Enter(PhaseForceStop)

	var softCount atomic.Int32
	stop := runWatchdogWithTick(context.Background(), tr,
		20*time.Millisecond, 0, fastTick,
		func(TurnPhase, time.Duration) { softCount.Add(1) },
		nil, nil)
	defer stop()

	time.Sleep(80 * time.Millisecond)
	if n := softCount.Load(); n < 1 {
		t.Fatalf("ForceStop should be idle-counted; got %d soft fires", n)
	}
}

func TestWatchdog_NonIdlePhase_NoSoft(t *testing.T) {
	nonIdle := []TurnPhase{
		PhaseSetup, PhaseRetryingLLM, PhaseCompacting,
		PhaseAwaitingApproval, PhaseExecutingTools, PhaseInjectingMessage,
	}
	for _, p := range nonIdle {
		t.Run(p.String(), func(t *testing.T) {
			tr := newPhaseTracker()
			tr.Enter(p)
			var softCount atomic.Int32
			stop := runWatchdogWithTick(context.Background(), tr,
				20*time.Millisecond, 0, fastTick,
				func(TurnPhase, time.Duration) { softCount.Add(1) },
				nil, nil)
			defer stop()
			time.Sleep(80 * time.Millisecond)
			if n := softCount.Load(); n != 0 {
				t.Fatalf("non-idle phase %s fired soft %d times", p, n)
			}
		})
	}
}

func TestWatchdog_SoftOncePerPhaseInstance(t *testing.T) {
	tr := newPhaseTracker()
	tr.Enter(PhaseAwaitingLLM)

	var softCount atomic.Int32
	stop := runWatchdogWithTick(context.Background(), tr,
		20*time.Millisecond, 0, fastTick,
		func(TurnPhase, time.Duration) { softCount.Add(1) },
		nil, nil)
	defer stop()

	// Stay in AwaitingLLM well past the soft threshold. Should fire only once.
	time.Sleep(200 * time.Millisecond)
	if n := softCount.Load(); n != 1 {
		t.Fatalf("want exactly 1 soft fire in single phase instance, got %d", n)
	}
}

func TestWatchdog_RearmsOnPhaseTransition(t *testing.T) {
	tr := newPhaseTracker()
	tr.Enter(PhaseAwaitingLLM)

	var softCount atomic.Int32
	stop := runWatchdogWithTick(context.Background(), tr,
		20*time.Millisecond, 0, fastTick,
		func(TurnPhase, time.Duration) { softCount.Add(1) },
		nil, nil)
	defer stop()

	time.Sleep(60 * time.Millisecond) // first soft fires
	tr.Enter(PhaseRetryingLLM)        // leave idle-counted
	time.Sleep(30 * time.Millisecond) // no fires while retrying
	tr.Enter(PhaseAwaitingLLM)        // re-enter idle — seq bumps, re-arm
	time.Sleep(60 * time.Millisecond) // second soft fires

	if n := softCount.Load(); n != 2 {
		t.Fatalf("want 2 soft fires across two phase instances, got %d", n)
	}
}

func TestWatchdog_CompactionNestedLLMIsObservable(t *testing.T) {
	tr := newPhaseTracker()
	tr.Enter(PhaseCompacting) // outer phase — not idle-counted

	var softCount atomic.Int32
	var observedPhase atomic.Int32
	stop := runWatchdogWithTick(context.Background(), tr,
		20*time.Millisecond, 0, fastTick,
		func(p TurnPhase, _ time.Duration) {
			softCount.Add(1)
			observedPhase.Store(int32(p))
		},
		nil, nil)
	defer stop()

	time.Sleep(40 * time.Millisecond) // outer Compacting: no fire

	// Nested LLM call inside compaction must be idle-watched.
	restore := tr.EnterTransient(PhaseAwaitingLLM)
	time.Sleep(60 * time.Millisecond)
	restore()

	if n := softCount.Load(); n != 1 {
		t.Fatalf("nested AwaitingLLM inside Compacting should fire soft once, got %d", n)
	}
	if TurnPhase(observedPhase.Load()) != PhaseAwaitingLLM {
		t.Fatalf("observed phase %s, want AwaitingLLM", TurnPhase(observedPhase.Load()))
	}
}

func TestWatchdog_HardCancelsWithCause(t *testing.T) {
	tr := newPhaseTracker()
	tr.Enter(PhaseAwaitingLLM)

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	var hardCount atomic.Int32
	stop := runWatchdogWithTick(ctx, tr,
		0, 40*time.Millisecond, fastTick,
		nil,
		func(TurnPhase, time.Duration) { hardCount.Add(1) },
		func(err error) { cancel(err) })
	defer stop()

	select {
	case <-ctx.Done():
		// ok
	case <-time.After(500 * time.Millisecond):
		t.Fatal("ctx should have been cancelled by hard watchdog")
	}
	if !errors.Is(context.Cause(ctx), ErrHardIdleTimeout) {
		t.Fatalf("want ErrHardIdleTimeout, got: %v", context.Cause(ctx))
	}
	if n := hardCount.Load(); n != 1 {
		t.Fatalf("want 1 onHard call, got %d", n)
	}
}

func TestWatchdog_Zero_IsNoop(t *testing.T) {
	tr := newPhaseTracker()
	tr.Enter(PhaseAwaitingLLM)

	var fired atomic.Int32
	stop := runWatchdogWithTick(context.Background(), tr, 0, 0, fastTick,
		func(TurnPhase, time.Duration) { fired.Add(1) },
		func(TurnPhase, time.Duration) { fired.Add(1) },
		nil)
	defer stop()

	time.Sleep(80 * time.Millisecond)
	if n := fired.Load(); n != 0 {
		t.Fatalf("disabled watchdog must not fire, got %d", n)
	}
}

func TestWatchdog_InvalidTracker_SelfDisables(t *testing.T) {
	tr := newPhaseTracker()
	tr.Enter(PhaseAwaitingLLM)

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	var softCount atomic.Int32
	var hardCount atomic.Int32
	var cancelled atomic.Int32
	stop := runWatchdogWithTick(ctx, tr,
		20*time.Millisecond, 40*time.Millisecond, fastTick,
		func(TurnPhase, time.Duration) { softCount.Add(1) },
		func(TurnPhase, time.Duration) { hardCount.Add(1) },
		func(err error) { cancelled.Add(1); cancel(err) })
	defer stop()

	// Corrupt the tracker by directly setting invalid.
	tr.invalid.Store(true)

	time.Sleep(100 * time.Millisecond)
	if softCount.Load() != 0 || hardCount.Load() != 0 || cancelled.Load() != 0 {
		t.Fatalf("invalid tracker must self-disable: soft=%d hard=%d cancel=%d",
			softCount.Load(), hardCount.Load(), cancelled.Load())
	}
	if ctx.Err() != nil {
		t.Fatalf("ctx must not be cancelled when tracker invalid: cause=%v", context.Cause(ctx))
	}
}

func TestWatchdog_StopReleasesGoroutine(t *testing.T) {
	tr := newPhaseTracker()
	tr.Enter(PhaseAwaitingLLM)

	stop := runWatchdogWithTick(context.Background(), tr,
		10*time.Second, 0, fastTick, // would not fire in 20ms
		nil, nil, nil)

	done := make(chan struct{})
	go func() { stop(); close(done) }()

	select {
	case <-done:
		// ok
	case <-time.After(200 * time.Millisecond):
		t.Fatal("stop() did not return — goroutine leak")
	}
}
