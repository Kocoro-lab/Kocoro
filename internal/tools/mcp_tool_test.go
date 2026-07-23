package tools

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
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
	time.Sleep(100 * time.Millisecond)

	h := sup.HealthFor("playwright")
	if h.State != mcp.StateDisconnected {
		t.Fatalf("expected disconnected after initial probe, got %v", h.State)
	}

	// Now inject a controllable client: CallTool fails once (io.EOF), then
	// succeeds. ListTools always succeeds (so the transport probe works).
	fake := &controllableCallToolClient{}
	mgr.SeedClient("playwright", fake)

	// Create MCPTool with supervisor for on-demand reconnect.
	tool := mcpgo.Tool{Name: "browser_navigate"}
	mt := NewMCPTool("playwright", tool, mgr)
	mt.SetSupervisor(sup)

	result, err := mt.Run(ctx, `{"url":"https://example.com"}`)
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
