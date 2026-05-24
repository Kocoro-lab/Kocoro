package tools

import (
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
