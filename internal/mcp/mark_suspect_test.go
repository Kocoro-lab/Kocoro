package mcp

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// ProbeNow short-circuits to the cached state when the last successful
// transport check is fresh (<60s). After a tool call fails, that cache can
// hide a connection that died moments ago — MarkTransportSuspect invalidates
// the freshness so the next ProbeNow performs a REAL probe.
func TestMarkTransportSuspectBypassesFreshHealthCache(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := NewClientManager()
	fake := &fakeListToolsClient{}
	mgr.SeedConfig("s", MCPServerConfig{Command: "dummy"})
	mgr.SeedClient("s", fake)

	sup := NewSupervisor(mgr)
	sup.Start(ctx)
	defer sup.Stop()

	// Wait for the initial probe to mark the server healthy.
	deadline := time.Now().Add(3 * time.Second)
	for sup.HealthFor("s").State != StateHealthy {
		if time.Now().After(deadline) {
			t.Fatal("precondition: server never became healthy")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Break the transport. The cached health is still fresh, so a plain
	// ProbeNow returns the stale Healthy without probing.
	fake.mu.Lock()
	fake.listToolsErr = fmt.Errorf("broken pipe")
	fake.mu.Unlock()

	if h := sup.ProbeNow("s"); h.State != StateHealthy {
		t.Fatalf("precondition: expected cached healthy from fresh ProbeNow, got %v", h.State)
	}

	// Invalidate freshness — the next ProbeNow must actually probe and see
	// the broken transport.
	sup.MarkTransportSuspect("s")
	if h := sup.ProbeNow("s"); h.State == StateHealthy {
		t.Fatal("expected a real probe after MarkTransportSuspect, got cached healthy")
	}
}
