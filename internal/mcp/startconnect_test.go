package mcp

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestRegisterConfigs_RegistersWithoutConnecting(t *testing.T) {
	mgr := NewClientManager()
	cfgs := map[string]MCPServerConfig{
		"alpha": {Command: "cat"},
		"beta":  {Command: "cat"},
	}
	mgr.RegisterConfigs(cfgs)

	if _, ok := mgr.ConfigFor("alpha"); !ok {
		t.Error("expected config alpha to be registered")
	}
	if _, ok := mgr.ConfigFor("beta"); !ok {
		t.Error("expected config beta to be registered")
	}
	if got := len(mgr.ConnectedServers()); got != 0 {
		t.Errorf("expected 0 connected servers without StartConnectAll, got %d", got)
	}
}

func TestStartConnectAll_ReturnsImmediately(t *testing.T) {
	// /nonexistent triggers a fast connect error inside the goroutine.
	// StartConnectAll itself must NOT wait for that — it's fire-and-forget.
	mgr := NewClientManager()
	servers := map[string]MCPServerConfig{
		"nope": {Command: "/nonexistent/path/cmd"},
	}

	var wg sync.WaitGroup
	wg.Add(1)
	var capturedName string
	var capturedErr error

	start := time.Now()
	mgr.StartConnectAll(context.Background(), servers, 5*time.Second, func(name string, err error) {
		capturedName = name
		capturedErr = err
		wg.Done()
	})

	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("StartConnectAll blocked for %v — should fire-and-forget", elapsed)
	}

	// Wait up to 5s for the callback. If the goroutine is correctly spawned
	// and the connect helper errors fast for a missing binary, it'll fire
	// well under that.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("onResult callback never fired")
	}

	if capturedName != "nope" {
		t.Errorf("expected name=nope, got %q", capturedName)
	}
	if capturedErr == nil {
		t.Error("expected non-nil error for nonexistent command")
	}
}

func TestStartConnectAll_SkipsDisabledServers(t *testing.T) {
	mgr := NewClientManager()
	servers := map[string]MCPServerConfig{
		"on":  {Command: "/nonexistent/cmd1"},
		"off": {Command: "/nonexistent/cmd2", Disabled: true},
	}

	var mu sync.Mutex
	calls := map[string]bool{}
	var wg sync.WaitGroup
	wg.Add(1)
	mgr.StartConnectAll(context.Background(), servers, 2*time.Second, func(name string, err error) {
		mu.Lock()
		calls[name] = true
		mu.Unlock()
		if name == "on" {
			wg.Done()
		}
	})

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("onResult never fired for enabled server")
	}

	mu.Lock()
	defer mu.Unlock()
	if !calls["on"] {
		t.Error("expected enabled server to receive callback")
	}
	if calls["off"] {
		t.Error("disabled server must not be dialed")
	}
}

func TestStartConnectAll_DedupesInFlightConnect(t *testing.T) {
	// Reload while a daemon-startup async connect is still mid-Initialize
	// must NOT spawn a second connect goroutine for the same server —
	// that's exactly the EADDRINUSE scenario the original Bug 1 fix was
	// supposed to close, and the code review caught us missing the
	// dedup. Use `sleep 30` so the inner Initialize stays blocked long
	// enough to observe the dedup decision.
	mgr := NewClientManager()
	servers := map[string]MCPServerConfig{
		"hang": {
			Command:               "sleep",
			Args:                  []string{"30"},
			ConnectTimeoutSeconds: 10,
		},
	}

	// First call: reserves the in-flight slot, kicks off the goroutine.
	firstCh := make(chan struct{}, 1)
	mgr.StartConnectAll(context.Background(), servers, 30*time.Second, func(name string, err error) {
		firstCh <- struct{}{}
	})

	// Give the first goroutine a hair of CPU so it can claim the slot
	// before the second StartConnectAll fires.
	time.Sleep(50 * time.Millisecond)

	// Second call against same mgr + same server: must be a no-op (the
	// onResult would normally fire fast for "sleep 30" because Initialize
	// errors out as soon as ctx times out, but the dedup skip path returns
	// without calling onResult).
	secondFired := make(chan struct{}, 1)
	mgr.StartConnectAll(context.Background(), servers, 30*time.Second, func(name string, err error) {
		secondFired <- struct{}{}
	})

	select {
	case <-secondFired:
		t.Fatal("second StartConnectAll for in-flight server fired onResult — dedup failed, would have spawned a duplicate subprocess")
	case <-time.After(500 * time.Millisecond):
		// good: skipped silently.
	}

	// Drain first call's eventual onResult so the test goroutine cleans up.
	select {
	case <-firstCh:
	case <-time.After(15 * time.Second):
		t.Fatal("first connect goroutine never finished")
	}
}

func TestStartConnectAll_PerServerTimeoutHonored(t *testing.T) {
	// `sleep 30` ignores stdin and writes nothing to stdout, so the MCP
	// Initialize handshake stalls forever waiting for a response. The only
	// way out is the per-server ctx timeout. If ConnectTimeoutSeconds=1
	// is honored we error in ~1-2s; if it falls through to the 60s default
	// we'd blow past the 8s assertion below.
	mgr := NewClientManager()
	servers := map[string]MCPServerConfig{
		"hang": {
			Command:               "sleep",
			Args:                  []string{"30"},
			ConnectTimeoutSeconds: 1,
		},
	}

	resultCh := make(chan error, 1)
	start := time.Now()
	mgr.StartConnectAll(context.Background(), servers, 60*time.Second, func(name string, err error) {
		resultCh <- err
	})

	select {
	case err := <-resultCh:
		if err == nil {
			t.Fatal("expected timeout/error for hanging stdio command")
		}
		if elapsed := time.Since(start); elapsed > 8*time.Second {
			t.Errorf("per-server timeout did not fire promptly: elapsed=%v (override likely ignored)", elapsed)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("connect goroutine did not finish; per-server timeout likely broken")
	}
}
