package tools

import (
	"context"
	"sync"
)

// browserUseTracker counts in-flight Runs that are using the chromedp
// BrowserTool. It mirrors mcp.chromeUseTracker so the chromedp Chrome inherits
// the same per-turn teardown semantics as the Playwright CDP Chrome — see
// internal/mcp/chrome.go for the full state-machine documentation.
type browserUseTracker struct {
	mu     sync.Mutex
	count  int
	owners map[*BrowserTool]int // per-instance refcount; used by deprecated/non-deprecated owner-aware release paths
}

var globalBrowserTracker = &browserUseTracker{}

func (t *browserUseTracker) activeCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.count
}

// ownerActiveCount returns the active lease count for a specific BrowserTool.
// Caller must hold tracker.mu OR call BrowserOwnerActiveCount which acquires it.
func (tr *browserUseTracker) ownerActiveCount(owner *BrowserTool) int {
	return tr.owners[owner]
}

// BrowserUseLease is the per-Run handle to globalBrowserTracker. State machine
// matches mcp.ChromeUseLease:
//   - initial:                          acquired=false, released=false
//   - MarkUsed when !acquired/released: counter++, acquired=true
//   - MarkUsed when acquired/released:  no-op
//   - Release when !released, acquired: counter--, released=true (teardown if last)
//   - Release when !released, !acquired:released=true (no counter change)
//   - Release when released:            no-op
type BrowserUseLease struct {
	tracker  *browserUseTracker
	acquired bool
	released bool
	owner    *BrowserTool
}

// NewBrowserUseLease returns a lease bound to the global tracker.
func NewBrowserUseLease() *BrowserUseLease {
	return newBrowserUseLeaseWithTracker(globalBrowserTracker)
}

func newBrowserUseLeaseWithTracker(tr *browserUseTracker) *BrowserUseLease {
	return &BrowserUseLease{tracker: tr}
}

// MarkUsed acquires the lease on first call. Idempotent and safe after Release.
func (l *BrowserUseLease) MarkUsed() {
	if l == nil {
		return
	}
	l.tracker.mu.Lock()
	defer l.tracker.mu.Unlock()
	if l.acquired || l.released {
		return
	}
	l.tracker.count++
	l.acquired = true
}

// MarkUsedWith acquires the lease and captures the BrowserTool that this Run
// uses. Idempotent on the same lease. owner is required — pass the *BrowserTool
// whose CleanupChromedp will be used for teardown.
func (l *BrowserUseLease) MarkUsedWith(owner *BrowserTool) {
	if l == nil {
		return
	}
	l.tracker.mu.Lock()
	defer l.tracker.mu.Unlock()
	if l.acquired || l.released {
		return
	}
	l.tracker.count++
	if l.tracker.owners == nil {
		l.tracker.owners = make(map[*BrowserTool]int)
	}
	l.tracker.owners[owner]++
	l.acquired = true
	l.owner = owner
}

// Owner returns the *BrowserTool captured by MarkUsedWith. nil when the lease
// was never acquired (or was acquired via legacy MarkUsed).
func (l *BrowserUseLease) Owner() *BrowserTool {
	if l == nil {
		return nil
	}
	l.tracker.mu.Lock()
	defer l.tracker.mu.Unlock()
	return l.owner
}

// ReleaseOnly decrements the tracker (if MarkUsed was called) without running
// teardown. Used by paths that should release but skip teardown.
func (l *BrowserUseLease) ReleaseOnly() {
	if l == nil {
		return
	}
	l.tracker.mu.Lock()
	defer l.tracker.mu.Unlock()
	if l.released {
		return
	}
	l.released = true
	if l.acquired {
		l.tracker.count--
	}
}

// ReleaseAndMaybeTeardown decrements the tracker (if MarkUsed was called); if
// the resulting count is zero, runs teardown WHILE STILL HOLDING tracker.mu.
// Concurrent MarkUsed() calls take the same mutex and block until teardown
// completes — prevents a new browser-using Run from having Chrome killed
// mid-launch.
//
// Returns torndown=true when the teardown callback ran; the err is whatever
// the callback returned (e.g. "Chrome still alive after SIGKILL"). Returns
// torndown=false when another lease still holds the tracker or the lease was
// never acquired.
func (l *BrowserUseLease) ReleaseAndMaybeTeardown(teardown func() error) (torndown bool, err error) {
	if l == nil {
		return false, nil
	}
	l.tracker.mu.Lock()
	defer l.tracker.mu.Unlock()
	if l.released {
		return false, nil
	}
	l.released = true
	if !l.acquired {
		return false, nil
	}
	l.tracker.count--
	if l.tracker.count > 0 {
		return false, nil
	}
	torndown = true
	if teardown != nil {
		err = teardown()
	}
	return torndown, err
}

// TeardownIfOnlyUser runs teardown while holding tracker.mu only when this
// lease is the sole active browser user. It does not release the lease; the
// normal end-of-turn cleanup still owns the counter decrement.
func (l *BrowserUseLease) TeardownIfOnlyUser(teardown func() error) (torndown bool, skipped bool, err error) {
	if l == nil {
		if teardown != nil {
			return true, false, teardown()
		}
		return true, false, nil
	}
	l.tracker.mu.Lock()
	defer l.tracker.mu.Unlock()
	if l.tracker.count > 1 {
		return false, true, nil
	}
	torndown = true
	if teardown != nil {
		err = teardown()
	}
	return torndown, false, err
}

type browserLeaseKey struct{}

// WithBrowserUseLease installs a fresh BrowserUseLease on the context. Call
// once at Run start (in RunAgent) before any tool dispatch can happen.
func WithBrowserUseLease(ctx context.Context) context.Context {
	return context.WithValue(ctx, browserLeaseKey{}, NewBrowserUseLease())
}

// BrowserUseLeaseFrom returns the lease installed by WithBrowserUseLease, or
// nil if none is present.
func BrowserUseLeaseFrom(ctx context.Context) *BrowserUseLease {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(browserLeaseKey{}).(*BrowserUseLease)
	return v
}

// MarkBrowserUsed marks the lease on ctx as having used the chromedp Chrome
// this Run. No-op when ctx carries no lease. Idempotent via the lease state
// machine.
func MarkBrowserUsed(ctx context.Context) {
	BrowserUseLeaseFrom(ctx).MarkUsed()
}

// GlobalBrowserTrackerActiveCountForTest exposes the global tracker count for
// cross-package tests. Test-only.
func GlobalBrowserTrackerActiveCountForTest() int {
	return globalBrowserTracker.activeCount()
}

// BrowserOwnerActiveCount returns how many active leases reference the given
// *BrowserTool. Used by the daemon reload handler to decide between eager
// cleanup (count == 0 → nothing to drain) and deferred handoff.
func BrowserOwnerActiveCount(owner *BrowserTool) int {
	globalBrowserTracker.mu.Lock()
	defer globalBrowserTracker.mu.Unlock()
	return globalBrowserTracker.owners[owner]
}
