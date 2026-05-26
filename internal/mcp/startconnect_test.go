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
