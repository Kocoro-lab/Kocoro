package daemon

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
	mcpproto "github.com/mark3labs/mcp-go/mcp"
)

func offeredToolName(tl client.Tool) string {
	if tl.Name != "" {
		return tl.Name
	}
	return tl.Function.Name
}

// TestRunAgent_NamedAgentMCPScope_ExcludesUnselectedServer is the daemon-path
// integration test for ApplyMCPServerScope. It exercises the real runner.go
// seam (Snapshot baseReg → CloneWithRuntimeConfig → ApplyMCPServerScope →
// NewAgentLoop → tools offered to the gateway) that the pure-function unit test
// can't reach. A named agent scoping MCP to serverA must have serverB's tools
// dropped from what's offered to the LLM. BypassRouting sidesteps the
// pre-existing routed-session TempDir cleanup flaky (a teardown race, not a
// body failure).
func TestRunAgent_NamedAgentMCPScope_ExcludesUnselectedServer(t *testing.T) {
	gw := &fakeGatewayBackend{reply: "done"}
	ts := httptest.NewServer(gw.handler())
	defer ts.Close()

	deps := runAgentContractTestDeps(t, ts.URL)
	defer deps.SessionCache.CloseAll()

	// Simulate the daemon's global MCP registration. runner.go's Snapshot()
	// returns deps.Registry as the base registry it clones per run, so register
	// both servers' tools there.
	deps.Registry.Register(tools.NewMCPTool("serverA", mcpproto.Tool{Name: "a_tool"}, nil))
	deps.Registry.Register(tools.NewMCPTool("serverB", mcpproto.Tool{Name: "b_tool"}, nil))
	deps.Config.MCPServers = map[string]mcp.MCPServerConfig{"serverA": {}, "serverB": {}}

	// A named agent scoping MCP to serverA only (inherit:false + serverA).
	agentDir := filepath.Join(deps.AgentsDir, "mcptest")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte("# mcptest\nscoped agent"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "config.yaml"),
		[]byte("mcp_servers:\n    _inherit: false\n    serverA: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := RunAgentRequest{
		Text:          "hi",
		Source:        "heartbeat",
		Agent:         "mcptest",
		BypassRouting: true,
	}
	if _, err := RunAgent(context.Background(), deps, req, nullEventHandler{}); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}

	reqs := gw.requests()
	if len(reqs) == 0 {
		t.Fatal("no gateway requests captured")
	}
	offered := map[string]bool{}
	for _, r := range reqs {
		for _, tl := range r.Tools {
			offered[offeredToolName(tl)] = true
		}
	}
	if !offered["a_tool"] {
		t.Errorf("serverA tool a_tool not offered; offered=%v", offered)
	}
	if offered["b_tool"] {
		t.Errorf("serverB tool b_tool offered — scope not enforced")
	}
}
