package mcp

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A disabled server is configured-but-off: ConnectAll never connects it, so a
// supervisor entry would just be a goroutine failing its probe forever —
// 2026-08-03 live state: disabled longbridge + intercom each had a perpetual
// probe loop marching through backoff.
func TestSupervisorStart_SkipsDisabledServers(t *testing.T) {
	mgr := NewClientManager()
	mgr.SeedConfig("enabled-srv", MCPServerConfig{Command: "dummy"})
	mgr.SeedConfig("disabled-srv", MCPServerConfig{Command: "dummy", Disabled: true})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup := NewSupervisor(mgr)
	sup.Start(ctx)
	defer sup.Stop()

	states := sup.HealthStates()
	if _, ok := states["enabled-srv"]; !ok {
		t.Error("enabled server must be supervised")
	}
	if _, ok := states["disabled-srv"]; ok {
		t.Error("disabled server must not get a probe loop")
	}
	// ProbeNow on the unsupervised name must stay a side-effect-free
	// StateDisconnected, not a reconnect attempt.
	if h := sup.ProbeNow("disabled-srv"); h.State != StateDisconnected {
		t.Errorf("ProbeNow on disabled server = %v, want StateDisconnected", h.State)
	}
}

// Reconnect is the only path that spawns a server subprocess outside initial
// connect; it must refuse disabled configs so no probe/retry path can
// relaunch a server the user turned off.
func TestReconnect_RefusesDisabledServer(t *testing.T) {
	mgr := NewClientManager()
	mgr.SeedConfig("off", MCPServerConfig{Command: "dummy", Disabled: true})

	if _, err := mgr.Reconnect(context.Background(), "off"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("Reconnect on disabled server must refuse, got err=%v", err)
	}
}

func TestCallTool_RefusesLazyStartOfDisabledServer(t *testing.T) {
	mgr := NewClientManager()
	mgr.SeedConfig("off", MCPServerConfig{Command: "dummy", Disabled: true})

	_, isErr, err := mgr.CallTool(context.Background(), "off", "some_tool", nil)
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("CallTool on disabled server must refuse lazy-start, got err=%v", err)
	}
	if !isErr {
		t.Error("expected isError=true")
	}
}

// Regression for the failure-count overflow: attempts grew unbounded, and at
// ~32 consecutive failures baseMin(5s ns)·2^(attempts-1) overflowed int64,
// went negative, bypassed the dormant clamp, and reset a long-dead server
// back to fast-tier probing. The interval must remain a positive dormant-tier
// value no matter how long the failure streak runs.
func TestBackoff_NoOverflowOnLongFailureStreak(t *testing.T) {
	b := newBackoffState(5*time.Second, 30*time.Second, 5*time.Minute)
	for i := 0; i < 200; i++ {
		b.recordFailure()
		if b.interval <= 0 {
			t.Fatalf("failure #%d: interval %v is not positive — overflow reintroduced", i+1, b.interval)
		}
	}
	// Deep in the streak the interval must still be dormant-tier (jitter is
	// 0.8–1.2× dormant), not a fast-tier value.
	if b.interval < time.Duration(float64(5*time.Minute)*0.79) {
		t.Fatalf("after long streak interval = %v, want dormant-tier (~5m ±20%%)", b.interval)
	}
	// recordSuccess still fully resets the clamped counter.
	b.recordSuccess()
	if b.attempts != 0 || b.interval != 0 {
		t.Fatalf("recordSuccess must reset state, got attempts=%d interval=%v", b.attempts, b.interval)
	}
}
