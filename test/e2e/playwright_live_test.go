package e2e

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
)

// TestLive_Playwright_ChromeOnDemandLifecycle validates, against REAL Chrome,
// the on-demand browser lifecycle that the turn-start-relaunch fix depends on:
//
//	launch → teardown → (recovery armed) → on-demand relaunch
//
// The fix stops the turn-start preflight from relaunching Chrome for CDP +
// keep_alive=false, relying instead on mcp_tool.go's pre-call
// ensureChromeDebugPort to relaunch only when the agent actually invokes a
// browser tool. That recovery hinges on two real-Chrome facts this test pins:
//
//   - After a turn's on-demand teardown, the dedicated CDP port is down and
//     ShouldPreflightDedicatedChrome(DefaultCDPPort) is true — i.e. the next
//     browser tool call WILL relaunch Chrome (recovery is armed, not lost).
//   - EnsureChromeDebugPort actually brings Chrome back on that port.
//
// The decision logic across all Playwright situations (state × connected ×
// CDP/keep_alive × source) and the Degraded registry-exposure scope are covered
// deterministically by TestPlaywrightTurnStartProbeAction and
// TestRebuildRegistryForHealth_DegradedExposureScope; this test is the live
// counterpart that exercises the real Chrome process those rely on.
//
// Gated by SHANNON_E2E_LIVE=1. Auto-skips when it can't run safely:
//   - non-macOS (the dedicated-Chrome launch path is macOS-specific), or
//   - Google Chrome isn't installed, or
//   - the dedicated CDP port isn't in a clean state — i.e.
//     ShouldPreflightDedicatedChrome is false because a daemon-owned Chrome is
//     alive or the port is occupied. This is the SAME predicate production uses,
//     so the skip both matches production loopback behavior and guarantees the
//     test never tears down a browser it did not launch.
//
// Run it with the daemon stopped:  SHANNON_E2E_LIVE=1 go test ./test/e2e/ -run ChromeOnDemand -v
func TestLive_Playwright_ChromeOnDemandLifecycle(t *testing.T) {
	skipUnlessLive(t)

	if runtime.GOOS != "darwin" {
		t.Skip("dedicated CDP Chrome launch path is macOS-specific")
	}
	if _, err := os.Stat("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"); err != nil {
		t.Skip("Google Chrome not installed at the expected path")
	}

	const port = mcp.DefaultCDPPort // ShouldPreflightDedicatedChrome only arms on the default port.

	// Safety gate FIRST, before registering any teardown: only run from a clean
	// state where nothing owns the dedicated CDP port. If it's not clean (daemon
	// running, Chrome alive, or port occupied), SKIP — never fail — so we can
	// never trigger a teardown of a browser this test did not launch.
	if !mcp.ShouldPreflightDedicatedChrome(port) {
		t.Skipf("CDP port %d not in a clean state (daemon running / Chrome alive / port occupied) — skipping to avoid disrupting a live browser", port)
	}

	// Only ever tear down Chrome that THIS test launched. Guarding cleanup on
	// launchedByTest means an early t.Fatalf (before our own launch) can't kill
	// someone else's Chrome.
	launchedByTest := false
	t.Cleanup(func() {
		if !launchedByTest {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = mcp.StopCDPChromeAndWait(ctx)
	})

	// 1) Browser turn: launch real Chrome on demand.
	if err := mcp.EnsureChromeDebugPort(port); err != nil {
		t.Fatalf("EnsureChromeDebugPort (initial launch): %v", err)
	}
	launchedByTest = true
	if !waitFor(10*time.Second, func() bool { return mcp.IsChromeCDPReachable(port) }) {
		t.Fatalf("Chrome CDP not reachable on port %d after launch", port)
	}
	if mcp.ShouldPreflightDedicatedChrome(port) {
		t.Fatalf("ShouldPreflightDedicatedChrome(%d) = true while Chrome is up; should be false", port)
	}

	// 2) End-of-turn on-demand teardown (CDP + keep_alive=false).
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer stopCancel()
	if err := mcp.StopCDPChromeAndWait(stopCtx); err != nil {
		t.Fatalf("StopCDPChromeAndWait: %v", err)
	}
	if !waitFor(10*time.Second, func() bool { return !mcp.IsChromeCDPReachable(port) }) {
		t.Fatalf("Chrome still CDP-reachable on port %d after teardown", port)
	}
	// Recovery armed: a non-browser turn does NOT relaunch (the fix), but the
	// next actual browser tool call would, because the preflight gate is true.
	if !mcp.ShouldPreflightDedicatedChrome(port) {
		t.Fatalf("ShouldPreflightDedicatedChrome(%d) = false after teardown; on-demand recovery would not arm", port)
	}

	// 3) Next browser turn: on-demand relaunch recovers Chrome.
	if err := mcp.EnsureChromeDebugPort(port); err != nil {
		t.Fatalf("EnsureChromeDebugPort (recovery relaunch): %v", err)
	}
	if !waitFor(10*time.Second, func() bool { return mcp.IsChromeCDPReachable(port) }) {
		t.Fatalf("Chrome CDP not reachable on port %d after on-demand recovery", port)
	}
}

func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return cond()
}
