package daemon

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
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

// TestIsUnattendedSource pins the attended/unattended split that feeds
// agent.SetUnattendedRun: schedule/cron/heartbeat/watcher/mcp have no human
// at the approval prompt (their handlers auto-approve through the unattended
// deny-list), so a persisted always-allow entry for a deny-listed tool must
// not bypass that gate. Interactive and IM sources stay attended — approvals
// round-trip to a human.
func TestIsUnattendedSource(t *testing.T) {
	for _, source := range []string{"schedule", "cron", "heartbeat", "watcher", "mcp", " Schedule "} {
		if !isUnattendedSource(source) {
			t.Errorf("isUnattendedSource(%q) = false, want true", source)
		}
	}
	for _, source := range []string{"", "local", "tui", "desktop", "ios_remote", "web", "webview", "slack", "line", "feishu", "telegram", "webhook", "koe"} {
		if isUnattendedSource(source) {
			t.Errorf("isUnattendedSource(%q) = true, want false", source)
		}
	}
}

func TestIsUnattendedRun_IncludesTransportWithoutApprovalRoundTrip(t *testing.T) {
	if !isUnattendedRun("kocoro", &httpEventHandler{}) {
		t.Fatal("synchronous HTTP must be unattended even with an attended-looking source")
	}
	if isUnattendedRun("desktop", nullEventHandler{}) {
		t.Fatal("interactive handler/source was classified as unattended")
	}
	if !isUnattendedRun("schedule", nullEventHandler{}) {
		t.Fatal("unattended source must remain sufficient regardless of handler")
	}
}

// Every inbound surface converges on RunAgent's one per-run registry clone.
// Pin the actual offered-tool seam so future source-specific routing cannot
// accidentally make GUI execution Desktop-only.
func TestRunAgent_AllInboundSourcesOfferComputerUse(t *testing.T) {
	reg, _, cleanup := tools.RegisterLocalTools(&config.Config{}, nil)
	defer cleanup()

	for _, source := range []string{
		"local", "tui", "desktop", "ios_remote", "web", "webview", "slack", "line",
		"feishu", "lark", "wecom", "wechat", "teams", "telegram", "discord",
		"koe", "webhook", "schedule", "heartbeat",
	} {
		t.Run(source, func(t *testing.T) {
			gw := &fakeGatewayBackend{reply: "done"}
			ts := httptest.NewServer(gw.handler())
			defer ts.Close()

			deps := runAgentContractTestDeps(t, ts.URL)
			defer deps.SessionCache.CloseAll()
			// Tool-reference-capable model keeps deferred schemas on the wire
			// with defer_loading=true, so this test can inspect the exact offered
			// registry rather than only the tool_search discovery summary.
			deps.Config.Agent.Model = "claude-sonnet-4-5-20250929"
			deps.Registry = reg
			deps.BaselineReg = reg

			if _, err := RunAgent(context.Background(), deps, RunAgentRequest{
				Text:          "inspect the current app",
				Source:        source,
				BypassRouting: true,
			}, nullEventHandler{}); err != nil {
				t.Fatalf("RunAgent(%s): %v", source, err)
			}

			offered := false
			for _, request := range gw.requests() {
				for _, tool := range request.Tools {
					if offeredToolName(tool) == "computer_use" {
						offered = true
					}
				}
			}
			if !offered {
				t.Fatalf("computer_use was not offered for source %q", source)
			}
		})
	}
}
