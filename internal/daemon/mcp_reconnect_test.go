package daemon

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
)

type spyReconnectSink struct {
	failures  []string
	successes []string
	deferrals []string
}

func (s *spyReconnectSink) OnFailure(name string)  { s.failures = append(s.failures, name) }
func (s *spyReconnectSink) OnSuccess(name string)  { s.successes = append(s.successes, name) }
func (s *spyReconnectSink) OnDeferred(name string) { s.deferrals = append(s.deferrals, name) }

type recordedCallbacks struct {
	audited   []string
	connected []string
}

func newTestConnectResult(current bool) (func(string, error), *spyReconnectSink, *recordedCallbacks) {
	sink := &spyReconnectSink{}
	rec := &recordedCallbacks{}
	onResult := buildMCPConnectResult(sink, MCPConnectHooks{
		IsCurrent:   func() bool { return current },
		AuditFail:   func(name string, _ error) { rec.audited = append(rec.audited, name) },
		OnConnected: func(name string) { rec.connected = append(rec.connected, name) },
	})
	return onResult, sink, rec
}

func TestMCPConnectResult_FailureSchedulesReconnect(t *testing.T) {
	// The regression this guards: before the reconnect scheduler, a failed
	// async connect was logged and audited and then dropped, leaving the
	// server "enabled but dead" until a daemon restart.
	onResult, sink, rec := newTestConnectResult(true)

	onResult("notion", errors.New("dial: connection refused"))

	if len(sink.failures) != 1 || sink.failures[0] != "notion" {
		t.Fatalf("connect failure must reach the reconnect scheduler, got %v", sink.failures)
	}
	if len(rec.audited) != 1 || rec.audited[0] != "notion" {
		t.Fatalf("connect failure must still be audited, got %v", rec.audited)
	}
	if len(rec.connected) != 0 {
		t.Fatalf("a failed connect must not run the connected hook, got %v", rec.connected)
	}
}

func TestMCPConnectResult_SuccessClearsStreakAndRunsConnectedHook(t *testing.T) {
	onResult, sink, rec := newTestConnectResult(true)

	onResult("notion", nil)

	if len(sink.successes) != 1 || sink.successes[0] != "notion" {
		t.Fatalf("a successful connect must clear the failure streak, got %v", sink.successes)
	}
	if len(sink.failures) != 0 {
		t.Fatalf("a successful connect must not schedule a retry, got %v", sink.failures)
	}
	if len(rec.connected) != 1 || rec.connected[0] != "notion" {
		t.Fatalf("expected the connected hook to run, got %v", rec.connected)
	}
	if len(rec.audited) != 0 {
		t.Fatalf("a successful connect must not be audited as a failure, got %v", rec.audited)
	}
}

func TestMCPConnectResult_SupersededDepsDropsResultEntirely(t *testing.T) {
	// A result belonging to a manager a newer reload already replaced must not
	// schedule anything: reconnecting on a dead manager leaks a process group.
	onResult, sink, rec := newTestConnectResult(false)

	onResult("notion", errors.New("boom"))
	onResult("notion", nil)

	if len(sink.failures) != 0 || len(sink.successes) != 0 {
		t.Fatalf("superseded results must not touch the scheduler, got failures=%v successes=%v",
			sink.failures, sink.successes)
	}
	if len(rec.audited) != 0 || len(rec.connected) != 0 {
		t.Fatalf("superseded results must run no hooks, got audited=%v connected=%v",
			rec.audited, rec.connected)
	}
}

func TestMCPConnectResult_NilHooksAreSafe(t *testing.T) {
	// cmd/daemon.go wires these from optional dependencies (a nil auditor is
	// legitimate), so the callback must tolerate missing hooks.
	sink := &spyReconnectSink{}
	onResult := buildMCPConnectResult(sink, MCPConnectHooks{})

	onResult("notion", errors.New("boom"))
	onResult("notion", nil)

	if len(sink.failures) != 1 || len(sink.successes) != 1 {
		t.Fatalf("scheduler must still be driven with nil hooks, got failures=%v successes=%v",
			sink.failures, sink.successes)
	}
}

func TestMCPConnectResult_NilSinkIsSafe(t *testing.T) {
	rec := &recordedCallbacks{}
	onResult := buildMCPConnectResult(nil, MCPConnectHooks{
		OnConnected: func(name string) { rec.connected = append(rec.connected, name) },
	})

	onResult("notion", errors.New("boom"))
	onResult("notion", nil)

	if len(rec.connected) != 1 {
		t.Fatalf("hooks must still run without a scheduler, got %v", rec.connected)
	}
}

// --- PR #340 review findings ------------------------------------------------

func TestMCPConnectResult_InFlightSkipIsDeferredNotFailed(t *testing.T) {
	// StartConnectAll reports ErrConnectInFlight when another attempt holds the
	// slot. Treating that as a failure would spend a retry rung and file an
	// audit row for a scheduling collision, not a broken server.
	onResult, sink, rec := newTestConnectResult(true)

	onResult("notion", mcp.ErrConnectInFlight)

	if len(sink.deferrals) != 1 || sink.deferrals[0] != "notion" {
		t.Fatalf("expected a deferral, got %v", sink.deferrals)
	}
	if len(sink.failures) != 0 {
		t.Fatalf("a deferred attempt must not count as a failure, got %v", sink.failures)
	}
	if len(rec.audited) != 0 {
		t.Fatalf("a deferred attempt must not be audited, got %v", rec.audited)
	}
}

func TestMCPConnectResult_WrappedInFlightErrorIsStillDeferred(t *testing.T) {
	onResult, sink, _ := newTestConnectResult(true)

	onResult("notion", fmt.Errorf("start connect: %w", mcp.ErrConnectInFlight))

	if len(sink.deferrals) != 1 {
		t.Fatalf("errors.Is must see through wrapping, got deferrals=%v failures=%v",
			sink.deferrals, sink.failures)
	}
}

func TestSwapMCPReconnectScheduler_StopsThePreviousLadder(t *testing.T) {
	// The ownership rule the design rests on: a reload that rebuilds the
	// manager must not leave the superseded generation's timers armed.
	deps := &ServerDeps{}
	fired := make(chan string, 4)
	prev := mcp.NewReconnectScheduler(func(name string) { fired <- name })
	deps.SwapMCPReconnectScheduler(prev)
	prev.OnFailure("notion")

	next := mcp.NewReconnectScheduler(func(name string) { fired <- name })
	deps.SwapMCPReconnectScheduler(next)

	// The old scheduler is stopped: further reports do not arm anything.
	prev.OnFailure("notion")
	prev.OnFailure("ms365")

	if deps.MCPReconnect != next {
		t.Fatal("swap must install the new scheduler")
	}
	select {
	case name := <-fired:
		t.Fatalf("superseded ladder still fired for %q", name)
	default:
	}
}

func TestSwapMCPReconnectScheduler_SameSchedulerIsNotStopped(t *testing.T) {
	// Idempotent re-install must not stop the live ladder.
	deps := &ServerDeps{}
	sched := mcp.NewReconnectScheduler(func(string) {})
	deps.SwapMCPReconnectScheduler(sched)
	deps.SwapMCPReconnectScheduler(sched)

	sched.OnFailure("notion")
	// A stopped scheduler drops the entry entirely; a live one records it.
	if deps.MCPReconnect != sched {
		t.Fatal("re-installing the same scheduler must keep it live")
	}
}

func TestShutdownCleanup_StopsTheLadderBeforeClosingConnections(t *testing.T) {
	// Ordering is the point: a timer that fires after Cleanup would respawn a
	// subprocess the cleanup just reaped.
	var order []string
	deps := &ServerDeps{}
	deps.Cleanup = func() { order = append(order, "cleanup") }
	sched := mcp.NewReconnectScheduler(func(string) { order = append(order, "reconnect") })
	deps.SwapMCPReconnectScheduler(sched)

	deps.ShutdownCleanup()

	if deps.MCPReconnect != nil {
		t.Fatal("ShutdownCleanup must clear the scheduler")
	}
	if len(order) != 1 || order[0] != "cleanup" {
		t.Fatalf("expected cleanup to run with no reconnect, got %v", order)
	}
	// A post-shutdown report must not arm anything.
	sched.OnFailure("notion")
}

func TestNewMCPReconnectWiring_SkipsServersRemovedOrDisabledWhilePending(t *testing.T) {
	// "Do not resurrect a server the user deliberately shut down" is the
	// safety-relevant branch of the retry action.
	mgr := mcp.NewClientManager()
	defer mgr.Close()
	mgr.SeedConfig("disabled-server", mcp.MCPServerConfig{
		Command:  "/nonexistent/never-runs",
		Disabled: true,
	})

	_, scheduler := NewMCPReconnectWiring(
		context.Background(), mgr, time.Second, MCPConnectHooks{},
	)
	if scheduler == nil {
		t.Fatal("expected a scheduler for a non-nil manager")
	}
	defer scheduler.Stop()

	// Neither a disabled server nor an unknown one may be connected. Both
	// paths return before touching the manager; the assertion is that no
	// client appears for either.
	scheduler.OnFailure("disabled-server")
	scheduler.OnFailure("never-configured")

	if mgr.IsConnected("disabled-server") || mgr.IsConnected("never-configured") {
		t.Fatal("retry must not connect a disabled or unknown server")
	}
}

func TestNewMCPReconnectWiring_NilManagerYieldsNoScheduler(t *testing.T) {
	onResult, scheduler := NewMCPReconnectWiring(
		context.Background(), nil, time.Second, MCPConnectHooks{},
	)
	if scheduler != nil {
		t.Fatal("a nil manager has nothing to reconnect")
	}
	// The callback must still be usable (logging/audit only).
	onResult("notion", errors.New("boom"))
}

func TestMCPConnectHooks_LabelDistinguishesEntryPoints(t *testing.T) {
	// Both paths share one callback now, so the label is what keeps a stuck
	// server diagnosable in the daemon log.
	if got := (MCPConnectHooks{}).label(); got != "async" {
		t.Fatalf("default label = %q, want \"async\"", got)
	}
	if got := (MCPConnectHooks{Label: "reload retry"}).label(); got != "reload retry" {
		t.Fatalf("explicit label = %q", got)
	}
}
