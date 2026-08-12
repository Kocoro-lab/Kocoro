//go:build live

package mcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// wedgedServerScript is a minimal REAL stdio MCP server: it completes the
// initialize handshake and advertises one tool, then never replies to
// tools/call — the wedged-alive subprocess from the 2026-07-29 incident.
// Each tools/call dispatch is appended to the file in WEDGE_DISPATCH_LOG so
// the test can assert exactly-once dispatch.
const wedgedServerScript = `import json, os, sys
log = os.environ["WEDGE_DISPATCH_LOG"]
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    req = json.loads(line)
    m = req.get("method")
    if m == "initialize":
        resp = {"jsonrpc": "2.0", "id": req["id"], "result": {
            "protocolVersion": req["params"]["protocolVersion"],
            "capabilities": {"tools": {}},
            "serverInfo": {"name": "wedge", "version": "0"}}}
    elif m == "tools/list":
        resp = {"jsonrpc": "2.0", "id": req["id"], "result": {
            "tools": [{"name": "hang", "description": "accepts the request, never replies",
                       "inputSchema": {"type": "object"}}]}}
    elif m == "tools/call":
        with open(log, "a") as f:
            f.write("DISPATCH\n")
        continue  # wedged-alive: never reply
    else:
        continue  # notifications etc.
    sys.stdout.write(json.dumps(resp) + "\n")
    sys.stdout.flush()
`

// The full production error chain, no seams faked: real mcp-go stdio client →
// real wedged subprocess → per-call timeout. mcp-go wraps the stdio
// transport's bare ctx.Err() in *transport.Error (client.go SendRequest), so
// classifying by wrapper type alone would call this a transport failure and
// re-dispatch a non-idempotent call that already ran for the full budget.
// The unit test pins the chain shape; this pins the chain as mcp-go actually
// produces it, plus exactly-once dispatch at the server.
func TestCallTool_RealStdioWedgedTimeoutIsNotTransportError(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "wedge_server.py")
	if err := os.WriteFile(script, []byte(wedgedServerScript), 0o600); err != nil {
		t.Fatal(err)
	}
	dispatchLog := filepath.Join(dir, "dispatch.log")

	mgr := NewClientManager()
	mgr.SetSupervised(true) // production daemon mode: no inline reconnect
	defer mgr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg := MCPServerConfig{
		Command:            python,
		Args:               []string{script},
		Env:                map[string]string{"WEDGE_DISPATCH_LOG": dispatchLog},
		ToolTimeoutSeconds: 1,
	}
	tools, err := mgr.ConnectAll(ctx, map[string]MCPServerConfig{"wedge": cfg})
	if err != nil {
		t.Fatalf("ConnectAll against the wedged server failed at handshake: %v", err)
	}
	if len(tools) != 1 || tools[0].Tool.Name != "hang" {
		t.Fatalf("expected the single hang tool, got %+v", tools)
	}

	start := time.Now()
	_, isError, callErr := mgr.CallTool(ctx, "wedge", "hang", nil)
	elapsed := time.Since(start)

	if callErr == nil {
		t.Fatal("expected per-call timeout error, got nil")
	}
	if !isError {
		t.Error("expected isError=true on timeout")
	}
	if elapsed < 900*time.Millisecond || elapsed > 10*time.Second {
		t.Errorf("per-call timeout did not bind as configured: %v", elapsed)
	}
	if !strings.Contains(callErr.Error(), "per-call timeout") {
		t.Errorf("error should carry the per-call timeout hint, got: %v", callErr)
	}
	// THE assertion: the real mcp-go timeout chain must not classify as a
	// transport failure — this is what gates the mcp_tool.go retry.
	if IsTransportError(callErr) {
		t.Fatalf("real per-call timeout classified as transport error (would re-dispatch a non-idempotent call): %v", callErr)
	}

	data, err := os.ReadFile(dispatchLog)
	if err != nil {
		t.Fatalf("dispatch log unreadable — tools/call never reached the server: %v", err)
	}
	if got := strings.Count(string(data), "DISPATCH"); got != 1 {
		t.Fatalf("expected exactly one tools/call dispatch, got %d", got)
	}
}
