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
	mu    sync.Mutex
	count int
}

var globalBrowserTracker = &browserUseTracker{}

func (t *browserUseTracker) activeCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.count
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
