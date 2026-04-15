package agent

import (
	"sync"
	"testing"
	"time"
)

func TestTurnPhase_CountsAsIdle(t *testing.T) {
	idle := map[TurnPhase]bool{
		PhaseAwaitingLLM: true,
		PhaseForceStop:   true,
	}
	all := []TurnPhase{
		PhaseInit, PhaseSetup, PhaseAwaitingLLM, PhaseRetryingLLM,
		PhaseCompacting, PhaseAwaitingApproval, PhaseExecutingTools,
		PhaseInjectingMessage, PhaseForceStop, PhaseDone,
	}
	for _, p := range all {
		want := idle[p]
		if got := p.CountsAsIdle(); got != want {
			t.Errorf("%s.CountsAsIdle() = %v, want %v", p, got, want)
		}
	}
}

func TestTurnPhase_String(t *testing.T) {
	cases := map[TurnPhase]string{
		PhaseInit: "init", PhaseSetup: "setup", PhaseAwaitingLLM: "awaiting_llm",
		PhaseRetryingLLM: "retrying_llm", PhaseCompacting: "compacting",
		PhaseAwaitingApproval: "awaiting_approval", PhaseExecutingTools: "executing_tools",
		PhaseInjectingMessage: "injecting_message", PhaseForceStop: "force_stop",
		PhaseDone: "done",
	}
	for p, want := range cases {
		if got := p.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int(p), got, want)
		}
	}
	if got := TurnPhase(999).String(); got != "unknown" {
		t.Errorf("unknown phase: %q", got)
	}
}

func TestPhaseTracker_EnterAndCurrent(t *testing.T) {
	tr := newPhaseTracker()
	p, _, _ := tr.Current()
	if p != PhaseInit {
		t.Fatalf("initial phase = %s, want init", p)
	}
	tr.Enter(PhaseAwaitingLLM)
	p, d, _ := tr.Current()
	if p != PhaseAwaitingLLM {
		t.Fatalf("after Enter: phase = %s", p)
	}
	if d < 0 || d > time.Second {
		t.Fatalf("since-time unreasonable: %v", d)
	}
}

func TestPhaseTracker_EnterTransient_RestoresPrev(t *testing.T) {
	tr := newPhaseTracker()
	tr.Enter(PhaseCompacting)

	restore := tr.EnterTransient(PhaseAwaitingLLM)
	p, _, _ := tr.Current()
	if p != PhaseAwaitingLLM {
		t.Fatalf("inside transient: phase = %s", p)
	}

	restore()
	p, _, _ = tr.Current()
	if p != PhaseCompacting {
		t.Fatalf("after restore: phase = %s, want compacting", p)
	}
}

func TestPhaseTracker_EnterTransient_NestedDoesNotLeak(t *testing.T) {
	tr := newPhaseTracker()
	tr.Enter(PhaseCompacting)

	r1 := tr.EnterTransient(PhaseAwaitingLLM)
	if p, _, _ := tr.Current(); p != PhaseAwaitingLLM {
		t.Fatalf("first transient: %s", p)
	}

	// Imagine a nested call also needing AwaitingLLM (uncommon but legal).
	r2 := tr.EnterTransient(PhaseAwaitingLLM)
	r2()
	if p, _, _ := tr.Current(); p != PhaseAwaitingLLM {
		t.Fatalf("after inner restore, outer transient lost: %s", p)
	}

	r1()
	if p, _, _ := tr.Current(); p != PhaseCompacting {
		t.Fatalf("after outer restore: %s, want compacting", p)
	}
	tr.AssertClean() // depth should be 0
}

func TestPhaseTracker_RestoreIdempotent(t *testing.T) {
	tr := newPhaseTracker()
	tr.Enter(PhaseSetup)
	restore := tr.EnterTransient(PhaseAwaitingLLM)
	restore()
	restore() // second call must not underflow depth or change phase
	if p, _, _ := tr.Current(); p != PhaseSetup {
		t.Fatalf("after double restore: %s", p)
	}
	tr.AssertClean()
}

func TestPhaseTracker_SeqBumpsOnEveryTransition(t *testing.T) {
	tr := newPhaseTracker()
	_, _, s0 := tr.Current()

	tr.Enter(PhaseAwaitingLLM)
	_, _, s1 := tr.Current()
	if s1 <= s0 {
		t.Fatalf("seq did not bump on Enter: s0=%d s1=%d", s0, s1)
	}

	// Re-entering the same phase type must still bump seq (so observers can
	// re-arm dedupes on transition, not phase-type identity).
	tr.Enter(PhaseAwaitingLLM)
	_, _, s2 := tr.Current()
	if s2 <= s1 {
		t.Fatalf("seq must bump on same-phase re-entry: s1=%d s2=%d", s1, s2)
	}

	// EnterTransient bumps; restore bumps again.
	restore := tr.EnterTransient(PhaseAwaitingLLM)
	_, _, s3 := tr.Current()
	if s3 <= s2 {
		t.Fatalf("seq did not bump on EnterTransient: s2=%d s3=%d", s2, s3)
	}
	restore()
	_, _, s4 := tr.Current()
	if s4 <= s3 {
		t.Fatalf("seq did not bump on transient restore: s3=%d s4=%d", s3, s4)
	}
}

func TestPhaseTracker_InvalidFlag(t *testing.T) {
	tr := newPhaseTracker()
	if tr.Invalid() {
		t.Fatal("new tracker should not be invalid")
	}

	// Trigger a violation via forgotten restore + AssertClean. Under
	// testing.Testing() this panics, so guard + recover.
	_ = tr.EnterTransient(PhaseAwaitingLLM) // intentionally drop
	func() {
		defer func() { _ = recover() }()
		tr.AssertClean()
	}()
	if !tr.Invalid() {
		t.Fatal("expected tracker to be marked invalid after violation")
	}
}

func TestPhaseTracker_AssertClean_DetectsForgottenRestore(t *testing.T) {
	tr := newPhaseTracker()
	_ = tr.EnterTransient(PhaseAwaitingLLM) // intentionally drop restore

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("AssertClean should have panicked for forgotten transient")
		}
	}()
	tr.AssertClean()
}

func TestPhaseTracker_Enter_PanicsInsideTransient(t *testing.T) {
	tr := newPhaseTracker()
	tr.Enter(PhaseSetup)
	restore := tr.EnterTransient(PhaseAwaitingLLM)
	defer restore()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Enter inside active transient should panic in test mode")
		}
	}()
	tr.Enter(PhaseExecutingTools) // violates layering
}

func TestPhaseTracker_Dirty(t *testing.T) {
	tr := newPhaseTracker()
	if tr.TakeDirty() {
		t.Fatal("new tracker should not be dirty")
	}
	tr.MarkDirty()
	if !tr.TakeDirty() {
		t.Fatal("MarkDirty should set dirty")
	}
	if tr.TakeDirty() {
		t.Fatal("TakeDirty should clear on read")
	}
}

func TestPhaseTracker_ConcurrentReadDuringWrite(t *testing.T) {
	tr := newPhaseTracker()
	tr.Enter(PhaseSetup)

	const N = 200
	var wg sync.WaitGroup
	wg.Add(2)

	// Writer: flip phases rapidly.
	go func() {
		defer wg.Done()
		phases := []TurnPhase{PhaseAwaitingLLM, PhaseExecutingTools, PhaseSetup}
		for i := 0; i < N; i++ {
			tr.Enter(phases[i%len(phases)])
		}
	}()

	// Reader: poll concurrently, verify return values are well-typed (no torn reads).
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			p, d, seq := tr.Current()
			if p < PhaseInit || p > PhaseDone {
				t.Errorf("torn phase read: %d", int(p))
				return
			}
			if d < 0 {
				t.Errorf("negative since-duration: %v", d)
				return
			}
			if seq < 0 {
				t.Errorf("negative seq: %d", seq)
				return
			}
		}
	}()

	wg.Wait()
}

func TestPhaseTracker_SinceRearms(t *testing.T) {
	tr := newPhaseTracker()
	tr.Enter(PhaseAwaitingLLM)
	time.Sleep(5 * time.Millisecond)
	_, d1, _ := tr.Current()
	if d1 < 5*time.Millisecond {
		t.Fatalf("since should be >= 5ms, got %v", d1)
	}

	tr.Enter(PhaseAwaitingLLM) // same phase re-entered
	_, d2, _ := tr.Current()
	if d2 > d1 {
		t.Fatalf("re-entry should reset since: got %v, prior was %v", d2, d1)
	}
}
