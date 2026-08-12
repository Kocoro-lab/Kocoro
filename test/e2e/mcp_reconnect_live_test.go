package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
)

// TestLive_MCPReconnect_RecoversFromFailedConnect qualifies the recovery half
// of the MCP connect path: a server whose first spawns fail must come up on
// its own, with no user action and no daemon restart.
//
// Before internal/mcp/reconnect.go a failed async connect was terminal — the
// result callback logged it, audited it, and returned. Rotating a credential
// rebuilds the manager and respawns the stdio child, and when that single
// attempt lost the race with the SIGTERM'd process group (npx spawns
// npx -> npm exec -> node, which can hold a port briefly), the server stayed
// "enabled but disconnected" until the daemon restarted. Reload still
// answered {"status":"reloaded"}, so the UI reported success while the tools
// stayed gone.
//
// The fixture reproduces that shape deterministically: the first two spawns
// exit non-zero, the third execs a real `shan mcp serve` over stdio. Recovery
// therefore has to cross two backoff rungs (5s + 10s) and complete a real
// JSON-RPC handshake — no fakes below the daemon boundary.
func TestLive_MCPReconnect_RecoversFromFailedConnect(t *testing.T) {
	skipUnlessLive(t)
	if runtime.GOOS == "windows" {
		t.Skip("fixture is a POSIX shell script")
	}

	bin := testBinary(t)
	fixtureDir := t.TempDir()
	countFile := filepath.Join(fixtureDir, "spawn-count")
	serverStateDir := filepath.Join(fixtureDir, "mcp-server-state")
	if err := os.MkdirAll(serverStateDir, 0o700); err != nil {
		t.Fatalf("create MCP server state dir: %v", err)
	}

	// failUntil is the number of spawns that fail before the fixture serves.
	// Two failures force the retry ladder across its first two rungs.
	const failUntil = 2
	script := filepath.Join(fixtureDir, "flaky-mcp.sh")
	body := fmt.Sprintf(`#!/bin/sh
COUNT_FILE=%q
N=$(cat "$COUNT_FILE" 2>/dev/null || echo 0)
N=$((N + 1))
echo "$N" > "$COUNT_FILE"
if [ "$N" -le %d ]; then
  exit 1
fi
exec env SHANNON_DIR=%q %q mcp serve
`, countFile, failUntil, serverStateDir, bin)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write flaky MCP fixture: %v", err)
	}

	const serverName = "flaky-recovering"
	options := isolatedLiveOptions{
		MCPAllowlist: serverName,
		MCPServers: map[string]mcp.MCPServerConfig{
			serverName: {
				Command:               script,
				Context:               "Flaky MCP server used only by the reconnect E2E.",
				KeepAlive:             true,
				ConnectTimeoutSeconds: 20,
				// The credential the production report rotated. Its value is
				// part of the connection config, so changing it is what makes
				// mcpConfigChanged true and takes the rebuild branch.
				Env: map[string]string{"PROBE_TOKEN": "initial-value"},
			},
		},
	}

	daemon := startIsolatedLiveDaemon(t, bin, options)

	// 5s + 10s of backoff, plus two failed spawns and one real handshake.
	waitForMCPState(t, daemon, serverName, "connected", 90*time.Second)

	if got := readSpawnCount(t, countFile); got != failUntil+1 {
		t.Fatalf("expected %d spawns (%d failures + 1 success), got %d\ndaemon log:\n%s",
			failUntil+1, failUntil, got, daemon.output.String())
	}
	// The state transition alone could in principle come from some other
	// retry path, so pin the scheduler as the thing that drove it.
	if log := daemon.output.String(); !strings.Contains(log, "scheduling reconnect attempt 1/") {
		t.Fatalf("recovery did not go through the reconnect scheduler\ndaemon log:\n%s", log)
	}

	// Second half: the production trigger. Rotating the credential rebuilds
	// the manager and respawns the child, and the fresh generation must carry
	// its own working ladder — the superseded one was stopped by the swap.
	if err := os.Remove(countFile); err != nil {
		t.Fatalf("reset spawn counter: %v", err)
	}
	rotated := options.MCPServers[serverName]
	rotated.Env = map[string]string{"PROBE_TOKEN": "rotated-value"}
	options.MCPServers[serverName] = rotated
	writeIsolatedDaemonConfig(t, daemon.stateDir, daemon.cloudEndpoint, options)

	reload := httpPost(t, daemon.baseURL+"/config/reload", map[string]interface{}{})
	if reload["status"] != "reloaded" {
		t.Fatalf("config reload response = %v", reload)
	}

	waitForMCPState(t, daemon, serverName, "connected", 90*time.Second)

	if got := readSpawnCount(t, countFile); got != failUntil+1 {
		t.Fatalf("after credential rotation expected %d spawns, got %d\ndaemon log:\n%s",
			failUntil+1, got, daemon.output.String())
	}
}

func readSpawnCount(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read spawn counter: %v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse spawn counter %q: %v", string(data), err)
	}
	return n
}
