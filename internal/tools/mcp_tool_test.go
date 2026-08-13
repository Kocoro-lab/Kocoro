package tools

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// --- Test 1: Disconnected → first call fails → on-demand reconnect → retry succeeds ---

func TestMCPTool_Run_ReconnectOnDisconnected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Set up a manager with config but NO client initially.
	mgr := mcp.NewClientManager()
	mgr.SeedConfig("playwright", mcp.MCPServerConfig{Command: "dummy"})

	// Set up supervisor and start it — initial probe will fail (no client)
	// → server enters StateDisconnected.
	sup := mcp.NewSupervisor(mgr)
	sup.Start(ctx)
	defer sup.Stop()

	// Let the initial probe cycle run and mark the server disconnected.
	// Poll instead of a fixed sleep so CI load can't flake it.
	deadline := time.Now().Add(3 * time.Second)
	for sup.HealthFor("playwright").State != mcp.StateDisconnected {
		if time.Now().After(deadline) {
			t.Fatalf("expected disconnected after initial probe, got %v", sup.HealthFor("playwright").State)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Now inject a controllable client: CallTool fails once (io.EOF), then
	// succeeds. ListTools always succeeds (so the transport probe works).
	fake := &controllableCallToolClient{}
	mgr.SeedClient("playwright", fake)

	// Create MCPTool with supervisor for on-demand reconnect. The tool must
	// carry a read-only/idempotent annotation: since the write-replay fix, a
	// POST-dispatch transport failure re-dispatches ONLY annotation-blessed
	// tools (unannotated ones surface as outcome-unknown instead).
	tool := mcpgo.Tool{
		Name:        "browser_tabs",
		Annotations: mcpgo.ToolAnnotation{ReadOnlyHint: mcpgo.ToBoolPtr(true)},
	}
	mt := NewMCPTool("playwright", tool, mgr)
	mt.SetSupervisor(sup)

	result, err := mt.Run(ctx, `{"action":"list"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error result: %s", result.Content)
	}

	// Verify: first call failed (EOF), ProbeNow reconnected, second call succeeded.
	calls := int(fake.callToolCount.Load())
	if calls != 2 {
		t.Errorf("expected 2 CallTool calls (fail + retry), got %d", calls)
	}
}

// The duplicate-write bug this guards: a stdio server can execute a write (send message,
// create event), then die before writing the JSON-RPC response. The client
// sees a transport error that is indistinguishable from died-before-acting,
// so re-dispatching an unannotated tool risks a duplicate side effect. The
// tool must be dispatched exactly once and the model must see an explicit
// outcome-unknown result, not a plain failure it would read as "didn't run".
func TestMCPTool_Run_WriteToolNotReplayedAfterPostDispatchTransportError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fake := &alwaysErrClient{err: io.EOF} // dead pipe AFTER dispatch
	mt, _ := newHealthySupervisedTool(t, ctx, "gws", "send_message", fake)

	result, err := mt.Run(ctx, `{"to":"x"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result")
	}
	if !result.SideEffectOutcomeUnknown {
		t.Fatal("post-dispatch transport loss must carry explicit outcome-unknown evidence")
	}
	if got := fake.callToolCount.Load(); got != 1 {
		t.Fatalf("unannotated tool must be dispatched exactly once, got %d", got)
	}
	if !strings.Contains(result.Content, "outcome UNKNOWN") {
		t.Errorf("result must surface the in-doubt outcome, got: %s", result.Content)
	}
	if !strings.Contains(result.Content, "verify") {
		t.Errorf("result must steer the model to verify before retrying, got: %s", result.Content)
	}
}

// The counterpart: a tool whose server-advertised annotations declare
// duplicate execution harmless keeps the availability-preserving replay.
func TestMCPTool_Run_IdempotentToolReplayedAfterTransportError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fake := &controllableCallToolClient{} // fails once with EOF, then succeeds
	mgr := mcp.NewClientManager()
	mgr.SeedConfig("gws", mcp.MCPServerConfig{Command: "dummy"})
	mgr.SeedClient("gws", fake)
	sup := mcp.NewSupervisor(mgr)
	sup.Start(ctx)
	t.Cleanup(sup.Stop)
	deadline := time.Now().Add(3 * time.Second)
	for sup.HealthFor("gws").State != mcp.StateHealthy {
		if time.Now().After(deadline) {
			t.Fatal("precondition: server never became healthy")
		}
		time.Sleep(10 * time.Millisecond)
	}
	tool := mcpgo.Tool{
		Name:        "upsert_row",
		Annotations: mcpgo.ToolAnnotation{IdempotentHint: mcpgo.ToBoolPtr(true)},
	}
	mt := NewMCPTool("gws", tool, mgr)
	mt.SetSupervisor(sup)

	result, err := mt.Run(ctx, `{"key":"a"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected replayed success, got error result: %s", result.Content)
	}
	if got := fake.callToolCount.Load(); got != 2 {
		t.Fatalf("idempotent tool should be re-dispatched once, got %d dispatches", got)
	}
}

// --- Test: known-disconnected server is probed BEFORE dispatch ---
//
// 2026-07-29 incident: google-workspace was marked disconnected at 11:53; a
// 14:11 tool call was dispatched onto the stale connection anyway and sat
// ~6.5 minutes on the dead pipe before the transport error surfaced (the
// eventual reconnect then took 12 seconds). The dispatch path must consult
// supervisor health FIRST and reconcile via ProbeNow, not discover the
// corpse mid-call.
func TestMCPTool_Run_ProbesKnownDisconnectedBeforeDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := mcp.NewClientManager()
	mgr.SeedConfig("gws", mcp.MCPServerConfig{Command: "dummy"})

	sup := mcp.NewSupervisor(mgr)
	sup.Start(ctx)
	defer sup.Stop()

	// Initial probe fails (no client) → StateDisconnected. Poll instead of
	// a fixed sleep so CI load can't flake it.
	deadline := time.Now().Add(3 * time.Second)
	for sup.HealthFor("gws").State != mcp.StateDisconnected {
		if time.Now().After(deadline) {
			t.Fatalf("precondition: expected disconnected, got %v", sup.HealthFor("gws").State)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Seed a fully working client. Supervisor state is now stale — the same
	// dispatch-vs-health disagreement as the incident. The gate's contract:
	// when health says disconnected, resolve it via ProbeNow before the
	// first CallTool ever fires.
	fake := &countingSuccessClient{}
	mgr.SeedClient("gws", fake)

	mt := NewMCPTool("gws", mcpgo.Tool{Name: "search_files"}, mgr)
	mt.SetSupervisor(sup)

	result, err := mt.Run(ctx, `{"q":"x"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error result: %s", result.Content)
	}
	if got := fake.callToolCount.Load(); got != 1 {
		t.Fatalf("expected exactly 1 dispatch, got %d", got)
	}
	// The pre-dispatch probe must have reconciled the stale health state.
	if h := sup.HealthFor("gws"); h.State != mcp.StateHealthy {
		t.Fatalf("expected pre-dispatch probe to mark server healthy, got %v", h.State)
	}
}

// countingSuccessClient is successCallToolClient plus a CallTool counter.
type countingSuccessClient struct {
	successCallToolClient
	callToolCount atomic.Int32
}

func (c *countingSuccessClient) CallTool(ctx context.Context, r mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	c.callToolCount.Add(1)
	return c.successCallToolClient.CallTool(ctx, r)
}

// alwaysErrClient fails every CallTool with a fixed error (ListTools stays
// healthy so supervisor probes succeed). Optional onCall hook fires before
// returning.
type alwaysErrClient struct {
	successCallToolClient
	err           error
	onCall        func()
	callToolCount atomic.Int32
}

func (c *alwaysErrClient) CallTool(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	c.callToolCount.Add(1)
	if c.onCall != nil {
		c.onCall()
	}
	return nil, c.err
}

// newHealthySupervisedTool builds a manager+supervisor around client, waits
// for the initial probe to mark the server healthy, and returns the wired
// MCPTool. Polls instead of a fixed sleep so CI load can't flake it.
func newHealthySupervisedTool(t *testing.T, ctx context.Context, server, toolName string, client mcpclient.MCPClient) (*MCPTool, *mcp.Supervisor) {
	t.Helper()
	mgr := mcp.NewClientManager()
	mgr.SeedConfig(server, mcp.MCPServerConfig{Command: "dummy"})
	mgr.SeedClient(server, client)
	sup := mcp.NewSupervisor(mgr)
	sup.Start(ctx)
	t.Cleanup(sup.Stop)
	deadline := time.Now().Add(3 * time.Second)
	for sup.HealthFor(server).State != mcp.StateHealthy {
		if time.Now().After(deadline) {
			t.Fatal("precondition: server never became healthy")
		}
		time.Sleep(10 * time.Millisecond)
	}
	mt := NewMCPTool(server, mcpgo.Tool{Name: toolName}, mgr)
	mt.SetSupervisor(sup)
	return mt, sup
}

// A retry re-executes the tool. Timeouts and protocol errors are NOT
// evidence the server didn't act (a write tool may time out AFTER its side
// effect landed) and a protocol error will fail identically on retry — so
// only transport-level failures (dead pipe: the request provably never got a
// response channel) may re-dispatch.
func TestMCPTool_Run_NoRetryOnNonTransportError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fake := &alwaysErrClient{err: fmt.Errorf("jsonrpc: invalid params")}
	mt, _ := newHealthySupervisedTool(t, ctx, "gws", "send_message", fake)

	result, err := mt.Run(ctx, `{"to":"x"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result")
	}
	if result.SideEffectOutcomeUnknown {
		t.Fatal("a definitive protocol error must remain a normal tool response")
	}
	if got := fake.callToolCount.Load(); got != 1 {
		t.Fatalf("non-transport error must not be retried, got %d dispatches", got)
	}
}

func TestMCPTool_Run_NoRetryOnPerCallTimeoutError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fake := &alwaysErrClient{err: fmt.Errorf("no response within per-call timeout 1s: %w", context.DeadlineExceeded)}
	mt, _ := newHealthySupervisedTool(t, ctx, "gws", "send_message", fake)

	result, err := mt.Run(ctx, `{"to":"x"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result")
	}
	if got := fake.callToolCount.Load(); got != 1 {
		t.Fatalf("per-call timeout must not be retried (possible duplicate side effect), got %d dispatches", got)
	}
}

func TestMCPTool_Run_NoRetryAfterCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fake := &alwaysErrClient{err: io.EOF} // transport error — would normally retry
	fake.onCall = cancel                  // but the run ctx dies mid-call (user interrupt)
	mt, _ := newHealthySupervisedTool(t, ctx, "gws", "search", fake)

	result, err := mt.Run(ctx, `{"q":"x"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result")
	}
	if got := fake.callToolCount.Load(); got != 1 {
		t.Fatalf("cancelled ctx must not retry, got %d dispatches", got)
	}
}

// A server that is HEALTHY at registry-build time can still die mid-flight.
// 2026-07-30 live repro: a kill -9'd workspace-mcp between the supervisor's
// periodic probes left the healthy-built tool without a supervisor, so
// neither the pre-dispatch gate nor the transport-failure retry could fire —
// the model saw a hard failure instead of a reconnect. Every rebuilt MCP
// tool must carry the supervisor; holding the reference is inert for healthy
// servers (probes fire only on known-disconnected dispatch or transport
// failure).
func TestRebuildRegistryForHealth_HealthyToolsCarrySupervisor(t *testing.T) {
	baseline := agent.NewToolRegistry()
	mgr := mcp.NewClientManager()
	mgr.SeedConfig("gws", mcp.MCPServerConfig{Command: "dummy"})
	mgr.SeedToolCache("gws", []mcp.RemoteTool{{ServerName: "gws", Tool: mcpgo.Tool{Name: "search_files"}}})
	sup := mcp.NewSupervisor(mgr)

	healthStates := map[string]mcp.ServerHealth{"gws": {State: mcp.StateHealthy}}
	reg := RebuildRegistryForHealth(baseline, nil, nil, healthStates, mgr, sup)

	tool, ok := reg.Get("search_files")
	if !ok {
		t.Fatal("expected healthy server's tool to be registered")
	}
	mt, ok := tool.(*MCPTool)
	if !ok {
		t.Fatalf("expected *MCPTool, got %T", tool)
	}
	if mt.supervisor == nil {
		t.Fatal("healthy-built MCP tool must carry the supervisor — a live connection can die mid-flight")
	}
}

// --- Test 2: No cache → disconnected server tools NOT injected ---

func TestRebuildRegistryForHealth_DisconnectedNoCache(t *testing.T) {
	baseline := agent.NewToolRegistry()
	baseline.Register(&ThinkTool{})
	baseline.Register(&BrowserTool{})

	healthStates := map[string]mcp.ServerHealth{
		"playwright": {State: mcp.StateDisconnected},
	}

	// Manager with no cached tools for the disconnected server.
	mgr := mcp.NewClientManager()
	// Deliberately NOT calling mgr.SeedToolCache("playwright", ...)

	reg := RebuildRegistryForHealth(baseline, nil, nil, healthStates, mgr, nil)
	if _, ok := reg.Get("browser_navigate"); ok {
		t.Error("browser_navigate should NOT be in registry when cache is empty")
	}
	// Legacy browser should remain when no Playwright tools are present.
	if _, ok := reg.Get("browser"); !ok {
		t.Error("legacy browser should remain when no Playwright tools are present")
	}
}

// --- Test 3: No supervisor → no reconnect, error returned directly ---

func TestMCPTool_Run_NoSupervisor_NoReconnect(t *testing.T) {
	mgr := mcp.NewClientManager()
	// No client → CallTool will fail.

	tool := mcpgo.Tool{Name: "browser_navigate"}
	mt := NewMCPTool("playwright", tool, mgr)
	// Deliberately NOT calling mt.SetSupervisor(...)

	result, err := mt.Run(context.Background(), `{"url":"https://example.com"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result when server not connected and no supervisor")
	}
}

func TestMCPTool_PlaywrightXAutomationGuardBlocksBeforeDispatch(t *testing.T) {
	originalURLs := playwrightCDPPageURLs
	originalPreflight := shouldPreflightChromeForTool
	originalEnsure := ensureChromeDebugPort
	shouldPreflightChromeForTool = func(int) bool { return false }
	t.Cleanup(func() {
		playwrightCDPPageURLs = originalURLs
		shouldPreflightChromeForTool = originalPreflight
		ensureChromeDebugPort = originalEnsure
	})

	run := func(t *testing.T, name, args string, urls func(int) ([]string, error)) (agent.ToolResult, int32) {
		t.Helper()
		fake := &countingSuccessClient{}
		mgr := mcp.NewClientManager()
		mgr.SeedConfig("playwright", mcp.MCPServerConfig{
			Command: "dummy",
			Args:    []string{"--cdp-endpoint", "http://127.0.0.1:9223"},
		})
		mgr.SeedClient("playwright", fake)
		playwrightCDPPageURLs = urls
		result, err := NewMCPTool("playwright", mcpgo.Tool{Name: name}, mgr).Run(
			context.Background(), args,
		)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return result, fake.callToolCount.Load()
	}

	t.Run("direct composer navigate", func(t *testing.T) {
		result, calls := run(t, "browser_navigate", `{"url":"https://x.com/intent/tweet?text=hello"}`,
			func(int) ([]string, error) { t.Fatal("direct URL guard queried CDP"); return nil, nil })
		if !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness || calls != 0 {
			t.Fatalf("result=%#v dispatches=%d", result, calls)
		}
	})

	t.Run("composer target mutation", func(t *testing.T) {
		result, calls := run(t, "browser_click", `{"ref":"e1"}`,
			func(int) ([]string, error) { return []string{"https://x.com/compose/post"}, nil })
		if !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness || calls != 0 {
			t.Fatalf("result=%#v dispatches=%d", result, calls)
		}
	})

	for _, tc := range []struct {
		name string
		tool string
		args string
	}{
		{name: "home inline composer type", tool: "browser_type", args: `{"element":"Post text","ref":"e42","text":"draft"}`},
		{name: "home inline composer click", tool: "browser_click", args: `{"element":"Post","ref":"e43"}`},
	} {
		t.Run(tc.name+" blocked by X target not element token", func(t *testing.T) {
			result, calls := run(t, tc.tool, tc.args,
				func(int) ([]string, error) { return []string{"https://x.com/home"}, nil })
			if !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness || calls != 0 ||
				!strings.Contains(result.Content, "inline composer") {
				t.Fatalf("result=%#v dispatches=%d", result, calls)
			}
		})
	}

	t.Run("explicit composer hint survives URL failure", func(t *testing.T) {
		result, calls := run(t, "browser_click", `{"element":"[data-testid=tweetButton]"}`,
			func(int) ([]string, error) { return nil, fmt.Errorf("CDP unavailable") })
		if !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness || calls != 0 {
			t.Fatalf("result=%#v dispatches=%d", result, calls)
		}
	})

	t.Run("unverifiable target fails closed", func(t *testing.T) {
		result, calls := run(t, "browser_click", `{"ref":"e1"}`,
			func(int) ([]string, error) { return nil, fmt.Errorf("CDP unavailable") })
		if !result.IsError || result.ErrorCategory != agent.ErrCategoryTransient || calls != 0 ||
			strings.Contains(result.Content, "X composer automation") {
			t.Fatalf("result=%#v dispatches=%d", result, calls)
		}
	})

	t.Run("non-CDP ordinary mutation remains available", func(t *testing.T) {
		fake := &countingSuccessClient{}
		mgr := mcp.NewClientManager()
		mgr.SeedConfig("playwright", mcp.MCPServerConfig{Command: "dummy", Args: []string{"--headless"}})
		mgr.SeedClient("playwright", fake)
		result, err := NewMCPTool("playwright", mcpgo.Tool{Name: "browser_click"}, mgr).Run(
			context.Background(), `{"ref":"e1"}`,
		)
		if err != nil || result.IsError || fake.callToolCount.Load() != 1 {
			t.Fatalf("result=%#v err=%v dispatches=%d", result, err, fake.callToolCount.Load())
		}
	})

	for _, name := range []string{"browser_run_code", "browser_evaluate"} {
		t.Run(name+" arbitrary code bypass disabled", func(t *testing.T) {
			result, calls := run(t, name,
				`{"code":"await page.goto('https://x.com/compose/post'); await page.getByTestId('tweetButton').click()"}`,
				func(int) ([]string, error) { t.Fatal("unrestricted code guard queried CDP"); return nil, nil })
			if !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness || calls != 0 ||
				!strings.Contains(result.Content, "disabled") {
				t.Fatalf("result=%#v dispatches=%d", result, calls)
			}
		})
	}

	t.Run("Chrome is ready before target inspection", func(t *testing.T) {
		ready := false
		shouldPreflightChromeForTool = func(int) bool { return true }
		ensureChromeDebugPort = func(int) error { ready = true; return nil }
		result, calls := run(t, "browser_click", `{"ref":"e1"}`,
			func(int) ([]string, error) {
				if !ready {
					t.Fatal("target URLs inspected before dedicated Chrome preflight")
				}
				return []string{"https://example.com/read"}, nil
			})
		shouldPreflightChromeForTool = func(int) bool { return false }
		ensureChromeDebugPort = originalEnsure
		if result.IsError || calls != 1 {
			t.Fatalf("result=%#v dispatches=%d", result, calls)
		}
	})

	t.Run("composer observation remains available", func(t *testing.T) {
		result, calls := run(t, "browser_snapshot", `{}`,
			func(int) ([]string, error) { t.Fatal("observation guard queried CDP"); return nil, nil })
		if result.IsError || calls != 1 {
			t.Fatalf("result=%#v dispatches=%d", result, calls)
		}
	})

	t.Run("ordinary X navigation remains available", func(t *testing.T) {
		result, calls := run(t, "browser_navigate", `{"url":"https://x.com/home"}`,
			func(int) ([]string, error) { t.Fatal("ordinary navigate queried CDP"); return nil, nil })
		if result.IsError || calls != 1 {
			t.Fatalf("result=%#v dispatches=%d", result, calls)
		}
	})
}

type orderedCallClient struct {
	successCallToolClient
	mu    sync.Mutex
	calls []string
}

func (c *orderedCallClient) CallTool(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	c.mu.Lock()
	c.calls = append(c.calls, req.Params.Name)
	c.mu.Unlock()
	return c.successCallToolClient.CallTool(ctx, req)
}

func (c *orderedCallClient) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

func TestMCPTool_PlaywrightTargetCheckAndCallAreSerializedAcrossRuns(t *testing.T) {
	originalURLs := playwrightCDPPageURLs
	originalPreflight := shouldPreflightChromeForTool
	playwrightCDPPageURLs = originalURLs
	shouldPreflightChromeForTool = func(int) bool { return false }
	t.Cleanup(func() {
		playwrightCDPPageURLs = originalURLs
		shouldPreflightChromeForTool = originalPreflight
	})

	guardEntered := make(chan struct{})
	releaseGuard := make(chan struct{})
	playwrightCDPPageURLs = func(int) ([]string, error) {
		close(guardEntered)
		<-releaseGuard
		return []string{"https://example.com/read"}, nil
	}

	client := &orderedCallClient{}
	mgr := mcp.NewClientManager()
	mgr.SeedConfig("playwright", mcp.MCPServerConfig{
		Command: "dummy",
		Args:    []string{"--cdp-endpoint", "http://127.0.0.1:9223"},
	})
	mgr.SeedClient("playwright", client)
	mutation := NewMCPTool("playwright", mcpgo.Tool{Name: "browser_click"}, mgr)
	tabSwitch := NewMCPTool("playwright", mcpgo.Tool{Name: "browser_tabs"}, mgr)

	mutationDone := make(chan agent.ToolResult, 1)
	go func() {
		result, _ := mutation.Run(context.Background(), `{"ref":"e1"}`)
		mutationDone <- result
	}()
	<-guardEntered

	tabDone := make(chan agent.ToolResult, 1)
	tabStarted := make(chan struct{})
	go func() {
		close(tabStarted)
		result, _ := tabSwitch.Run(context.Background(), `{"action":"select","index":1}`)
		tabDone <- result
	}()
	<-tabStarted
	time.Sleep(25 * time.Millisecond)
	if got := client.snapshot(); len(got) != 0 {
		t.Fatalf("another Run dispatched between target check and mutation: %v", got)
	}
	close(releaseGuard)

	if result := <-mutationDone; result.IsError {
		t.Fatalf("mutation failed: %#v", result)
	}
	if result := <-tabDone; result.IsError {
		t.Fatalf("tab switch failed: %#v", result)
	}
	if got := client.snapshot(); len(got) != 2 || got[0] != "browser_click" || got[1] != "browser_tabs" {
		t.Fatalf("dispatch order = %v, want guarded mutation before competing Run", got)
	}
}

func TestMCPTool_Run_AllowsRequiredZeroValues(t *testing.T) {
	mgr := mcp.NewClientManager()
	mgr.SeedClient("remote", &successCallToolClient{})
	tool := mcpgo.Tool{
		Name: "set_options",
		InputSchema: mcpgo.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"enabled": map[string]any{"type": "boolean"},
				"offset":  map[string]any{"type": "number"},
				"query":   map[string]any{"type": "string"},
				"items":   map[string]any{"type": "array"},
			},
			Required: []string{"enabled", "offset", "query", "items"},
		},
	}

	result, err := NewMCPTool("remote", tool, mgr).Run(
		context.Background(),
		`{"enabled":false,"offset":0,"query":"","items":[]}`,
	)
	if err != nil || result.IsError {
		t.Fatalf("required zero values were rejected: err=%v result=%#v", err, result)
	}
}

func TestMCPTool_Run_PreflightsDedicatedChromeWhenAlreadyConnected(t *testing.T) {
	mgr := mcp.NewClientManager()
	mgr.SeedConfig("playwright", mcp.MCPServerConfig{
		Command: "dummy",
		Args:    []string{"--cdp-endpoint", "http://127.0.0.1:9223"},
	})
	mgr.SeedClient("playwright", &successCallToolClient{})

	origEnsure := ensureChromeDebugPort
	origShouldPreflight := shouldPreflightChromeForTool
	t.Cleanup(func() {
		ensureChromeDebugPort = origEnsure
		shouldPreflightChromeForTool = origShouldPreflight
	})

	var ensureCalls atomic.Int32
	ensureChromeDebugPort = func(port int) error {
		ensureCalls.Add(1)
		if port != 9223 {
			t.Fatalf("expected dedicated port 9223, got %d", port)
		}
		return nil
	}
	shouldPreflightChromeForTool = func(port int) bool {
		return port == 9223
	}

	mt := NewMCPTool("playwright", mcpgo.Tool{Name: "browser_navigate"}, mgr)
	result, err := mt.Run(context.Background(), `{"url":"https://example.com"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error result: %s", result.Content)
	}
	if got := ensureCalls.Load(); got != 1 {
		t.Fatalf("expected 1 dedicated Chrome preflight, got %d", got)
	}
}

// --- Fake MCP client with controllable CallTool ---

// controllableCallToolClient is a minimal MCPClient where CallTool fails on the
// first call (io.EOF) and succeeds on subsequent calls. ListTools always succeeds
// so the supervisor's transport probe can mark the server healthy.
type controllableCallToolClient struct {
	callToolCount atomic.Int32
}

type successCallToolClient struct{}

func (c *controllableCallToolClient) Initialize(context.Context, mcpgo.InitializeRequest) (*mcpgo.InitializeResult, error) {
	return &mcpgo.InitializeResult{}, nil
}
func (c *successCallToolClient) Initialize(context.Context, mcpgo.InitializeRequest) (*mcpgo.InitializeResult, error) {
	return &mcpgo.InitializeResult{}, nil
}
func (c *controllableCallToolClient) Ping(context.Context) error { return nil }
func (c *successCallToolClient) Ping(context.Context) error      { return nil }
func (c *controllableCallToolClient) ListResourcesByPage(context.Context, mcpgo.ListResourcesRequest) (*mcpgo.ListResourcesResult, error) {
	return &mcpgo.ListResourcesResult{}, nil
}
func (c *successCallToolClient) ListResourcesByPage(context.Context, mcpgo.ListResourcesRequest) (*mcpgo.ListResourcesResult, error) {
	return &mcpgo.ListResourcesResult{}, nil
}
func (c *controllableCallToolClient) ListResources(context.Context, mcpgo.ListResourcesRequest) (*mcpgo.ListResourcesResult, error) {
	return &mcpgo.ListResourcesResult{}, nil
}
func (c *successCallToolClient) ListResources(context.Context, mcpgo.ListResourcesRequest) (*mcpgo.ListResourcesResult, error) {
	return &mcpgo.ListResourcesResult{}, nil
}
func (c *controllableCallToolClient) ListResourceTemplatesByPage(context.Context, mcpgo.ListResourceTemplatesRequest) (*mcpgo.ListResourceTemplatesResult, error) {
	return &mcpgo.ListResourceTemplatesResult{}, nil
}
func (c *successCallToolClient) ListResourceTemplatesByPage(context.Context, mcpgo.ListResourceTemplatesRequest) (*mcpgo.ListResourceTemplatesResult, error) {
	return &mcpgo.ListResourceTemplatesResult{}, nil
}
func (c *controllableCallToolClient) ListResourceTemplates(context.Context, mcpgo.ListResourceTemplatesRequest) (*mcpgo.ListResourceTemplatesResult, error) {
	return &mcpgo.ListResourceTemplatesResult{}, nil
}
func (c *successCallToolClient) ListResourceTemplates(context.Context, mcpgo.ListResourceTemplatesRequest) (*mcpgo.ListResourceTemplatesResult, error) {
	return &mcpgo.ListResourceTemplatesResult{}, nil
}
func (c *controllableCallToolClient) ReadResource(context.Context, mcpgo.ReadResourceRequest) (*mcpgo.ReadResourceResult, error) {
	return &mcpgo.ReadResourceResult{}, nil
}
func (c *successCallToolClient) ReadResource(context.Context, mcpgo.ReadResourceRequest) (*mcpgo.ReadResourceResult, error) {
	return &mcpgo.ReadResourceResult{}, nil
}
func (c *controllableCallToolClient) Subscribe(context.Context, mcpgo.SubscribeRequest) error {
	return nil
}
func (c *successCallToolClient) Subscribe(context.Context, mcpgo.SubscribeRequest) error {
	return nil
}
func (c *controllableCallToolClient) Unsubscribe(context.Context, mcpgo.UnsubscribeRequest) error {
	return nil
}
func (c *successCallToolClient) Unsubscribe(context.Context, mcpgo.UnsubscribeRequest) error {
	return nil
}
func (c *controllableCallToolClient) ListPromptsByPage(context.Context, mcpgo.ListPromptsRequest) (*mcpgo.ListPromptsResult, error) {
	return &mcpgo.ListPromptsResult{}, nil
}
func (c *successCallToolClient) ListPromptsByPage(context.Context, mcpgo.ListPromptsRequest) (*mcpgo.ListPromptsResult, error) {
	return &mcpgo.ListPromptsResult{}, nil
}
func (c *controllableCallToolClient) ListPrompts(context.Context, mcpgo.ListPromptsRequest) (*mcpgo.ListPromptsResult, error) {
	return &mcpgo.ListPromptsResult{}, nil
}
func (c *successCallToolClient) ListPrompts(context.Context, mcpgo.ListPromptsRequest) (*mcpgo.ListPromptsResult, error) {
	return &mcpgo.ListPromptsResult{}, nil
}
func (c *controllableCallToolClient) GetPrompt(context.Context, mcpgo.GetPromptRequest) (*mcpgo.GetPromptResult, error) {
	return &mcpgo.GetPromptResult{}, nil
}
func (c *successCallToolClient) GetPrompt(context.Context, mcpgo.GetPromptRequest) (*mcpgo.GetPromptResult, error) {
	return &mcpgo.GetPromptResult{}, nil
}
func (c *controllableCallToolClient) ListToolsByPage(context.Context, mcpgo.ListToolsRequest) (*mcpgo.ListToolsResult, error) {
	return &mcpgo.ListToolsResult{}, nil
}
func (c *successCallToolClient) ListToolsByPage(context.Context, mcpgo.ListToolsRequest) (*mcpgo.ListToolsResult, error) {
	return &mcpgo.ListToolsResult{}, nil
}
func (c *controllableCallToolClient) ListTools(context.Context, mcpgo.ListToolsRequest) (*mcpgo.ListToolsResult, error) {
	return &mcpgo.ListToolsResult{}, nil
}
func (c *successCallToolClient) ListTools(context.Context, mcpgo.ListToolsRequest) (*mcpgo.ListToolsResult, error) {
	return &mcpgo.ListToolsResult{}, nil
}
func (c *controllableCallToolClient) CallTool(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	n := c.callToolCount.Add(1)
	if n == 1 {
		return nil, io.EOF // transport error → triggers reconnect path
	}
	return mcpgo.NewToolResultText("ok"), nil
}
func (c *successCallToolClient) CallTool(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return mcpgo.NewToolResultText("ok"), nil
}
func (c *controllableCallToolClient) SetLevel(context.Context, mcpgo.SetLevelRequest) error {
	return nil
}
func (c *successCallToolClient) SetLevel(context.Context, mcpgo.SetLevelRequest) error {
	return nil
}
func (c *controllableCallToolClient) Complete(context.Context, mcpgo.CompleteRequest) (*mcpgo.CompleteResult, error) {
	return &mcpgo.CompleteResult{}, nil
}
func (c *successCallToolClient) Complete(context.Context, mcpgo.CompleteRequest) (*mcpgo.CompleteResult, error) {
	return &mcpgo.CompleteResult{}, nil
}
func (c *controllableCallToolClient) Close() error { return nil }
func (c *successCallToolClient) Close() error      { return nil }
func (c *controllableCallToolClient) OnNotification(func(mcpgo.JSONRPCNotification)) {
}
func (c *successCallToolClient) OnNotification(func(mcpgo.JSONRPCNotification)) {}

func TestMCPTool_PlaywrightDispatchMarksChromeUsed(t *testing.T) {
	assertGlobalChromeTrackerClean(t)

	ctx := mcp.WithChromeUseLease(context.Background())
	lease := mcp.ChromeUseLeaseFrom(ctx)
	if lease == nil {
		t.Fatal("expected lease installed")
	}
	defer lease.ReleaseOnly() // panic-safe cleanup to keep global tracker clean

	mgr := mcp.NewClientManager()
	mgr.SeedConfig("playwright", mcp.MCPServerConfig{
		Command: "dummy",
		Args:    []string{"--cdp-endpoint", "http://127.0.0.1:9223"},
	})

	tool := NewMCPTool("playwright", mcpgo.Tool{Name: "browser_navigate"}, mgr)

	// Stub ensureChromeDebugPort so the dispatch path runs without needing
	// real Chrome / network. CallTool will fail (no real MCP server) — we
	// don't care; we only care that MarkChromeUsed ran on the CDP path,
	// BEFORE the failure path.
	oldEnsure := ensureChromeDebugPort
	ensureChromeDebugPort = func(int) error { return nil }
	defer func() { ensureChromeDebugPort = oldEnsure }()

	_, _ = tool.Run(ctx, `{"url":"about:blank"}`)

	if got := mcp.GlobalChromeTrackerActiveCountForTest(); got < 1 {
		t.Fatalf("expected playwright dispatch to mark chrome used, count=%d", got)
	}
}

func TestMCPTool_NonCDPPlaywrightDoesNotMarkChromeUsed(t *testing.T) {
	assertGlobalChromeTrackerClean(t)

	ctx := mcp.WithChromeUseLease(context.Background())

	mgr := mcp.NewClientManager()
	mgr.SeedConfig("playwright", mcp.MCPServerConfig{
		Command: "dummy",
		Args:    []string{"--some-stdio-mode"},
	})
	mgr.SeedClient("playwright", &successCallToolClient{})

	tool := NewMCPTool("playwright", mcpgo.Tool{Name: "browser_navigate"}, mgr)
	_, _ = tool.Run(ctx, `{"url":"about:blank"}`)

	if got := mcp.GlobalChromeTrackerActiveCountForTest(); got != 0 {
		t.Fatalf("expected non-CDP playwright dispatch to NOT mark chrome used, count=%d", got)
	}
}

func TestMCPTool_NonPlaywrightDispatchDoesNotMarkChromeUsed(t *testing.T) {
	assertGlobalChromeTrackerClean(t)

	ctx := mcp.WithChromeUseLease(context.Background())

	mgr := mcp.NewClientManager()
	mgr.SeedConfig("filesystem", mcp.MCPServerConfig{Command: "dummy"})

	tool := NewMCPTool("filesystem", mcpgo.Tool{Name: "read_file"}, mgr)

	_, _ = tool.Run(ctx, `{"path":"/tmp/x"}`)

	if got := mcp.GlobalChromeTrackerActiveCountForTest(); got != 0 {
		t.Fatalf("expected non-playwright dispatch to NOT mark chrome used, count=%d", got)
	}
}

// assertGlobalChromeTrackerClean fails fast if a prior test leaked global
// tracker state. Tests that exercise the global tracker must call this first.
func assertGlobalChromeTrackerClean(t *testing.T) {
	t.Helper()
	if got := mcp.GlobalChromeTrackerActiveCountForTest(); got != 0 {
		t.Fatalf("global chrome tracker leaked count=%d from a prior test", got)
	}
}
