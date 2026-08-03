package mcp

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Same dying-server shape as the tools-package live test, but through the
// UNSUPERVISED ClientManager path (one-shot / TUI without supervisor): the
// inline reconnect-and-retry must not re-dispatch an unannotated write.
// 2026-08-03 e2e: a real `shan -y` run against this exact server committed
// twice, which the supervised-only live test did not catch.
const dyingUnsupervisedServerScript = `import json, os, sys
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
            "tools": [{"name": "e2e_commit_write", "description": "write",
                       "inputSchema": {"type": "object"}}]}}
    elif m == "tools/call":
        f = open(log, "a")
        f.write("COMMIT\n")
        f.flush()
        os.fsync(f.fileno())
        f.close()
        os._exit(0)
    else:
        continue
    sys.stdout.write(json.dumps(resp) + "\n")
    sys.stdout.flush()
`

func TestCallTool_Unsupervised_WriteNotReplayedWhenServerDiesBeforeResponse(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "dying_server.py")
	if err := os.WriteFile(script, []byte(dyingUnsupervisedServerScript), 0o600); err != nil {
		t.Fatal(err)
	}
	commitLog := filepath.Join(dir, "commits.log")

	mgr := NewClientManager() // unsupervised: inline reconnect path active
	defer mgr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg := MCPServerConfig{
		Command:            python,
		Args:               []string{script},
		Env:                map[string]string{"COMMIT_LOG": commitLog},
		ToolTimeoutSeconds: 10,
	}
	if _, err := mgr.ConnectAll(ctx, map[string]MCPServerConfig{"dying": cfg}); err != nil {
		t.Fatalf("ConnectAll: %v", err)
	}

	_, isErr, callErr := mgr.CallTool(ctx, "dying", "e2e_commit_write", map[string]any{"to": "x"})
	if callErr == nil || !isErr {
		t.Fatalf("expected error, got isErr=%v err=%v", isErr, callErr)
	}
	var ou *OutcomeUnknownError
	if !errors.As(callErr, &ou) {
		t.Errorf("expected OutcomeUnknownError, got %T: %v", callErr, callErr)
	}
	data, _ := os.ReadFile(commitLog)
	if got := strings.Count(string(data), "COMMIT\n"); got != 1 {
		t.Fatalf("write must be committed exactly once, got %d", got)
	}
}
