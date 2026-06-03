package tools

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
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

	// Total: 33 local + 2 server = 35. doc-extract pdf/docx/xlsx/pptx are
	// pre-existing local extractors; the +1 in this assertion is
	// schedule_show (added for last-run inspection).
	schemas := reg.Schemas()
	if len(schemas) != 35 {
		t.Errorf("expected 35 tools, got %d", len(schemas))
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
	if len(schemas) != 33 {
		t.Errorf("expected 33 local tools, got %d", len(schemas))
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

	// 33 local + 1 server (bash skipped) = 34. doc-extract pdf/docx/xlsx/pptx
	// are pre-existing local extractors; the +1 in this assertion is
	// schedule_show (added for last-run inspection).
	schemas := reg.Schemas()
	if len(schemas) != 34 {
		t.Errorf("expected 34 tools, got %d", len(schemas))
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

func TestRebuildRegistryForHealth_PlaywrightDegraded(t *testing.T) {
	baseline := agent.NewToolRegistry()
	baseline.Register(&ThinkTool{})
	baseline.Register(&BrowserTool{})

	healthStates := map[string]mcp.ServerHealth{
		"playwright": {State: mcp.StateDegraded},
	}

	mgr := mcp.NewClientManager()
	// CDP + keep_alive=false is the only Degraded config whose tools stay exposed.
	mgr.SeedConfig("playwright", mcp.MCPServerConfig{
		Command: "playwright-mcp",
		Args:    []string{"--cdp-endpoint", "http://127.0.0.1:9223"},
	})
	mgr.SeedToolCache("playwright", []mcp.RemoteTool{
		{ServerName: "playwright", Tool: mcpproto.Tool{Name: "browser_navigate"}},
	})

	reg := RebuildRegistryForHealth(baseline, nil, nil, healthStates, mgr, nil)
	// Degraded (CDP + keep_alive=false steady state after a prior turn's
	// on-demand teardown) keeps the cached Playwright tools exposed so the
	// model can recover the browser on demand — invoking a browser tool
	// relaunches Chrome via mcp_tool.go's pre-call ensureChromeDebugPort.
	// Hiding them here is what forced the turn-start probe relaunch that
	// popped a blank Chrome window on non-browser turns.
	if _, ok := reg.Get("browser_navigate"); !ok {
		t.Error("browser_navigate should be present from cache when degraded (on-demand recovery)")
	}
	// Legacy browser is removed when Playwright tools are present (even degraded).
	if _, ok := reg.Get("browser"); ok {
		t.Error("legacy browser should be removed when Playwright tools are present")
	}
}

// TestRebuildRegistryForHealth_DegradedExposureScope locks in the narrow
// contract: Degraded cached tools are exposed ONLY for Playwright in CDP mode
// with keep_alive=false (the on-demand idle state). Every other Degraded server
// — keep_alive=true playwright, non-CDP playwright, or any other server — stays
// hidden so we never surface broken cached tools or strip the working legacy
// browser fallback.
func TestRebuildRegistryForHealth_DegradedExposureScope(t *testing.T) {
	cdpCfg := mcp.MCPServerConfig{Command: "playwright-mcp", Args: []string{"--cdp-endpoint", "http://127.0.0.1:9223"}}
	cdpKeepAlive := cdpCfg
	cdpKeepAlive.KeepAlive = true
	stdioCfg := mcp.MCPServerConfig{Command: "playwright-mcp", Args: []string{"--headless"}}

	cases := []struct {
		name        string
		server      string
		cfg         mcp.MCPServerConfig
		toolName    string
		wantExposed bool
	}{
		{"playwright cdp keepalive=false exposed", "playwright", cdpCfg, "browser_navigate", true},
		{"playwright cdp keepalive=true hidden", "playwright", cdpKeepAlive, "browser_navigate", false},
		{"playwright non-cdp hidden", "playwright", stdioCfg, "browser_navigate", false},
		{"non-playwright degraded hidden", "other-mcp", mcp.MCPServerConfig{Command: "other-mcp"}, "other_tool", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseline := agent.NewToolRegistry()
			baseline.Register(&ThinkTool{})
			baseline.Register(&BrowserTool{})

			mgr := mcp.NewClientManager()
			mgr.SeedConfig(tc.server, tc.cfg)
			mgr.SeedToolCache(tc.server, []mcp.RemoteTool{
				{ServerName: tc.server, Tool: mcpproto.Tool{Name: tc.toolName}},
			})
			healthStates := map[string]mcp.ServerHealth{tc.server: {State: mcp.StateDegraded}}

			reg := RebuildRegistryForHealth(baseline, nil, nil, healthStates, mgr, nil)
			if _, exposed := reg.Get(tc.toolName); exposed != tc.wantExposed {
				t.Fatalf("%s exposed=%v, want %v", tc.toolName, exposed, tc.wantExposed)
			}
			_, browserPresent := reg.Get("browser")
			if tc.wantExposed {
				if browserPresent && tc.toolName == "browser_navigate" {
					t.Error("legacy browser should be removed when playwright tools are exposed")
				}
			} else if !browserPresent {
				t.Error("legacy browser fallback should remain when degraded server tools are hidden")
			}
		})
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
// 2026-05-14-thinking-blocks-alignment.md Phase E.
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

func TestRegisterLocalTools_CleanupSkipsDeprecatedBrowser(t *testing.T) {
	// Indirect assertion: the cleanup closure returned by RegisterLocalTools
	// closes over the *BrowserTool it registered. Marking that browser
	// deprecated must make cleanup a no-op on the browser side.
	reg, _, cleanup := RegisterLocalTools(nil, nil)
	bt, ok := reg.Get("browser")
	if !ok {
		t.Fatalf("browser tool not registered")
	}
	browser, ok := bt.(*BrowserTool)
	if !ok {
		t.Fatalf("registered browser is not *BrowserTool")
	}
	browser.MarkDeprecated()
	cleanup()
	if got := browser.CleanupCalledForTest(); got != 0 {
		t.Fatalf("deprecated browser must not be cleaned up by registration cleanup; got Cleanup calls=%d", got)
	}
	if !browser.IsDeprecated() {
		t.Fatalf("deprecated flag must persist after cleanup")
	}
}

// TestKocoroSkillAllowedToolsAreRegistered guards the manifest↔registry
// invariant for the bundled kocoro skill: every tool named in its
// allowed-tools must resolve to a real registered local tool. The drift this
// catches is silent and severe — allowed-tools is enforced as execution-time
// denial (loop.go), so a tool the skill's own docs tell the model to call but
// that is missing from the allowlist gets hard-denied with "[skill
// restriction]" at call time. schedule_show shipped as a tool in PR #216 but
// was left out of the allowlist for exactly this reason; nothing failed until
// the model tried to use it.
func TestKocoroSkillAllowedToolsAreRegistered(t *testing.T) {
	// Pin HOME so config.ShannonDir() is deterministic — RegisterLocalTools
	// only registers the schedule_* tools when ShannonDir() != "".
	home := t.TempDir()
	t.Setenv("HOME", home)

	src, err := skills.BundledSkillSource(config.ShannonDir())
	if err != nil {
		t.Fatalf("extract bundled skills: %v", err)
	}
	loaded, err := skills.LoadSkills(src)
	if err != nil {
		t.Fatalf("load bundled skills: %v", err)
	}
	var kocoro *skills.Skill
	for _, s := range loaded {
		if s.Slug == "kocoro" {
			kocoro = s
			break
		}
	}
	if kocoro == nil {
		t.Fatal("bundled kocoro skill not found")
	}
	if len(kocoro.AllowedTools) == 0 {
		t.Fatal("kocoro skill declares no allowed-tools; expected a restrictive allowlist")
	}

	reg, _, cleanup := RegisterLocalTools(nil, nil)
	defer cleanup()

	allowed := make(map[string]bool, len(kocoro.AllowedTools))
	for _, name := range kocoro.AllowedTools {
		allowed[name] = true
	}

	// Direction 1: every allowlist entry resolves to a real registered tool.
	// Catches a dangling allowlist (typo, or a tool that was renamed/removed).
	for _, name := range kocoro.AllowedTools {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("kocoro allowed-tools lists %q, but it is not a registered local tool; "+
				"the skill filter would hard-deny it with [skill restriction] at call time", name)
		}
	}

	// Direction 2 (the schedule_show regression class): every registered
	// schedule_* tool must be in the allowlist. The kocoro skill is THE
	// schedule-management assistant — its docs steer the model to the native
	// schedule_* tools — so a registered schedule tool that is missing from
	// the allowlist is silently undocumented-and-denied. schedule_show shipped
	// in PR #216 registered-but-not-allowlisted and hit exactly this. A future
	// schedule_* tool tripping this assertion is the desired signal: allowlist
	// it (or consciously decide not to and update this test).
	for _, name := range reg.Names() {
		if strings.HasPrefix(name, "schedule_") && !allowed[name] {
			t.Errorf("registered tool %q is not in the kocoro skill allowlist; "+
				"the skill manages schedules natively, so this tool would be hard-denied "+
				"with [skill restriction] when the skill is active", name)
		}
	}
}

func TestRegisterAllWithBaseline_DoesNotSweepOrphans(t *testing.T) {
	// The presence of CleanupOrphanedChromedp() inside RegisterAllWithBaseline
	// makes reload kill live Chrome. We verify the call is gone by using a
	// test-only counter: assert it == 0 after RegisterAllWithBaseline.
	cleanupOrphanedChromedpCalledForTest.Store(0)
	cfg := &config.Config{}
	_, _, _, _, _, _ = RegisterAllWithBaseline(nil, cfg)
	if cleanupOrphanedChromedpCalledForTest.Load() != 0 {
		t.Fatalf("RegisterAllWithBaseline must not invoke CleanupOrphanedChromedp; got %d calls", cleanupOrphanedChromedpCalledForTest.Load())
	}
}
