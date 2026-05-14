package tools

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
	mcpproto "github.com/mark3labs/mcp-go/mcp"
)

func TestRegisterAll_WithServerTools(t *testing.T) {
	serverTools := []client.ServerToolSchema{
		{Name: "web_search", Description: "Search the web"},
		{Name: "getStockBars", Description: "Get stock price bars"},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(serverTools)
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg, _, _, cleanup, err := RegisterAll(gw, nil)
	defer cleanup()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check local tools are registered
	for _, name := range []string{"use_skill", "file_read", "file_write", "file_edit", "glob", "grep", "bash", "think", "directory_list", "http", "system_info", "clipboard", "notify", "process", "applescript", "accessibility", "ghostty", "browser", "screenshot", "computer", "wait_for", "schedule_create", "schedule_list", "schedule_update", "schedule_remove", "archive_inspect", "archive_extract", "pdf_to_text", "docx_to_text", "xlsx_to_text", "pptx_to_text"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("local tool %q not registered", name)
		}
	}

	// Check server tools are registered
	for _, name := range []string{"web_search", "getStockBars"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("server tool %q not registered", name)
		}
	}

	// Total: 32 local + 2 server = 34 (4 doc-extract tools added Phase 2)
	schemas := reg.Schemas()
	if len(schemas) != 34 {
		t.Errorf("expected 34 tools, got %d", len(schemas))
	}
}

func TestRegisterAll_ServerUnavailable(t *testing.T) {
	// Point to a closed server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg, _, _, cleanup, err := RegisterAll(gw, nil)
	defer cleanup()
	if err == nil {
		t.Error("expected warning error when server is unavailable")
	}

	// Local tools should still be registered
	for _, name := range []string{"file_read", "bash", "glob"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("local tool %q should still be registered", name)
		}
	}

	schemas := reg.Schemas()
	if len(schemas) != 32 {
		t.Errorf("expected 32 local tools, got %d", len(schemas))
	}
}

func TestRegisterAll_LocalPriority(t *testing.T) {
	// Server returns a tool named "bash" — should be skipped
	serverTools := []client.ServerToolSchema{
		{Name: "bash", Description: "Server bash (should be skipped)"},
		{Name: "web_search", Description: "Search the web"},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(serverTools)
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg, _, _, cleanup, err := RegisterAll(gw, nil)
	defer cleanup()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// "bash" should be the local BashTool, not the server one
	tool, ok := reg.Get("bash")
	if !ok {
		t.Fatal("bash tool not found")
	}
	if _, isServer := tool.(*ServerTool); isServer {
		t.Error("bash should be local tool, not server tool")
	}

	// web_search should be server tool
	tool, ok = reg.Get("web_search")
	if !ok {
		t.Fatal("web_search tool not found")
	}
	if _, isServer := tool.(*ServerTool); !isServer {
		t.Error("web_search should be a server tool")
	}

	// 32 local + 1 server (bash skipped) = 33 (4 doc-extract tools added Phase 2)
	schemas := reg.Schemas()
	if len(schemas) != 33 {
		t.Errorf("expected 33 tools, got %d", len(schemas))
	}
}

func TestRegisterServerTools_AllowlistFiltering(t *testing.T) {
	serverTools := []client.ServerToolSchema{
		{Name: "web_search", Description: "Search the web"},
		{Name: "python_executor", Description: "Run Python in sandbox"},
		{Name: "calculator", Description: "Basic calculator"},
		{Name: "getStockBars", Description: "Get stock price bars"},
		{Name: "session_file_write", Description: "Write session file"},
		{Name: "some_future_tool", Description: "Unknown new tool"},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(serverTools)
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg, _, _, cleanup, err := RegisterAll(gw, nil)
	defer cleanup()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Allowlisted tools should be registered
	for _, name := range []string{"web_search", "getStockBars"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("allowlisted tool %q should be registered", name)
		}
	}

	// Non-allowlisted tools should be filtered out
	for _, name := range []string{"python_executor", "calculator", "session_file_write", "some_future_tool"} {
		if _, ok := reg.Get(name); ok {
			t.Errorf("non-allowlisted tool %q should NOT be registered", name)
		}
	}
}

func TestExtractGatewayTools(t *testing.T) {
	reg := agent.NewToolRegistry()
	gw := client.NewGatewayClient("http://test", "key")
	reg.Register(NewServerTool(client.ServerToolSchema{Name: "web_search", Description: "search"}, gw))
	reg.Register(&ThinkTool{})
	tools := ExtractGatewayTools(reg)
	if len(tools) != 1 {
		t.Fatalf("expected 1 gateway tool, got %d", len(tools))
	}
	if tools[0].Info().Name != "web_search" {
		t.Errorf("expected web_search, got %s", tools[0].Info().Name)
	}
}

func TestExtractPostOverlays(t *testing.T) {
	baseline := agent.NewToolRegistry()
	baseline.Register(&ThinkTool{})

	full := baseline.Clone()
	gw := client.NewGatewayClient("http://test", "key")
	full.Register(NewServerTool(client.ServerToolSchema{Name: "web_search", Description: "search"}, gw))
	mgr := mcp.NewClientManager()
	full.Register(NewMCPTool("playwright", mcpproto.Tool{Name: "browser_navigate"}, mgr))
	full.Register(&NotifyTool{}) // a local overlay

	overlays := ExtractPostOverlays(full, baseline)
	if len(overlays) != 1 {
		t.Fatalf("expected 1 overlay, got %d", len(overlays))
	}
	if overlays[0].Info().Name != "notify" {
		t.Errorf("expected notify, got %s", overlays[0].Info().Name)
	}
}

func TestRebuildRegistryForHealth_PlaywrightHealthy(t *testing.T) {
	baseline := agent.NewToolRegistry()
	baseline.Register(&ThinkTool{})
	baseline.Register(&BrowserTool{})

	healthStates := map[string]mcp.ServerHealth{
		"playwright": {State: mcp.StateHealthy},
	}

	mgr := mcp.NewClientManager()
	mgr.SeedToolCache("playwright", []mcp.RemoteTool{
		{ServerName: "playwright", Tool: mcpproto.Tool{Name: "browser_navigate"}},
	})

	reg := RebuildRegistryForHealth(baseline, nil, nil, healthStates, mgr, nil)
	if _, ok := reg.Get("browser"); ok {
		t.Error("legacy browser should be removed when Playwright is healthy")
	}
	if _, ok := reg.Get("browser_navigate"); !ok {
		t.Error("browser_navigate should be registered from healthy Playwright")
	}
}

func TestRebuildRegistryForHealth_PlaywrightDisconnected(t *testing.T) {
	baseline := agent.NewToolRegistry()
	baseline.Register(&ThinkTool{})
	baseline.Register(&BrowserTool{})

	healthStates := map[string]mcp.ServerHealth{
		"playwright": {State: mcp.StateDisconnected},
	}

	mgr := mcp.NewClientManager()
	mgr.SeedToolCache("playwright", []mcp.RemoteTool{
		{ServerName: "playwright", Tool: mcpproto.Tool{Name: "browser_navigate"}},
	})

	reg := RebuildRegistryForHealth(baseline, nil, nil, healthStates, mgr, nil)
	// Disconnected Playwright tools are included from cache for on-demand reconnect.
	if _, ok := reg.Get("browser_navigate"); !ok {
		t.Error("browser_navigate should be present from cache even when disconnected")
	}
	// Legacy browser is removed when Playwright tools are present (even disconnected).
	if _, ok := reg.Get("browser"); ok {
		t.Error("legacy browser should be removed when Playwright tools are present")
	}
}

func TestRebuildRegistryForHealth_GatewayAndPostOverlays(t *testing.T) {
	baseline := agent.NewToolRegistry()
	baseline.Register(&ThinkTool{})

	gw := client.NewGatewayClient("http://test", "key")
	gatewayOverlay := []agent.Tool{
		NewServerTool(client.ServerToolSchema{Name: "web_search", Description: "search"}, gw),
	}
	postOverlays := []agent.Tool{
		&NotifyTool{},
	}

	reg := RebuildRegistryForHealth(baseline, gatewayOverlay, postOverlays, nil, nil, nil)
	if _, ok := reg.Get("think"); !ok {
		t.Error("baseline tool 'think' should be present")
	}
	if _, ok := reg.Get("web_search"); !ok {
		t.Error("gateway overlay 'web_search' should be present")
	}
	if _, ok := reg.Get("notify"); !ok {
		t.Error("post overlay 'notify' should be present")
	}
}

// TestShouldRegisterThinkTool covers the gating predicate that decides whether
// the local `think` tool is registered. See plan
// 2026-05-14-thinking-blocks-cc-alignment.md Phase E.
func TestShouldRegisterThinkTool(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*config.Config)
		want bool
	}{
		{"nil cfg → register (fail-open)", nil, true},
		{"gateway + thinking=true → skip", func(c *config.Config) {
			c.Agent.Thinking = true
		}, false},
		{"gateway + thinking=false → register (no native)", func(c *config.Config) {
			c.Agent.Thinking = false
		}, true},
		{"ollama + thinking=true → register (no native on ollama)", func(c *config.Config) {
			c.Provider = "ollama"
			c.Agent.Thinking = true
		}, true},
		{"force_think_tool overrides gateway+thinking", func(c *config.Config) {
			c.Agent.Thinking = true
			c.Agent.ForceThinkTool = true
		}, true},
		{"force_think_tool=false honors default skip", func(c *config.Config) {
			c.Agent.Thinking = true
			c.Agent.ForceThinkTool = false
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg *config.Config
			if tc.mod != nil {
				cfg = &config.Config{}
				tc.mod(cfg)
			}
			got := shouldRegisterThinkTool(cfg)
			if got != tc.want {
				t.Errorf("shouldRegisterThinkTool = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRegisterLocalTools_HidesThinkUnderDefaultGateway(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agent.Thinking = true
	reg, _, cleanup := RegisterLocalTools(cfg, nil)
	defer cleanup()
	if _, ok := reg.Get("think"); ok {
		t.Errorf("think tool must not be registered under gateway+thinking=true; got names %v", reg.Names())
	}
}

func TestRegisterLocalTools_KeepsThinkWhenThinkingDisabled(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agent.Thinking = false
	reg, _, cleanup := RegisterLocalTools(cfg, nil)
	defer cleanup()
	if _, ok := reg.Get("think"); !ok {
		t.Errorf("think tool must be registered when thinking=false; got names %v", reg.Names())
	}
}

func TestRegisterLocalTools_KeepsThinkUnderOllama(t *testing.T) {
	cfg := &config.Config{}
	cfg.Provider = "ollama"
	cfg.Agent.Thinking = true
	reg, _, cleanup := RegisterLocalTools(cfg, nil)
	defer cleanup()
	if _, ok := reg.Get("think"); !ok {
		t.Errorf("think tool must be registered on Ollama; got names %v", reg.Names())
	}
}

func TestRegisterLocalTools_ForceThinkToolOverride(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agent.Thinking = true
	cfg.Agent.ForceThinkTool = true
	reg, _, cleanup := RegisterLocalTools(cfg, nil)
	defer cleanup()
	if _, ok := reg.Get("think"); !ok {
		t.Errorf("ForceThinkTool=true must re-register think; got names %v", reg.Names())
	}
}
