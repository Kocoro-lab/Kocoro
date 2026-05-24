package daemon

import (
	"context"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

func TestCleanupBrowserToolAfterTurn_UsesLeaseOwnerNotRegistry(t *testing.T) {
	// Simulates the reload scenario: lease captures OLD, registry holds NEW.
	// Cleanup must run against OLD (the lease owner), not NEW.
	oldBT := &tools.BrowserTool{}
	newBT := &tools.BrowserTool{}

	// Registry holds NEW (the post-reload state)
	reg := agent.NewToolRegistry()
	reg.Register(newBT)

	// Lease captures OLD (acquired pre-reload by the in-flight Run)
	ctx := tools.WithBrowserUseLease(context.Background())
	tools.MarkBrowserUsed(ctx, oldBT)

	cleanupBrowserToolAfterTurn(ctx)

	// OBSERVABLE assertion: OLD's CleanupChromedp was called; NEW's was not.
	// Counters added on BrowserTool in Task 5 make this directly checkable.
	if got := oldBT.CleanupChromedpCalledForTest(); got != 1 {
		t.Fatalf("OLD.CleanupChromedp must be called once; got %d", got)
	}
	if got := newBT.CleanupChromedpCalledForTest(); got != 0 {
		t.Fatalf("NEW.CleanupChromedp must NOT be called; got %d", got)
	}
	if got := tools.BrowserOwnerActiveCount(oldBT); got != 0 {
		t.Fatalf("owners[oldBT] = %d, want 0 after cleanup", got)
	}
}

func TestCleanupBrowserToolAfterTurn_DeprecatedOwnerCallsFullCleanup(t *testing.T) {
	// When the lease's owner is deprecated (reload handoff), the teardown
	// callback must be full Cleanup() (covers BOTH chromedp and pinchtab
	// backends), not just CleanupChromedp. Otherwise a deprecated OLD on
	// pinchtab would leak pinchtab state through the lease teardown path,
	// since register.go's cleanup is gated off by IsDeprecated.
	oldBT := &tools.BrowserTool{}
	oldBT.MarkDeprecated()

	ctx := tools.WithBrowserUseLease(context.Background())
	tools.MarkBrowserUsed(ctx, oldBT)

	cleanupBrowserToolAfterTurn(ctx)

	if got := oldBT.CleanupCalledForTest(); got != 1 {
		t.Fatalf("deprecated OLD must hit full Cleanup() once; got %d", got)
	}
	if got := oldBT.CleanupChromedpCalledForTest(); got != 0 {
		t.Fatalf("deprecated OLD must NOT take the CleanupChromedp-only path; got %d", got)
	}
}

func TestCleanupBrowserToolAfterTurn_NilOwnerReleasesOnly(t *testing.T) {
	// Lease created via WithBrowserUseLease but MarkUsedWith never called —
	// no browser activity this turn. Cleanup is no-op; ReleaseOnly keeps
	// the counter sane.
	ctx := tools.WithBrowserUseLease(context.Background())
	cleanupBrowserToolAfterTurn(ctx)
	// No panic, no allocations beyond release.
}
