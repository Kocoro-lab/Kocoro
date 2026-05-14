package daemon

import (
	"fmt"
	"sync"
	"testing"
)

func TestApprovalTracker_RefCountAcrossParallelMarks(t *testing.T) {
	tr := NewApprovalTracker()
	tr.Mark("sess-1")
	tr.Mark("sess-1")
	tr.Clear("sess-1")
	// One pending approval still in flight — must still report awaiting.
	if !tr.IsAwaiting("sess-1") {
		t.Fatal("IsAwaiting should remain true after one Clear when two Marks were issued")
	}
	tr.Clear("sess-1")
	if tr.IsAwaiting("sess-1") {
		t.Fatal("IsAwaiting should return false after matching Clear count")
	}
	if got := tr.SessionIDs(); got != nil {
		t.Fatalf("SessionIDs should return nil when empty, got %v", got)
	}
}

func TestApprovalTracker_ClearWithoutMarkIsNoop(t *testing.T) {
	tr := NewApprovalTracker()
	tr.Clear("sess-1") // must not panic, must not create a negative entry
	if tr.IsAwaiting("sess-1") {
		t.Fatal("unmatched Clear must not flip IsAwaiting on")
	}
	tr.Mark("sess-1")
	tr.Clear("sess-1")
	tr.Clear("sess-1") // second Clear when count==0 must stay no-op
	if tr.IsAwaiting("sess-1") {
		t.Fatal("over-clear must not corrupt state")
	}
}

func TestApprovalTracker_EmptySessionIDIsNoop(t *testing.T) {
	tr := NewApprovalTracker()
	tr.Mark("")  // non-routed paths pass ""; must be ignored.
	tr.Clear("") // symmetrical.
	if got := tr.SessionIDs(); got != nil {
		t.Fatalf("empty sessionID Mark must not add an entry; got %v", got)
	}
}

func TestApprovalTracker_NilReceiverIsSafe(t *testing.T) {
	var tr *ApprovalTracker
	// Daemon handlers may receive a nil tracker when deps is constructed
	// outside NewServer (older test fixtures). Methods must stay no-op.
	tr.Mark("sess-1")
	tr.Clear("sess-1")
	if tr.IsAwaiting("sess-1") {
		t.Fatal("nil tracker must always report not-awaiting")
	}
	if got := tr.SessionIDs(); got != nil {
		t.Fatalf("nil tracker must return nil SessionIDs, got %v", got)
	}
	if got := tr.AwaitingSet(); got != nil {
		t.Fatalf("nil tracker must return nil AwaitingSet, got %v", got)
	}
}

func TestApprovalTracker_ConcurrentMarkClear(t *testing.T) {
	// Run with -race: parallel Mark/Clear from N goroutines must settle to
	// an empty tracker. Race detector catches any missing lock.
	tr := NewApprovalTracker()
	const sessions = 8
	const iters = 200
	var wg sync.WaitGroup
	for s := 0; s < sessions; s++ {
		id := fmt.Sprintf("sess-%d", s)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				tr.Mark(id)
				tr.Clear(id)
			}
		}()
	}
	wg.Wait()
	if got := tr.SessionIDs(); got != nil {
		t.Fatalf("after balanced Mark/Clear, tracker should be empty; got %v", got)
	}
}

func TestApprovalTracker_SessionIDsAndAwaitingSet(t *testing.T) {
	tr := NewApprovalTracker()
	tr.Mark("a")
	tr.Mark("b")
	tr.Mark("a") // a has refcount 2

	ids := tr.SessionIDs()
	if len(ids) != 2 {
		t.Fatalf("SessionIDs should return both sessions, got %v", ids)
	}
	set := tr.AwaitingSet()
	if _, ok := set["a"]; !ok {
		t.Fatal("AwaitingSet missing session a")
	}
	if _, ok := set["b"]; !ok {
		t.Fatal("AwaitingSet missing session b")
	}

	tr.Clear("a") // a still awaiting (refcount 1)
	if !tr.IsAwaiting("a") {
		t.Fatal("a should still be awaiting after one of two Clears")
	}
	tr.Clear("a")
	tr.Clear("b")
	if got := tr.SessionIDs(); got != nil {
		t.Fatalf("expected empty tracker, got %v", got)
	}
}
