package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
)

// dyingWriteServerScript is a minimal REAL stdio MCP server reproducing the
// duplicate-write bug: on tools/call it durably commits its side effect (a line
// appended + fsync'd to COMMIT_LOG), then exits BEFORE writing the JSON-RPC
// response. On the wire the client sees only a dead transport — provably
// indistinguishable from "the server never acted". It advertises two tools:
//   - send_message: unannotated write → must NOT be auto-replayed
//   - list_messages: readOnlyHint=true → replay allowed
// Both commit-and-die, so the commit count in COMMIT_LOG is exactly the
// number of dispatches that reached a server.
const dyingWriteServerScript = `import json, os, sys
log = os.environ["COMMIT_LOG"]
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
            "serverInfo": {"name": "dying", "version": "0"}}}
    elif m == "tools/list":
        resp = {"jsonrpc": "2.0", "id": req["id"], "result": {
            "tools": [
                {"name": "send_message", "description": "unannotated write",
                 "inputSchema": {"type": "object"}},
                {"name": "list_messages", "description": "annotated read",
                 "inputSchema": {"type": "object"},
                 "annotations": {"readOnlyHint": True}}]}}
    elif m == "tools/call":
        f = open(log, "a")
        f.write("COMMIT\n")
        f.flush()
        os.fsync(f.fileno())
        f.close()
        os._exit(0)  # die before the response — the in-doubt window
    else:
        continue
    sys.stdout.write(json.dumps(resp) + "\n")
    sys.stdout.flush()
`

// startDyingServer connects a real supervised manager+supervisor to the
// dying server and returns the wired MCPTool for toolName plus the commit
// log path. No seams are faked: real mcp-go stdio client, real subprocess,
// real supervisor probes and on-demand reconnect.
func startDyingServer(t *testing.T, toolName string) (*MCPTool, string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "dying_server.py")
	if err := os.WriteFile(script, []byte(dyingWriteServerScript), 0o600); err != nil {
		t.Fatal(err)
	}
	commitLog := filepath.Join(dir, "commits.log")

	mgr := mcp.NewClientManager()
	mgr.SetSupervised(true) // production daemon mode: retry decision lives in MCPTool
	t.Cleanup(func() { mgr.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	cfg := mcp.MCPServerConfig{
		Command:            python,
		Args:               []string{script},
		Env:                map[string]string{"COMMIT_LOG": commitLog},
		ToolTimeoutSeconds: 10,
	}
	tools, err := mgr.ConnectAll(ctx, map[string]mcp.MCPServerConfig{"dying": cfg})
	if err != nil {
		t.Fatalf("ConnectAll failed at handshake: %v", err)
	}

	sup := mcp.NewSupervisor(mgr)
	sup.Start(ctx)
	t.Cleanup(sup.Stop)
	deadline := time.Now().Add(5 * time.Second)
	for sup.HealthFor("dying").State != mcp.StateHealthy {
		if time.Now().After(deadline) {
			t.Fatalf("precondition: server never became healthy: %v", sup.HealthFor("dying").State)
		}
		time.Sleep(20 * time.Millisecond)
	}

	for _, rt := range tools {
		if rt.Tool.Name == toolName {
			mt := NewMCPTool("dying", rt.Tool, mgr)
			mt.SetSupervisor(sup)
			return mt, commitLog
		}
	}
	t.Fatalf("tool %q not advertised; got %+v", toolName, tools)
	return nil, ""
}

func countCommits(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "COMMIT\n")
}

// The full-chain duplicate-write repro: server executes the write, fsyncs, and dies
// before responding. Pre-fix, the transport-error retry re-dispatched the
// call after reconnect and the commit count reached 2. Post-fix the
// unannotated write is dispatched exactly once and the model sees an
// explicit outcome-unknown result.
func TestMCPTool_Run_RealStdio_WriteCommittedOnceWhenServerDiesBeforeResponse(t *testing.T) {
	mt, commitLog := startDyingServer(t, "send_message")

	result, err := mt.Run(context.Background(), `{"to":"x"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected error result, got success: %s", result.Content)
	}
	if !strings.Contains(result.Content, "outcome UNKNOWN") {
		t.Errorf("result must surface the in-doubt outcome, got: %s", result.Content)
	}
	if got := countCommits(t, commitLog); got != 1 {
		t.Fatalf("write must be committed exactly once, got %d commits", got)
	}
}

// Counterpart with a real server: the readOnlyHint-annotated tool keeps the
// availability-preserving replay — after the supervisor's on-demand
// reconnect the call is re-dispatched, so the (also-dying) server records a
// second dispatch.
func TestMCPTool_Run_RealStdio_ReadOnlyToolIsReplayedAfterServerDeath(t *testing.T) {
	mt, commitLog := startDyingServer(t, "list_messages")

	result, err := mt.Run(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The replayed dispatch dies too, so the final result is still an error —
	// what matters is that the replay was ALLOWED to happen.
	if !result.IsError {
		t.Fatalf("expected error result from twice-dying server, got: %s", result.Content)
	}
	// >= 2, not == 2: the property under test is that replay was ALLOWED.
	// An exact count couples the assertion to ProbeNow reconnect timing
	// racing the background probe loop, which can flake on a loaded runner.
	if got := countCommits(t, commitLog); got < 2 {
		t.Fatalf("read-only tool should have been re-dispatched (>=2 dispatches), got %d", got)
	}
}
