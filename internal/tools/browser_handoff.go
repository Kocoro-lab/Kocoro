package tools

import (
	"log"
	"time"
)

// HandBrowserOff completes the reload-time handoff of a *BrowserTool that is
// about to be replaced. It marks OLD deprecated (so register.go's cleanup
// gate skips it) and either eagerly cleans up (no in-flight leases) or
// schedules a non-destructive watchdog (active leases drain via the lease
// teardown path).
//
// Safe to call with browser == nil.
func HandBrowserOff(browser *BrowserTool, backstop time.Duration) {
	if browser == nil {
		return
	}
	browser.MarkDeprecated()
	if cleanupIfNoOwnersLocked(browser) {
		log.Printf("daemon: reload: OLD browser cleaned up (no in-flight leases)")
		return
	}
	time.AfterFunc(backstop, func() {
		if cleanupIfNoOwnersLocked(browser) {
			return
		}
		// Active leases remain. Do NOT call Cleanup — would kill live work.
		// We can read the count outside the lock here because we already
		// know it's non-zero (and stale-by-one is fine for a log message).
		n := BrowserOwnerActiveCount(browser)
		log.Printf("daemon: reload watchdog: deprecated OLD browser still has %d active lease(s) after %s; deferring to lease teardown", n, backstop)
	})
}

// cleanupIfNoOwnersLocked atomically checks that no leases reference browser
// and, if so, runs browser.Cleanup() while still holding tracker.mu. The
// composite check+cleanup is what closes the TOCTOU window against a
// concurrent MarkUsedWith: any Run that hasn't yet incremented the owner
// count blocks on this lock and observes the post-cleanup state instead of
// racing with it. Returns true when cleanup ran.
//
// Lock order: tracker.mu → t.mu (browser.Cleanup acquires t.mu internally).
// This matches the order ReleaseAndMaybeTeardown uses when running its
// teardown callback under tracker.mu, so no inversion is introduced.
func cleanupIfNoOwnersLocked(browser *BrowserTool) bool {
	globalBrowserTracker.mu.Lock()
	defer globalBrowserTracker.mu.Unlock()
	if globalBrowserTracker.owners[browser] != 0 {
		return false
	}
	browser.Cleanup()
	return true
}
