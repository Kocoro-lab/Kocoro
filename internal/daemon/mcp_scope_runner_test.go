package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
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

func firstStreamingGatewayRequest(t *testing.T, reqs []client.CompletionRequest) client.CompletionRequest {
	t.Helper()
	for _, req := range reqs {
		if req.Stream {
			return req
		}
	}
	t.Fatalf("no streaming main-agent request captured: %+v", reqs)
	return client.CompletionRequest{}
}

// TestRunAgent_NamedAgentMCPScope_ExcludesUnselectedServer is the daemon-path
// integration test for ApplyMCPServerScope. It exercises the real runner.go
// seam (Snapshot baseReg → CloneWithRuntimeConfig → ApplyMCPServerScope →
// NewAgentLoop → tools offered to the gateway) that the pure-function unit test
// can't reach. A named agent scoping MCP to serverA must retain serverA's
// Deferred schema while dropping serverB's tools entirely. BypassRouting
// sidesteps the pre-existing routed-session TempDir cleanup flaky (a teardown
// race, not a body failure).
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
	mainReq := firstStreamingGatewayRequest(t, reqs)
	offered := map[string]client.Tool{}
	for _, tl := range mainReq.Tools {
		offered[offeredToolName(tl)] = tl
	}
	if _, ok := offered["a_tool"]; ok {
		t.Error("cold serverA MCP tool must stay out of the initial tools wire")
	}
	if _, ok := offered["b_tool"]; ok {
		t.Errorf("serverB tool b_tool offered — scope not enforced")
	}
	if _, ok := offered["tool_search"]; !ok {
		t.Fatalf("tool_search missing from scoped Deferred request; offered=%v", offered)
	}
	messageWire, _ := json.Marshal(mainReq.Messages)
	if !strings.Contains(string(messageWire), "a_tool") {
		t.Error("serverA tool is not discoverable through the Deferred summary")
	}
	if strings.Contains(string(messageWire), "b_tool") {
		t.Error("serverB tool leaked into the scoped Deferred summary")
	}
}

// TestIsUnattendedSource pins the attended/unattended split that feeds
// agent.SetUnattendedRun: schedule/cron/heartbeat/watcher/mcp have no human
// at the approval prompt (their handlers auto-approve through the unattended
// deny-list), so a persisted always-allow entry for a deny-listed tool must
// not bypass that gate. Interactive and IM sources stay attended — approvals
// round-trip to a human.
func TestIsUnattendedSource(t *testing.T) {
	for _, source := range []string{"schedule", "cron", "heartbeat", "watcher", "mcp", " Schedule ", "wechat", "wecom", "discord", "telegram", "koe"} {
		if !isUnattendedSource(source) {
			t.Errorf("isUnattendedSource(%q) = false, want true", source)
		}
	}
	for _, source := range []string{"", "local", "tui", "desktop", "ios_remote", "web", "webview", "slack", "line", "feishu", "lark", "teams", "webhook"} {
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
	for _, tc := range []struct {
		name    string
		handler agent.EventHandler
		want    bool
	}{
		{"remote auto-approve", &remoteRunEventHandler{autoApprove: true}, true},
		{"remote broker round-trip", &remoteRunEventHandler{autoApprove: false}, false},
		{"sse auto-approve", &sseEventHandler{autoApprove: true}, true},
		{"sse broker round-trip", &sseEventHandler{autoApprove: false}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUnattendedRun("desktop", tc.handler); got != tc.want {
				t.Fatalf("isUnattendedRun(desktop, %s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// Every inbound surface converges on RunAgent's one per-run registry clone.
// Pin the actual discovery seam so future source-specific routing cannot
// accidentally make GUI execution Desktop-only. Computer schemas are run-
// scoped and must not be advertised before tool_search binds a profile.
func TestRunAgent_AllInboundSourcesDiscoverComputerUse(t *testing.T) {
	reg, _, cleanup := tools.RegisterLocalTools(&config.Config{}, nil)
	defer cleanup()

	for _, source := range []string{
		"local", "tui", "desktop", "ios_remote", "web", "webview", "slack", "line",
		"feishu", "lark", "wecom", "wechat", "teams", "telegram", "discord",
		"koe", "webhook", "schedule", "heartbeat",
	} {
		t.Run(source, func(t *testing.T) {
			gw := &fakeGatewayBackend{reply: "done"}
			completionHandler := gw.handler()
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/completions/resolve" {
					http.Error(
						w,
						"ordinary turns must not resolve computer use",
						http.StatusInternalServerError,
					)
					return
				}
				completionHandler(w, r)
			}))
			defer ts.Close()

			deps := runAgentContractTestDeps(t, ts.URL)
			defer deps.SessionCache.CloseAll()
			deps.Registry = reg
			deps.BaselineReg = reg

			if _, err := RunAgent(context.Background(), deps, RunAgentRequest{
				Text:          "inspect the current app",
				Source:        source,
				BypassRouting: true,
			}, nullEventHandler{}); err != nil {
				t.Fatalf("RunAgent(%s): %v", source, err)
			}

			requests := gw.requests()
			if len(requests) == 0 {
				t.Fatal("no gateway requests captured")
			}
			mainReq := firstStreamingGatewayRequest(t, requests)
			hasToolSearch := false
			for _, tool := range mainReq.Tools {
				switch offeredToolName(tool) {
				case "computer_use", "computer":
					t.Fatalf("cold computer schema leaked for source %q", source)
				case "tool_search":
					hasToolSearch = true
				}
			}
			if !hasToolSearch {
				t.Fatalf("tool_search was not offered for source %q", source)
			}
			messageWire, _ := json.Marshal(mainReq.Messages)
			if !strings.Contains(string(messageWire), "computer_use") {
				t.Fatalf("computer_use was not discoverable for source %q", source)
			}
		})
	}
}
