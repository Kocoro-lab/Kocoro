package daemon

import (
	"errors"
	"testing"
)

type spyReconnectSink struct {
	failures  []string
	successes []string
}

func (s *spyReconnectSink) OnFailure(name string) { s.failures = append(s.failures, name) }
func (s *spyReconnectSink) OnSuccess(name string) { s.successes = append(s.successes, name) }

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
