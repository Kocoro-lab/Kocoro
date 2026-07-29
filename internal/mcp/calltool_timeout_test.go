package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// hangingCallToolClient simulates the wedged-alive MCP subprocess from the
// 2026-07-29 incident: the process accepts the request write and never
// replies. CallTool blocks until the caller's ctx is cancelled.
type hangingCallToolClient struct {
	fakeListToolsClient
}

func (h *hangingCallToolClient) CallTool(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// Per-server override: MCPServerConfig.ToolTimeoutSeconds bounds a single
// tools/call attempt even when the caller's ctx has no deadline.
func TestCallTool_PerServerTimeoutBoundsHangingCall(t *testing.T) {
	mgr := NewClientManager()
	mgr.SetSupervised(true) // production daemon mode: no inline reconnect
	mgr.SeedConfig("slow", MCPServerConfig{Command: "dummy", ToolTimeoutSeconds: 1})
	mgr.SeedClient("slow", &hangingCallToolClient{})

	start := time.Now()
	_, isError, err := mgr.CallTool(context.Background(), "slow", "search", nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !isError {
		t.Error("expected isError=true on timeout")
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("returned before the 1s per-call timeout: %v", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Errorf("per-call timeout did not bind, took %v", elapsed)
	}
}

// Manager-level default (wired from mcp.tool_timeout_secs) applies when the
// server config carries no override.
func TestCallTool_ManagerDefaultTimeoutBoundsHangingCall(t *testing.T) {
	mgr := NewClientManager()
	mgr.SetSupervised(true)
	mgr.SetToolCallTimeout(100 * time.Millisecond)
	mgr.SeedConfig("slow", MCPServerConfig{Command: "dummy"})
	mgr.SeedClient("slow", &hangingCallToolClient{})

	start := time.Now()
	_, _, err := mgr.CallTool(context.Background(), "slow", "search", nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed > 3*time.Second {
		t.Errorf("manager default timeout did not bind, took %v", elapsed)
	}
}

// An already-short caller deadline must win over a longer per-call timeout —
// the wrap may only tighten, never extend.
func TestCallTool_CallerDeadlineStillWins(t *testing.T) {
	mgr := NewClientManager()
	mgr.SetSupervised(true)
	mgr.SeedConfig("slow", MCPServerConfig{Command: "dummy", ToolTimeoutSeconds: 60})
	mgr.SeedClient("slow", &hangingCallToolClient{})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _, err := mgr.CallTool(ctx, "slow", "search", nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from caller deadline, got nil")
	}
	if elapsed > 3*time.Second {
		t.Errorf("caller deadline was extended by per-call timeout, took %v", elapsed)
	}
}

// The default constant pins the industry-consensus value (Codex and Hermes
// both default MCP tool calls to 300s).
func TestDefaultToolCallTimeoutValue(t *testing.T) {
	if DefaultToolCallTimeout != 300*time.Second {
		t.Fatalf("DefaultToolCallTimeout = %v, want 300s", DefaultToolCallTimeout)
	}
}
