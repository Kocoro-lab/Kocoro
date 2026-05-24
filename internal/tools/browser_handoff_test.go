package tools

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestHandBrowserOff_NoLeases_CleansEagerly(t *testing.T) {
	bt := &BrowserTool{}
	HandBrowserOff(bt, 50*time.Millisecond)
	if !bt.IsDeprecated() {
		t.Fatalf("OLD must be marked deprecated")
	}
	if got := bt.CleanupCalledForTest(); got != 1 {
		t.Fatalf("eager Cleanup expected once; got %d", got)
	}
}

func TestHandBrowserOff_WithActiveLease_DefersCleanup(t *testing.T) {
	bt := &BrowserTool{}
	// Acquire a lease against bt to simulate an in-flight Run.
	lease := newBrowserUseLeaseWithTracker(globalBrowserTracker)
	lease.MarkUsedWith(bt)
	defer lease.ReleaseOnly() // exercise per-owner cleanup of test state

	HandBrowserOff(bt, 50*time.Millisecond)

	// Watchdog hasn't fired yet — Cleanup must not have run.
	if got := bt.CleanupCalledForTest(); got != 0 {
		t.Fatalf("Cleanup must NOT fire while lease is active; got %d calls", got)
	}
	if !bt.IsDeprecated() {
		t.Fatalf("OLD must be marked deprecated")
	}

	// Wait past the watchdog interval.
	time.Sleep(150 * time.Millisecond)

	// Watchdog fired, saw count>0, refused to call Cleanup.
	if got := bt.CleanupCalledForTest(); got != 0 {
		t.Fatalf("watchdog with active lease must NOT call Cleanup; got %d calls", got)
	}
	if got := BrowserOwnerActiveCount(bt); got != 1 {
		t.Fatalf("lease must still be active; owners[bt] = %d", got)
	}
}

func TestHandBrowserOff_WatchdogAfterDrainCleans(t *testing.T) {
	bt := &BrowserTool{}
	lease := newBrowserUseLeaseWithTracker(globalBrowserTracker)
	lease.MarkUsedWith(bt)

	HandBrowserOff(bt, 100*time.Millisecond)

	// Release the lease BEFORE the watchdog fires. Mimic the deprecated lease
	// path: caller passes Cleanup() as teardown callback.
	lease.ReleaseAndMaybeTeardown(func() error {
		bt.Cleanup()
		return nil
	})
	if got := bt.CleanupCalledForTest(); got != 1 {
		t.Fatalf("lease release with deprecated callback must Cleanup once; got %d", got)
	}

	// Watchdog fires later. owners[bt] is 0 now; the spec says watchdog
	// calls Cleanup again. Cleanup is idempotent — second call is fine.
	time.Sleep(200 * time.Millisecond)
	if got := bt.CleanupCalledForTest(); got < 1 {
		t.Fatalf("Cleanup must have run at least once total; got %d", got)
	}
}

func TestHandBrowserOff_NilBrowser_NoOp(t *testing.T) {
	// Helper must tolerate nil OLD (when reg.Get fails to find a browser).
	HandBrowserOff(nil, 50*time.Millisecond)
	// No panic = pass.
}

func TestReloadBrowserHandoff_MixedOldNewConcurrent(t *testing.T) {
	// Pre-reload: lease A on OLD. Post-reload: lease B on NEW.
	// OLD and NEW each cleaned up when their own lease releases.
	// Locks in the v3 three-way-gate bug as a regression test.
	oldBT := &BrowserTool{}
	newBT := &BrowserTool{}

	ctxOld := WithBrowserUseLease(context.Background())
	MarkBrowserUsed(ctxOld, oldBT)

	// Simulate reload: mark OLD deprecated; NEW takes its place.
	oldBT.MarkDeprecated()

	ctxNew := WithBrowserUseLease(context.Background())
	MarkBrowserUsed(ctxNew, newBT)

	if BrowserOwnerActiveCount(oldBT) != 1 {
		t.Fatalf("owners[oldBT] = %d, want 1", BrowserOwnerActiveCount(oldBT))
	}
	if BrowserOwnerActiveCount(newBT) != 1 {
		t.Fatalf("owners[newBT] = %d, want 1", BrowserOwnerActiveCount(newBT))
	}

	// Release NEW first: per-owner gate fires NEW's teardown even though
	// OLD's lease is still active (the v3 bug would have left NEW leaked).
	var newCleanupFired, oldCleanupFired int
	leaseNew := BrowserUseLeaseFrom(ctxNew)
	leaseNew.ReleaseAndMaybeTeardown(func() error { newCleanupFired++; return nil })
	if newCleanupFired != 1 {
		t.Fatalf("NEW teardown must fire on release with per-owner gate; fired=%d", newCleanupFired)
	}

	// Release OLD: deprecated path, per-owner gate fires.
	leaseOld := BrowserUseLeaseFrom(ctxOld)
	leaseOld.ReleaseAndMaybeTeardown(func() error { oldCleanupFired++; return nil })
	if oldCleanupFired != 1 {
		t.Fatalf("OLD teardown must fire on release; fired=%d", oldCleanupFired)
	}
}

func TestHandBrowserOff_RaceCleanupVsMarkUsed(t *testing.T) {
	// Regression for the TOCTOU race between HandBrowserOff's zero-count
	// fast-path and a concurrent MarkBrowserUsed from an in-flight Run that
	// captured OLD via a pre-reload registry clone. Under -race, an
	// interleaving where MarkUsedWith landed between the count read and
	// the Cleanup write would either trigger a race report (if the same
	// memory was touched) or, more importantly, leave the lease counter
	// inconsistent (zombie owner entry after cleanup). With the atomic
	// check+cleanup the race is impossible: either the lease wins (and
	// Cleanup is deferred to the watchdog/teardown path) or HandBrowserOff
	// wins (and MarkUsedWith observes a clean post-cleanup tracker state).
	//
	// We can't make this test deterministic because the race window is
	// small, but -count=N catches it probabilistically.
	for i := 0; i < 50; i++ {
		bt := &BrowserTool{}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			HandBrowserOff(bt, 50*time.Millisecond)
		}()
		go func() {
			defer wg.Done()
			ctx := WithBrowserUseLease(context.Background())
			MarkBrowserUsed(ctx, bt)
			// Simulate Run completing; release the lease so we don't leak owners[bt].
			lease := BrowserUseLeaseFrom(ctx)
			lease.ReleaseAndMaybeTeardown(nil)
		}()
		wg.Wait()
		// Final state: owners[bt] must be 0 regardless of which goroutine
		// raced first. Any non-zero count means the lease release didn't
		// see its prior MarkUsedWith — i.e. inconsistency.
		if got := BrowserOwnerActiveCount(bt); got != 0 {
			t.Fatalf("iter %d: owners[bt] = %d after race, want 0", i, got)
		}
		if !bt.IsDeprecated() {
			t.Fatalf("iter %d: bt must be deprecated", i)
		}
	}
}
