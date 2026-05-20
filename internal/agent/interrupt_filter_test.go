package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agenttypes"
)

func TestInterruptFilteredContext_SwallowsInterrupt(t *testing.T) {
	parent, parentCancel := context.WithCancelCause(context.Background())
	child, childCancel := InterruptFilteredContext(parent)
	defer childCancel()

	parentCancel(agenttypes.NewCancelError(agenttypes.ReasonInterrupt))

	// Give the watcher goroutine a moment.
	select {
	case <-child.Done():
		t.Fatalf("child ctx should NOT be cancelled by Interrupt; got %v", context.Cause(child))
	case <-time.After(25 * time.Millisecond):
		// good — child is still alive
	}
}

func TestInterruptFilteredContext_PropagatesUserCancel(t *testing.T) {
	parent, parentCancel := context.WithCancelCause(context.Background())
	child, childCancel := InterruptFilteredContext(parent)
	defer childCancel()

	parentCancel(agenttypes.NewCancelError(agenttypes.ReasonUserCancel))

	select {
	case <-child.Done():
		// good
	case <-time.After(100 * time.Millisecond):
		t.Fatal("UserCancel should propagate to child within 100ms")
	}

	cause := context.Cause(child)
	r, ok := agenttypes.ExtractReason(cause)
	if !ok || r != agenttypes.ReasonUserCancel {
		t.Errorf("propagated cause: want UserCancel, got reason=%v ok=%v cause=%v", r, ok, cause)
	}
}

func TestInterruptFilteredContext_PropagatesUnrelatedCancel(t *testing.T) {
	parent, parentCancel := context.WithCancelCause(context.Background())
	child, childCancel := InterruptFilteredContext(parent)
	defer childCancel()

	plainErr := errors.New("plain unrelated err")
	parentCancel(plainErr)

	select {
	case <-child.Done():
		if !errors.Is(context.Cause(child), plainErr) {
			t.Errorf("unrelated err should pass through, got %v", context.Cause(child))
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("unrelated cancel should propagate to child")
	}
}

func TestInterruptFilteredContext_PreservesParentValues(t *testing.T) {
	type ctxKey string
	const k ctxKey = "cwd"
	parent := context.WithValue(context.Background(), k, "/projects/foo")

	child, cancel := InterruptFilteredContext(parent)
	defer cancel()

	got, ok := child.Value(k).(string)
	if !ok || got != "/projects/foo" {
		t.Errorf("child must inherit parent values, got %q ok=%v", got, ok)
	}
}

func TestInterruptFilteredContext_ChildCancelStopsWatcher(t *testing.T) {
	parent := context.Background()
	child, cancel := InterruptFilteredContext(parent)
	cancel()

	<-child.Done()
	// Should not deadlock — the watcher exits when child.Done() fires.
}

func TestInterruptFilteredContext_CancelFuncIsConcurrentSafe(t *testing.T) {
	for attempt := 0; attempt < 200; attempt++ {
		_, cancel := InterruptFilteredContext(context.Background())
		start := make(chan struct{})
		panicCh := make(chan any, 32)
		var wg sync.WaitGroup

		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						panicCh <- r
					}
				}()
				<-start
				cancel()
			}()
		}

		close(start)
		wg.Wait()
		close(panicCh)
		for p := range panicCh {
			t.Fatalf("cancel func panicked under concurrent calls on attempt %d: %v", attempt, p)
		}
	}
}
