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
	if BrowserOwnerActiveCount(browser) == 0 {
		browser.Cleanup()
		log.Printf("daemon: reload: OLD browser cleaned up (no in-flight leases)")
		return
	}
	time.AfterFunc(backstop, func() {
		n := BrowserOwnerActiveCount(browser)
		if n == 0 {
			browser.Cleanup()
			return
		}
		// Active leases remain. Do NOT call Cleanup — would kill live work.
		log.Printf("daemon: reload watchdog: deprecated OLD browser still has %d active lease(s) after %s; deferring to lease teardown", n, backstop)
	})
}
