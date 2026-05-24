package tools

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBrowserUseLease_MarkUsedIncrementsTracker(t *testing.T) {
	tr := &browserUseTracker{}
	if got := tr.activeCount(); got != 0 {
		t.Fatalf("expected initial count 0, got %d", got)
	}

	lease := newBrowserUseLeaseWithTracker(tr)
	lease.MarkUsed()
	if got := tr.activeCount(); got != 1 {
		t.Fatalf("expected count 1 after MarkUsed, got %d", got)
	}

	// Idempotent: second MarkUsed must not increment.
	lease.MarkUsed()
	if got := tr.activeCount(); got != 1 {
		t.Fatalf("expected count still 1 after second MarkUsed, got %d", got)
	}
}

func TestBrowserUseLease_ReleaseOnlyDecrementsWithoutTeardown(t *testing.T) {
	tr := &browserUseTracker{}
	lease := newBrowserUseLeaseWithTracker(tr)
	lease.MarkUsed()
	lease.ReleaseOnly()
	if got := tr.activeCount(); got != 0 {
		t.Fatalf("expected count 0 after ReleaseOnly, got %d", got)
	}

	// Idempotent: second ReleaseOnly does not decrement below 0.
	lease.ReleaseOnly()
	if got := tr.activeCount(); got != 0 {
		t.Fatalf("expected count still 0, got %d", got)
	}
}

func TestBrowserUseLease_ReleaseWithoutAcquireNoOp(t *testing.T) {
	tr := &browserUseTracker{}
	lease := newBrowserUseLeaseWithTracker(tr)
	lease.ReleaseOnly()
	if got := tr.activeCount(); got != 0 {
		t.Fatalf("expected count 0 when releasing without acquire, got %d", got)
	}
}

func TestBrowserUseLease_ReleaseBeforeAcquireDoesNotLeak(t *testing.T) {
	tr := &browserUseTracker{}
	lease := newBrowserUseLeaseWithTracker(tr)

	lease.ReleaseOnly()
	lease.MarkUsed() // late acquire — must not leak the counter
	if got := tr.activeCount(); got != 0 {
		t.Fatalf("counter leaked after Release-then-MarkUsed: got %d", got)
	}
}

func TestBrowserUseLease_RaceAcquireRelease(t *testing.T) {
	const leases = 200
	tr := &browserUseTracker{}
	var wg sync.WaitGroup
	for range leases {
		lease := newBrowserUseLeaseWithTracker(tr)
		wg.Add(2)
		go func() { defer wg.Done(); lease.MarkUsed() }()
		go func() { defer wg.Done(); lease.ReleaseOnly() }()
	}
	wg.Wait()
	if got := tr.activeCount(); got != 0 {
		t.Fatalf("counter leaked under race: got %d", got)
	}
}

func TestMarkBrowserUsed_ViaContextLease(t *testing.T) {
	tr := &browserUseTracker{}
	lease := newBrowserUseLeaseWithTracker(tr)
	ctx := context.WithValue(context.Background(), browserLeaseKey{}, lease)

	MarkBrowserUsed(ctx)
	if got := tr.activeCount(); got != 1 {
		t.Fatalf("expected count 1 after MarkBrowserUsed, got %d", got)
	}

	// MarkBrowserUsed on a ctx without a lease is a safe no-op.
	MarkBrowserUsed(context.Background())
	if got := tr.activeCount(); got != 1 {
		t.Fatalf("expected count still 1, got %d", got)
	}
}

func TestBrowserUseLeaseFrom_MissingReturnsNil(t *testing.T) {
	if got := BrowserUseLeaseFrom(context.Background()); got != nil {
		t.Fatalf("expected nil for ctx without lease, got %v", got)
	}
}

func TestWithBrowserUseLease_InstallsFreshLease(t *testing.T) {
	ctx := WithBrowserUseLease(context.Background())
	lease := BrowserUseLeaseFrom(ctx)
	if lease == nil {
		t.Fatal("expected lease installed on ctx, got nil")
	}
	if lease.tracker != globalBrowserTracker {
		t.Fatal("expected lease bound to globalBrowserTracker")
	}
}

func TestBrowserReleaseAndMaybeTeardown_LastReleaseRunsTeardown(t *testing.T) {
	tr := &browserUseTracker{}
	lease := newBrowserUseLeaseWithTracker(tr)
	lease.MarkUsed()

	calls := 0
	torndown, err := lease.ReleaseAndMaybeTeardown(func() error { calls++; return nil })
	if !torndown {
		t.Fatal("expected torndown=true")
	}
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected teardown called once, got %d", calls)
	}
	if got := tr.activeCount(); got != 0 {
		t.Fatalf("expected count 0 after teardown, got %d", got)
	}
}

func TestBrowserReleaseAndMaybeTeardown_NotLastSkipsTeardown(t *testing.T) {
	tr := &browserUseTracker{}
	leaseA := newBrowserUseLeaseWithTracker(tr)
	leaseB := newBrowserUseLeaseWithTracker(tr)
	leaseA.MarkUsed()
	leaseB.MarkUsed()

	calls := 0
	teardown := func() error { calls++; return nil }

	torndown, _ := leaseA.ReleaseAndMaybeTeardown(teardown)
	if torndown {
		t.Fatal("expected torndown=false when another lease holds")
	}
	if calls != 0 {
		t.Fatalf("expected no teardown, got %d", calls)
	}
	if got := tr.activeCount(); got != 1 {
		t.Fatalf("expected count 1, got %d", got)
	}

	torndown, _ = leaseB.ReleaseAndMaybeTeardown(teardown)
	if !torndown {
		t.Fatal("expected torndown=true on final release")
	}
	if calls != 1 {
		t.Fatalf("expected teardown called once, got %d", calls)
	}
}

func TestBrowserReleaseAndMaybeTeardown_HoldsLockAcrossTeardown(t *testing.T) {
	tr := &browserUseTracker{}
	leaseA := newBrowserUseLeaseWithTracker(tr)
	leaseA.MarkUsed()

	teardownStarted := make(chan struct{})
	teardownRelease := make(chan struct{})
	teardown := func() error {
		close(teardownStarted)
		<-teardownRelease
		return nil
	}

	releaseDone := make(chan struct{})
	go func() {
		_, _ = leaseA.ReleaseAndMaybeTeardown(teardown)
		close(releaseDone)
	}()

	<-teardownStarted

	// Concurrent MarkUsed must wait — it competes for tracker.mu, which
	// ReleaseAndMaybeTeardown holds during teardown.
	acquireDone := make(chan struct{})
	leaseB := newBrowserUseLeaseWithTracker(tr)
	go func() {
		leaseB.MarkUsed()
		close(acquireDone)
	}()

	select {
	case <-acquireDone:
		t.Fatal("MarkUsed returned while teardown was running — lock not held")
	case <-time.After(150 * time.Millisecond):
	}

	close(teardownRelease)
	<-releaseDone
	<-acquireDone

	if got := tr.activeCount(); got != 1 {
		t.Fatalf("expected count 1 after relaunch acquire, got %d", got)
	}
}

func TestBrowserReleaseAndMaybeTeardown_Idempotent(t *testing.T) {
	tr := &browserUseTracker{}
	lease := newBrowserUseLeaseWithTracker(tr)
	lease.MarkUsed()

	calls := 0
	teardown := func() error { calls++; return nil }

	_, _ = lease.ReleaseAndMaybeTeardown(teardown)
	_, _ = lease.ReleaseAndMaybeTeardown(teardown)

	if calls != 1 {
		t.Fatalf("expected exactly 1 teardown invocation, got %d", calls)
	}
}

func TestBrowserReleaseAndMaybeTeardown_NilLeaseNoOp(t *testing.T) {
	var lease *BrowserUseLease
	torndown, err := lease.ReleaseAndMaybeTeardown(func() error { return nil })
	if torndown {
		t.Fatal("expected torndown=false for nil lease")
	}
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
}

func TestBrowserReleaseAndMaybeTeardown_NilTeardownSafe(t *testing.T) {
	tr := &browserUseTracker{}
	lease := newBrowserUseLeaseWithTracker(tr)
	lease.MarkUsed()
	torndown, err := lease.ReleaseAndMaybeTeardown(nil)
	if !torndown {
		t.Fatal("expected torndown=true even with nil callback")
	}
	if err != nil {
		t.Fatalf("expected nil err with nil callback, got %v", err)
	}
	if got := tr.activeCount(); got != 0 {
		t.Fatalf("expected count 0, got %d", got)
	}
}

func TestBrowserReleaseAndMaybeTeardown_PropagatesTeardownError(t *testing.T) {
	tr := &browserUseTracker{}
	lease := newBrowserUseLeaseWithTracker(tr)
	lease.MarkUsed()

	wantErr := errors.New("chrome still alive")
	torndown, err := lease.ReleaseAndMaybeTeardown(func() error { return wantErr })
	if !torndown {
		t.Fatal("expected torndown=true even on teardown error")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected wantErr, got %v", err)
	}
}

func TestBrowserTeardownIfOnlyUser_SkipsWhenAnotherRunActive(t *testing.T) {
	tr := &browserUseTracker{}
	leaseA := newBrowserUseLeaseWithTracker(tr)
	leaseB := newBrowserUseLeaseWithTracker(tr)
	leaseA.MarkUsed()
	leaseB.MarkUsed()

	calls := 0
	torndown, skipped, err := leaseA.TeardownIfOnlyUser(func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if torndown {
		t.Fatal("expected torndown=false when another run is active")
	}
	if !skipped {
		t.Fatal("expected skipped=true when another run is active")
	}
	if calls != 0 {
		t.Fatalf("expected teardown not called, got %d calls", calls)
	}
	if got := tr.activeCount(); got != 2 {
		t.Fatalf("expected count unchanged at 2, got %d", got)
	}
}

func TestBrowserTeardownIfOnlyUser_RunsWithoutReleasingLease(t *testing.T) {
	tr := &browserUseTracker{}
	lease := newBrowserUseLeaseWithTracker(tr)
	lease.MarkUsed()

	calls := 0
	torndown, skipped, err := lease.TeardownIfOnlyUser(func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !torndown || skipped {
		t.Fatalf("expected torndown=true skipped=false, got %v %v", torndown, skipped)
	}
	if calls != 1 {
		t.Fatalf("expected teardown called once, got %d", calls)
	}
	if got := tr.activeCount(); got != 1 {
		t.Fatalf("expected lease count to remain 1 until end-of-turn release, got %d", got)
	}
}

func TestBrowserToolCleanupChromedp_PinchtabDoesNotKillChromedp(t *testing.T) {
	orig := killChromedpChromeForDirFn
	defer func() { killChromedpChromeForDirFn = orig }()

	calls := 0
	killChromedpChromeForDirFn = func(string) error {
		calls++
		return nil
	}

	bt := &BrowserTool{backend: backendPinchtab}
	if err := bt.CleanupChromedp(); err != nil {
		t.Fatalf("unexpected cleanup error: %v", err)
	}
	if calls != 0 {
		t.Fatalf("pinchtab cleanup must not kill chromedp, got %d calls", calls)
	}
}

func TestBrowserToolCleanupChromedp_KillsOnlyTrackedDataDir(t *testing.T) {
	orig := killChromedpChromeForDirFn
	defer func() { killChromedpChromeForDirFn = orig }()

	var gotDir string
	killChromedpChromeForDirFn = func(dir string) error {
		gotDir = dir
		return nil
	}

	bt := &BrowserTool{backend: backendChromedp, chromedpDataDir: "/tmp/kocoro-owned-profile"}
	if err := bt.CleanupChromedp(); err != nil {
		t.Fatalf("unexpected cleanup error: %v", err)
	}
	if gotDir != "/tmp/kocoro-owned-profile" {
		t.Fatalf("expected exact tracked data dir cleanup, got %q", gotDir)
	}
	if bt.backend != backendNone || bt.chromedpDataDir != "" {
		t.Fatalf("expected browser state reset, backend=%v dir=%q", bt.backend, bt.chromedpDataDir)
	}
}

func TestBrowserToolRun_CloseSkipsWhenAnotherRunActive(t *testing.T) {
	orig := killChromedpChromeForDirFn
	defer func() { killChromedpChromeForDirFn = orig }()

	calls := 0
	killChromedpChromeForDirFn = func(string) error {
		calls++
		return nil
	}

	tr := &browserUseTracker{}
	leaseA := newBrowserUseLeaseWithTracker(tr)
	leaseB := newBrowserUseLeaseWithTracker(tr)
	leaseB.MarkUsed()
	ctx := context.WithValue(context.Background(), browserLeaseKey{}, leaseA)

	bt := &BrowserTool{backend: backendChromedp, chromedpDataDir: "/tmp/kocoro-owned-profile"}
	result, err := bt.Run(ctx, `{"action":"close","description":"test"}`)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// Skipped-close is informational, not a tool failure — IsError must be
	// false so it doesn't burn the LLM's all-errors retry budget or trip
	// loopdetect on a benign "another Run is still using the browser" signal.
	if result.IsError {
		t.Fatalf("skipped close must be informational (IsError=false), got error result %q", result.Content)
	}
	if !strings.Contains(result.Content, "skipped") {
		t.Fatalf("expected skipped-close message to mention 'skipped', got %q", result.Content)
	}
	if calls != 0 {
		t.Fatalf("close must not kill chromedp while another run is active, got %d calls", calls)
	}
	if bt.backend != backendChromedp {
		t.Fatalf("expected backend to remain active, got %v", bt.backend)
	}
	leaseA.ReleaseOnly()
	leaseB.ReleaseOnly()
}

// TestBrowserToolRun_MarksLeaseBeforeBackendSetup is the integration test that
// guards the race-protective ordering fix: BrowserTool.Run must call
// MarkBrowserUsed BEFORE invoking the backend setup so a concurrent teardown
// from another Run cannot kill Chrome between backend setup and our Mark. The
// buggy ordering (Mark *after* backend setup) would leave count==0 here.
func TestBrowserToolRun_MarksLeaseBeforeBackendSetup(t *testing.T) {
	tr := &browserUseTracker{}
	lease := newBrowserUseLeaseWithTracker(tr)
	ctx := context.WithValue(context.Background(), browserLeaseKey{}, lease)

	// Replace the backend-setup step with a probe that verifies the lease was
	// already marked by the time Run reaches backend setup. Restore on exit.
	orig := ensureBackendFn
	defer func() { ensureBackendFn = orig }()

	var countAtEnsure int
	ensureBackendFn = func(_ *BrowserTool, _ context.Context) error {
		countAtEnsure = tr.activeCount()
		return errors.New("test stub: skip real backend setup")
	}

	bt := &BrowserTool{}
	argsJSON := `{"action":"navigate","url":"https://example.com","description":"test"}`
	if _, _ = bt.Run(ctx, argsJSON); countAtEnsure != 1 {
		t.Fatalf("expected lease count 1 when ensureBackend runs (Mark-before-ensure), got %d — the race window between ensureBackend and MarkUsed is open", countAtEnsure)
	}

	// Cleanup: release the lease so the global tracker doesn't leak state
	// into other tests (this test uses an isolated tracker via lease, but Run
	// also touches globalBrowserTracker via the ctx-installed lease? It does
	// not — we installed our own lease on ctx via WithValue, not via
	// WithBrowserUseLease which binds to the global tracker).
	lease.ReleaseOnly()
}

// TestBrowserToolRun_CloseActionMarksLease verifies the close action
// participates in the lease so it cannot tear down Chrome while another Run is
// using it.
func TestBrowserToolRun_CloseActionMarksLease(t *testing.T) {
	tr := &browserUseTracker{}
	lease := newBrowserUseLeaseWithTracker(tr)
	ctx := context.WithValue(context.Background(), browserLeaseKey{}, lease)

	bt := &BrowserTool{}
	argsJSON := `{"action":"close","description":"test"}`
	_, _ = bt.Run(ctx, argsJSON)

	if got := tr.activeCount(); got != 1 {
		t.Fatalf("close action must mark the lease, got count %d", got)
	}
	lease.ReleaseOnly()
}

// TestBrowserToolRun_InvalidArgsSkipsLease verifies a validation failure
// returns without marking the lease — args were rejected, no backend work
// happened. Sanity check.
func TestBrowserToolRun_InvalidArgsSkipsLease(t *testing.T) {
	tr := &browserUseTracker{}
	lease := newBrowserUseLeaseWithTracker(tr)
	ctx := context.WithValue(context.Background(), browserLeaseKey{}, lease)

	bt := &BrowserTool{}
	// navigate without url fails validation
	argsJSON := `{"action":"navigate","description":"test"}`
	_, _ = bt.Run(ctx, argsJSON)

	if got := tr.activeCount(); got != 0 {
		t.Fatalf("invalid args must not mark the lease, got count %d", got)
	}
}
