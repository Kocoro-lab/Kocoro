package tools

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/images"
	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
	"github.com/Kocoro-lab/ShanClaw/internal/schedule"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
	"github.com/Kocoro-lab/ShanClaw/internal/uploads"
)

// shouldRegisterThinkTool reports whether the local `think` tool should be
// added to the registry. Skipped by default on the gateway path with native
// thinking enabled — the two signals are redundant on Sonnet 4.6 / Opus 4.7
// adaptive mode.
// Kept on:
//   - Ollama (`cfg.Provider == "ollama"`) — OpenAI-shape API has no native thinking.
//   - Thinking disabled by user (`cfg.Agent.Thinking == false`) — no native fallback.
//   - Explicit escape hatch (`cfg.Agent.ForceThinkTool == true`).
//
// See plan 2026-05-14-thinking-blocks-alignment.md Phase E for the wider
// rationale (the ritual `think({})` empty-input emissions surfaced as a
// 14-minute production hang before Phase 0's bottom guards landed).
func shouldRegisterThinkTool(cfg *config.Config) bool {
	if cfg == nil {
		return true
	}
	if cfg.Agent.ForceThinkTool {
		return true
	}
	if cfg.Provider == "ollama" {
		return true
	}
	if !cfg.Agent.Thinking {
		return true
	}
	return false
}

// RegisterLocalTools registers only the local tools.
// If cfg is non-nil, extra safe commands from permissions.allowed_commands
// are passed to the BashTool so they skip approval.
// Returns the registry and a cleanup function that shuts down any active
// tool resources (e.g. browser process).
func RegisterLocalTools(cfg *config.Config, secretsStore *skills.SecretsStore) (*agent.ToolRegistry, *[]*skills.Skill, func()) {
	reg := agent.NewToolRegistry()

	skillsPtr := &[]*skills.Skill{}
	reg.Register(newUseSkillTool(skillsPtr))

	reg.Register(&FileReadTool{})
	reg.Register(&FileWriteTool{})
	reg.Register(&FileEditTool{})
	reg.Register(&GlobTool{})
	reg.Register(&GrepTool{})

	bashTool := &BashTool{SecretsStore: secretsStore}
	if cfg != nil {
		bashTool.ExtraSafeCommands = cfg.Permissions.AllowedCommands
		if cfg.Tools.BashMaxOutput > 0 {
			bashTool.MaxOutput = cfg.Tools.BashMaxOutput
		}
		if cfg.Tools.BashTimeout > 0 {
			bashTool.DefaultTimeoutSecs = cfg.Tools.BashTimeout
		}
		if cfg.Tools.BashMaxTimeout > 0 {
			bashTool.MaxTimeoutSecs = cfg.Tools.BashMaxTimeout
		}
		bashTool.ConcurrencyEnabled = cfg.Agent.BashConcurrencyEnabled
	}
	reg.Register(bashTool)

	reg.Register(&MemoryAppendTool{})
	if shouldRegisterThinkTool(cfg) {
		reg.Register(&ThinkTool{})
	}
	reg.Register(&DirectoryListTool{})
	reg.Register(&ArchiveInspectTool{})
	reg.Register(&ArchiveExtractTool{})
	reg.Register(&PDFToTextTool{})
	reg.Register(&DocxToTextTool{})
	reg.Register(&XlsxToTextTool{})
	reg.Register(&PptxToTextTool{})
	reg.Register(&HTTPTool{})
	reg.Register(&SystemInfoTool{})
	reg.Register(&ClipboardTool{})
	reg.Register(&NotifyTool{})
	reg.Register(&ProcessTool{})
	reg.Register(&AppleScriptTool{})
	axClient := SharedAXClient()
	reg.Register(&AccessibilityTool{client: axClient})
	reg.Register(&GhosttyTool{tabs: newTabRegistry()})

	browser := &BrowserTool{}
	reg.Register(browser)
	reg.Register(&ScreenshotTool{})
	reg.Register(&ComputerTool{client: axClient})
	reg.Register(&WaitTool{client: axClient})

	// Schedule tools (direct access for TUI/one-shot where daemon API is unavailable).
	// In daemon mode, kocoro skill routes schedule operations through the HTTP API
	// for audit logging and confirm gates; these tools serve as fallback.
	if shanDir := config.ShannonDir(); shanDir != "" {
		schMgr := schedule.NewManager(filepath.Join(shanDir, "schedules.json"))
		for _, tool := range NewScheduleTools(schMgr, shanDir) {
			reg.Register(tool)
		}
	}

	cleanup := func() {
		if !browser.IsDeprecated() {
			browser.Cleanup()
		}
		axClient.Close()
	}
	return reg, skillsPtr, cleanup
}

// CloneWithRuntimeConfig returns a registry clone with session-scoped local tool
// settings applied. Tools with per-run mutable state (BashTool, CloudDelegateTool)
// are deep-copied so concurrent routes don't share mutable fields.
func CloneWithRuntimeConfig(reg *agent.ToolRegistry, cfg *config.Config) *agent.ToolRegistry {
	if reg == nil {
		return nil
	}

	cloned := reg.Clone()

	// Deep-copy BashTool with session-scoped settings.
	if bashTool, ok := cloned.Get("bash"); ok {
		if existing, ok := bashTool.(*BashTool); ok {
			bashCopy := *existing
			if cfg != nil {
				bashCopy.ExtraSafeCommands = append([]string(nil), cfg.Permissions.AllowedCommands...)
				if cfg.Tools.BashMaxOutput > 0 {
					bashCopy.MaxOutput = cfg.Tools.BashMaxOutput
				} else {
					bashCopy.MaxOutput = 0
				}
				if cfg.Tools.BashTimeout > 0 {
					bashCopy.DefaultTimeoutSecs = cfg.Tools.BashTimeout
				} else {
					bashCopy.DefaultTimeoutSecs = 0
				}
				if cfg.Tools.BashMaxTimeout > 0 {
					bashCopy.MaxTimeoutSecs = cfg.Tools.BashMaxTimeout
				} else {
					bashCopy.MaxTimeoutSecs = 0
				}
				bashCopy.ConcurrencyEnabled = cfg.Agent.BashConcurrencyEnabled
			}
			cloned.Register(&bashCopy)
		}
	}

	// Deep-copy CloudDelegateTool so per-run handler/agent context
	// mutations don't race across concurrent daemon routes.
	if cdTool, ok := cloned.Get("cloud_delegate"); ok {
		if existing, ok := cdTool.(*CloudDelegateTool); ok {
			cdCopy := *existing
			cloned.Register(&cdCopy)
		}
	}

	return cloned
}

// gatewayAllowedTools is the allowlist of server-side tools worth registering
// locally. Cloud-only tools (python_executor, calculator, etc.) are excluded
// to prevent the LLM from choosing them over better local equivalents.
// All cloud tools remain available via cloud_delegate.
var gatewayAllowedTools = map[string]bool{
	// Research
	"web_search":        true,
	"web_fetch":         true,
	"web_subpage_fetch": true,
	"web_crawl":         true,
	"x_search":          true,
	// Financial
	"getStockBars":      true,
	"alpaca_news":       true,
	"sec_filings":       true,
	"news_aggregator":   true,
	"twitter_sentiment": true,
	// Ads/Enterprise
	"ads_serp_extract":        true,
	"ads_transparency_search": true,
	"ads_competitor_discover": true,
	"lp_visual_analyze":       true,
	"lp_batch_analyze":        true,
	"ads_creative_analyze":    true,
	"yahoo_jp_ads_discover":   true,
	"meta_ad_library_search":  true,
	// Analytics
	"ga4_run_report":          true,
	"ga4_run_realtime_report": true,
	"ga4_get_metadata":        true,
	// Visual
	"page_screenshot": true,
}

// RegisterServerTools fetches server-side tools from the gateway and appends
// entries to the provided registry. Only allowlisted tools are registered;
// others are skipped (still available via cloud_delegate). Local tools always
// keep priority.
func RegisterServerTools(ctx context.Context, gw *client.GatewayClient, reg *agent.ToolRegistry) error {
	if reg == nil {
		return fmt.Errorf("tool registry is nil")
	}

	schemas, err := gw.ListTools(ctx)
	if err != nil {
		return fmt.Errorf("server tools unavailable: %w", err)
	}

	for _, schema := range schemas {
		if _, exists := reg.Get(schema.Name); exists {
			continue // local tool takes priority
		}
		if !gatewayAllowedTools[schema.Name] {
			continue // not allowlisted; available via cloud_delegate
		}
		reg.Register(NewServerTool(schema, gw))
	}

	return nil
}

// SetRegistrySkills updates the use_skill tool in a registry to point to the
// given skills slice. Returns the skills pointer for the caller to keep in sync.
// This is safe for concurrent use because it creates a new use_skill tool instance.
func SetRegistrySkills(reg *agent.ToolRegistry, s []*skills.Skill) {
	reg.Register(newUseSkillTool(&s))
}

// ApplyToolFilter applies the agent's tool allow/deny filter to a registry.
// Returns a new filtered registry, or the original if no filter applies.
func ApplyToolFilter(reg *agent.ToolRegistry, agentDef ...*agents.Agent) *agent.ToolRegistry {
	if len(agentDef) == 0 || agentDef[0] == nil || agentDef[0].Config == nil || agentDef[0].Config.Tools == nil {
		return reg
	}
	f := agentDef[0].Config.Tools
	if len(f.Allow) > 0 {
		return reg.FilterByAllow(f.Allow)
	}
	if len(f.Deny) > 0 {
		return reg.FilterByDeny(f.Deny)
	}
	return reg
}

// CompleteRegistration connects MCP servers and gateway tools on top of a base
// local-only registry, then applies per-agent tool filtering. The filter runs
// AFTER all tool sources are registered so it applies to MCP and gateway tools too.
// The returned cleanup function closes MCP connections.
func CompleteRegistration(ctx context.Context, gw *client.GatewayClient, cfg *config.Config, baseReg *agent.ToolRegistry, agentDef ...*agents.Agent) (*agent.ToolRegistry, *mcp.ClientManager, func(), error) {
	reg := baseReg.Clone()

	mcpServers := resolveMCPServers(cfg, agentDef...)

	// CDP mode: only launch Chrome at boot when keepAlive is true (eager mode).
	// playwright-mcp can discover tools without Chrome running, so keepAlive=false
	// skips Chrome entirely — it launches on-demand at first tool invocation.
	if pwCfg, hasPW := mcpServers["playwright"]; hasPW && !pwCfg.Disabled && mcp.IsPlaywrightCDPMode(pwCfg) {
		if pwCfg.KeepAlive {
			if err := mcp.EnsureChromeDebugPort(mcp.PlaywrightCDPPort(pwCfg)); err != nil {
				log.Printf("Playwright CDP: Chrome debug port unavailable: %v — skipping", err)
				delete(mcpServers, "playwright")
			}
		}
	}

	var mcpMgr *mcp.ClientManager
	if len(mcpServers) > 0 {
		mcpMgr = mcp.NewClientManager()
		// Advertise workspace roots to servers that honor the MCP `roots`
		// capability (playwright-mcp restricts browser_file_upload to
		// declared roots). Must be installed before ConnectAll so the
		// initialize handshake carries the client capability flag.
		rootCandidates := mcp.DefaultWorkspaceRootCandidates(config.ShannonDir())
		rootCandidates = append(rootCandidates, cfg.MCP.WorkspaceRoots...)
		mcpMgr.SetRootsHandler(mcp.NewRootsHandler(rootCandidates))
		mcpTools, mcpErr := mcpMgr.ConnectAll(ctx, mcpServers)
		if mcpErr != nil {
			log.Printf("MCP connection warning: %v", mcpErr)
		}
		hasPlaywright := false
		for _, t := range mcpTools {
			if _, exists := reg.Get(t.Tool.Name); exists {
				continue
			}
			reg.Register(NewMCPTool(t.ServerName, t.Tool, mcpMgr))
			if t.Tool.Name == "browser_navigate" {
				hasPlaywright = true
			}
		}
		// Disable legacy browser/automation tools when Playwright MCP is available.
		// AppleScript, accessibility, and screenshot are macOS-native fallbacks that
		// the LLM picks when playwright tools hit errors — remove them so the agent
		// stays on playwright for all browser automation.
		if hasPlaywright {
			// Shut down any chromedp Chrome instance before removing the tool
			if bt, ok := reg.Get("browser"); ok {
				if browserTool, ok := bt.(*BrowserTool); ok {
					browserTool.Cleanup()
				}
			}
			for _, legacy := range []string{"browser", "applescript", "accessibility", "screenshot", "wait_for"} {
				reg.Remove(legacy)
			}
			log.Printf("Playwright MCP connected — disabled legacy browser/automation tools")
			// When keepAlive is false, disconnect playwright after tool discovery.
			// It will reconnect on-demand at first tool invocation.
			// When keepAlive is true, keep the connection alive to avoid latency.
			if cfg, ok := mcpMgr.ConfigFor("playwright"); ok && !cfg.KeepAlive {
				mcpMgr.Disconnect("playwright")
				log.Printf("Playwright MCP disconnected — will reconnect on demand")
			}
		}
	}

	var err error
	if gw != nil {
		err = RegisterServerTools(ctx, gw, reg)
	}

	// Apply tool filter AFTER all sources are registered
	reg = ApplyToolFilter(reg, agentDef...)

	cleanup := func() {
		if mcpMgr != nil {
			mcpMgr.Close()
		}
	}

	return reg, mcpMgr, cleanup, err
}

// RegisterAllWithBaseline is like RegisterAll but also returns the baseline (local-only)
// registry separately, for use by the MCP health supervisor's registry rebuild.
func RegisterAllWithBaseline(gw *client.GatewayClient, cfg *config.Config, agentDef ...*agents.Agent) (
	baseline *agent.ToolRegistry,
	reg *agent.ToolRegistry,
	skillsPtr *[]*skills.Skill,
	mcpMgr *mcp.ClientManager,
	cleanup func(),
	err error,
) {
	localReg, sp, baseCleanup := RegisterLocalTools(cfg, nil)
	baseline = localReg

	// 45s allows time for Chrome CDP launch (up to 15s) + MCP handshake.
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	reg, mcpMgr, remoteCleanup, err := CompleteRegistration(ctx, gw, cfg, localReg, agentDef...)

	cleanup = func() {
		baseCleanup()
		remoteCleanup()
	}

	return baseline, reg, sp, mcpMgr, cleanup, err
}

// StartMCPFunc kicks off the deferred MCP connection goroutines for an
// async registration. It blocks only long enough to register every server's
// config under one lock; each individual connect runs in its own goroutine
// with a per-server timeout (MCPServerConfig.ConnectTimeoutSeconds or
// cfg.MCP.DefaultConnectTimeoutSecs). onResult fires once per non-disabled
// server with its outcome.
type StartMCPFunc func(parentCtx context.Context, onResult func(serverName string, err error))

// CompleteRegistrationAsync builds the registry like CompleteRegistration but
// does NOT block on MCP server connects. Instead it returns a StartMCPFunc
// closure the caller invokes after wiring the supervisor; the initial
// registry contains only local + gateway tools, and MCP tools fill in as
// each server's background connect succeeds (via supervisor OnChange →
// RebuildRegistryForHealth). Callers that need the full sync flow (TUI,
// one-shot CLI) keep using CompleteRegistration.
func CompleteRegistrationAsync(ctx context.Context, gw *client.GatewayClient, cfg *config.Config, baseReg *agent.ToolRegistry, agentDef ...*agents.Agent) (*agent.ToolRegistry, *mcp.ClientManager, StartMCPFunc, func(), error) {
	reg := baseReg.Clone()

	mcpServers := resolveMCPServers(cfg, agentDef...)

	// CDP mode: same eager-launch gate as the sync path. Chrome must be
	// reachable before the playwright connect attempt fires; if not we drop
	// the server from the set so the supervisor doesn't keep retrying.
	if pwCfg, hasPW := mcpServers["playwright"]; hasPW && !pwCfg.Disabled && mcp.IsPlaywrightCDPMode(pwCfg) {
		if pwCfg.KeepAlive {
			if err := mcp.EnsureChromeDebugPort(mcp.PlaywrightCDPPort(pwCfg)); err != nil {
				log.Printf("Playwright CDP: Chrome debug port unavailable: %v — skipping", err)
				delete(mcpServers, "playwright")
			}
		}
	}

	var mcpMgr *mcp.ClientManager
	if len(mcpServers) > 0 {
		mcpMgr = mcp.NewClientManager()
		rootCandidates := mcp.DefaultWorkspaceRootCandidates(config.ShannonDir())
		rootCandidates = append(rootCandidates, cfg.MCP.WorkspaceRoots...)
		mcpMgr.SetRootsHandler(mcp.NewRootsHandler(rootCandidates))
		// Pre-register all configs so supervisor.Start (called by daemon
		// between this return and StartMCPFunc invocation) sees the full
		// server set and creates per-server probe entries.
		mcpMgr.RegisterConfigs(mcpServers)
	}

	var err error
	if gw != nil {
		err = RegisterServerTools(ctx, gw, reg)
	}

	reg = ApplyToolFilter(reg, agentDef...)

	cleanup := func() {
		if mcpMgr != nil {
			mcpMgr.Close()
		}
	}

	startMCP := StartMCPFunc(func(parentCtx context.Context, onResult func(serverName string, err error)) {
		if mcpMgr == nil || len(mcpServers) == 0 {
			return
		}
		defaultTimeout := time.Duration(cfg.MCP.DefaultConnectTimeoutSecs) * time.Second
		if defaultTimeout <= 0 {
			defaultTimeout = 60 * time.Second
		}
		mcpMgr.StartConnectAll(parentCtx, mcpServers, defaultTimeout, onResult)
	})

	return reg, mcpMgr, startMCP, cleanup, err
}

// RegisterAllWithBaselineAsync mirrors RegisterAllWithBaseline but defers
// MCP connects: the returned startMCP closure runs them in the background
// once the caller has stood up the supervisor and atomically swapped the
// new deps into place. HTTP /config/reload and daemon startup are no
// longer blocked by slow MCP handshakes (Intercom OAuth can be 30-180s).
func RegisterAllWithBaselineAsync(gw *client.GatewayClient, cfg *config.Config, agentDef ...*agents.Agent) (
	baseline *agent.ToolRegistry,
	reg *agent.ToolRegistry,
	skillsPtr *[]*skills.Skill,
	mcpMgr *mcp.ClientManager,
	cleanup func(),
	startMCP StartMCPFunc,
	err error,
) {
	localReg, sp, baseCleanup := RegisterLocalTools(cfg, nil)
	baseline = localReg

	// Shorter ctx than the sync variant — no MCP connect inside, so we just
	// need to cover the gateway tools/list round-trip.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var remoteCleanup func()
	reg, mcpMgr, startMCP, remoteCleanup, err = CompleteRegistrationAsync(ctx, gw, cfg, localReg, agentDef...)

	cleanup = func() {
		baseCleanup()
		remoteCleanup()
	}

	return baseline, reg, sp, mcpMgr, cleanup, startMCP, err
}

// RegisterAll registers local tools, connects MCP servers, and then fetches
// server-side tools from the gateway. Local tools take priority, then MCP, then gateway.
// If agentDef is non-nil, tool filtering and MCP scoping are applied per-agent.
// The returned cleanup function must be called on shutdown.
func RegisterAll(gw *client.GatewayClient, cfg *config.Config, agentDef ...*agents.Agent) (*agent.ToolRegistry, *[]*skills.Skill, *mcp.ClientManager, func(), error) {
	reg, skillsPtr, baseCleanup := RegisterLocalTools(cfg, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	reg, mcpMgr, remoteCleanup, err := CompleteRegistration(ctx, gw, cfg, reg, agentDef...)

	cleanup := func() {
		baseCleanup()
		remoteCleanup()
	}

	return reg, skillsPtr, mcpMgr, cleanup, err
}

// resolveMCPServers determines which MCP servers to connect based on agent config.
// If the agent has no MCP config, returns the global set.
// If _inherit is true, agent servers are merged on top of global.
// If _inherit is false, only agent servers are used.
func resolveMCPServers(cfg *config.Config, agentDef ...*agents.Agent) map[string]mcp.MCPServerConfig {
	if cfg == nil {
		return nil
	}

	// No agent or no agent MCP config → return a copy of the global set.
	// Must be a copy: CompleteRegistration calls delete() on the returned map to
	// gate servers (e.g. playwright without readiness marker), and mutating
	// cfg.MCPServers directly would corrupt the live config seen by Snapshot().
	if len(agentDef) == 0 || agentDef[0] == nil || agentDef[0].Config == nil || agentDef[0].Config.MCPServers == nil {
		result := make(map[string]mcp.MCPServerConfig, len(cfg.MCPServers))
		for name, srv := range cfg.MCPServers {
			if name == "playwright" {
				srv = mcp.NormalizePlaywrightCDPConfig(srv)
			}
			result[name] = srv
		}
		return result
	}

	agentMCP := agentDef[0].Config.MCPServers
	result := make(map[string]mcp.MCPServerConfig)

	// If inherit, start with global servers
	if agentMCP.Inherit {
		for name, srv := range cfg.MCPServers {
			if name == "playwright" {
				srv = mcp.NormalizePlaywrightCDPConfig(srv)
			}
			result[name] = srv
		}
	}

	// Overlay agent-specific servers
	for name, ref := range agentMCP.Servers {
		srv := mcp.MCPServerConfig{
			Command:   ref.Command,
			Args:      ref.Args,
			Env:       ref.Env,
			Type:      ref.Type,
			URL:       ref.URL,
			Disabled:  ref.Disabled,
			Context:   ref.Context,
			KeepAlive: ref.KeepAlive,
		}
		if name == "playwright" {
			srv = mcp.NormalizePlaywrightCDPConfig(srv)
		}
		result[name] = srv
	}

	return result
}

// ShouldSkipReloadRetry mirrors the PostConnectDisconnectIfDiscoveryOnly
// predicate so /config/reload's "retry disconnected enabled servers" pass
// (daemon/server.go retryDisconnectedEnabledMCPServers) doesn't undo the
// discover-then-disconnect optimization on every reload.
//
// Without this check the loop is:
//   1. daemon startup → async connect playwright → tools cached
//   2. PostConnectDisconnectIfDiscoveryOnly intentionally Disconnects
//   3. user POSTs /config/reload (e.g. Desktop's startup sync ping)
//   4. retry sees playwright "not connected" → StartConnectAll again
//   5. successful connect → ProbeNow → serverLoop probeNowCh handler →
//      maybeRelaunchDegradedCDPChrome relaunches Chrome because the
//      capability probe ran without Chrome on the previous cycle and
//      left state=Degraded
//
// Net effect: a blank Chrome window pops every time the Desktop client
// reconnects. mgr.CachedTools() being non-empty is the "we already
// discovered, this Disconnect is intentional" signal — empty cache means
// the first connect attempt failed (genuine retry case).
func ShouldSkipReloadRetry(mcpMgr *mcp.ClientManager, serverName string, cfg mcp.MCPServerConfig) bool {
	if cfg.KeepAlive {
		return false
	}
	if serverName != "playwright" {
		return false
	}
	if mcpMgr == nil {
		return false
	}
	return len(mcpMgr.CachedTools(serverName)) > 0
}

// PostConnectDisconnectIfDiscoveryOnly preserves the legacy "discover-then-
// disconnect" optimization for playwright when its config has KeepAlive=
// false. The synchronous registration path used to disconnect playwright
// right after the initial ConnectAll so Chrome wouldn't stay open idle;
// the async path doesn't have a natural place to do that, so this helper
// runs from the daemon's onResult success callback. Tools remain in the
// mgr's tool cache, so CallTool's existing on-demand reconnect path
// handles tool invocation by re-spawning playwright-mcp + Chrome.
//
// Generic for all server names — currently only playwright opts into this
// behavior via KeepAlive=false, but the function is name-agnostic so a
// future built-in can opt in the same way.
func PostConnectDisconnectIfDiscoveryOnly(mcpMgr *mcp.ClientManager, serverName string) {
	if mcpMgr == nil {
		return
	}
	cfg, ok := mcpMgr.ConfigFor(serverName)
	if !ok || cfg.KeepAlive {
		return
	}
	// Restrict to playwright for now: it's the only server where idle
	// resource pressure (Chrome) justifies the extra reconnect roundtrip
	// on first tool call. Generalize when a second server needs it.
	if serverName != "playwright" {
		return
	}
	mcpMgr.Disconnect(serverName)
	log.Printf("[mcp] %s: disconnected after tool discovery (KeepAlive=false); will reconnect on demand", serverName)
}

// CleanupPlaywrightReconnect runs after a supervisor-driven reconnect.
// Hides Chrome so the persistent connection doesn't steal focus.
// Chrome stays minimized/hidden in all modes — Playwright operates via CDP.
func CleanupPlaywrightReconnect(ctx context.Context, mcpMgr *mcp.ClientManager) {
	if ctx.Err() != nil {
		return
	}
	cfg, ok := mcpMgr.ConfigFor("playwright")
	if !ok || !cfg.KeepAlive {
		return // on-demand: Chrome already stays minimized from launch
	}
	// keep_alive: hide Chrome so it doesn't steal focus.
	time.Sleep(2 * time.Second)
	if ctx.Err() != nil {
		return
	}
	mcp.HideCDPChrome()
}

// ResolveMCPContext builds the MCP context string scoped to the agent's servers.
// If the agent has no MCP config, falls back to global servers.
func ResolveMCPContext(cfg *config.Config, agentDef ...*agents.Agent) string {
	servers := resolveMCPServers(cfg, agentDef...)
	return mcp.BuildContext(servers)
}

// RegisterSessionSearch registers the session_search tool if a manager is available.
func RegisterSessionSearch(reg *agent.ToolRegistry, mgr *session.Manager) {
	if mgr == nil {
		return
	}
	reg.Register(&SessionSearchTool{manager: mgr})
}

// RegisterMemoryTool registers the memory_recall tool. svc may be nil when
// the daemon's memory service failed to start, when memory.provider is
// disabled, or in CLI/TUI attach paths where AttachPolicy returned ready=false.
// fallback must always be supplied so the tool can degrade gracefully.
func RegisterMemoryTool(reg *agent.ToolRegistry, svc MemoryQuerier, fallback FallbackQuery) {
	if reg == nil {
		return
	}
	reg.Register(&MemoryTool{Service: svc, Fallback: fallback})
}

// RegisterCloudDelegate registers the cloud_delegate tool if cloud is enabled.
func RegisterCloudDelegate(reg *agent.ToolRegistry, gw *client.GatewayClient, cfg *config.Config, handler agent.EventHandler, agentName, agentPrompt string) {
	if cfg == nil || !cfg.Cloud.Enabled || cfg.APIKey == "" {
		return
	}
	timeout := time.Duration(cfg.Cloud.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 3600 * time.Second
	}
	idleTimeout := time.Duration(cfg.Cloud.StreamIdleTimeoutSecs) * time.Second
	reg.Register(NewCloudDelegateTool(gw, cfg.APIKey, timeout, idleTimeout, handler, agentName, agentPrompt))
}

// RegisterPublishTool registers the publish_to_web tool. It needs the gateway
// client (for the shared *http.Client) and a configured API key — without a
// key, /api/v1/uploads will reject every call with 401, so we skip rather
// than register a tool that can only fail.
func RegisterPublishTool(reg *agent.ToolRegistry, gw *client.GatewayClient, cfg *config.Config) {
	if cfg == nil || !cfg.Cloud.Enabled || cfg.APIKey == "" || gw == nil {
		return
	}
	allow := buildPublishAllowlist(cfg.Cloud.PublishAllowedExtensions)
	uploadsClient := uploads.NewClient(cfg.Endpoint, cfg.APIKey, gw.HTTPClient())
	reg.Register(NewPublishToWebTool(uploadsClient, allow))
}

// RegisterGenerateImageTool registers the generate_image tool. Same gating as
// publish_to_web: needs the gateway client (for the shared *http.Client) and
// a configured API key — without a key, /api/v1/images/generations will 401.
func RegisterGenerateImageTool(reg *agent.ToolRegistry, gw *client.GatewayClient, cfg *config.Config) {
	if cfg == nil || !cfg.Cloud.Enabled || cfg.APIKey == "" || gw == nil {
		return
	}
	imagesClient := images.NewClient(cfg.Endpoint, cfg.APIKey, gw.HTTPClient())
	reg.Register(NewGenerateImageTool(imagesClient))
}

// RegisterEditImageTool registers the edit_image tool. Same gating as
// generate_image: needs the gateway client (for the shared *http.Client) and
// a configured API key — without a key, /api/v1/images/edits will 401. The
// edit endpoint requires image_urls under https://static.kocoro.ai/, so
// register alongside generate_image and publish_to_web (the two ways to
// produce CDN URLs the LLM can feed in).
func RegisterEditImageTool(reg *agent.ToolRegistry, gw *client.GatewayClient, cfg *config.Config) {
	if cfg == nil || !cfg.Cloud.Enabled || cfg.APIKey == "" || gw == nil {
		return
	}
	imagesClient := images.NewClient(cfg.Endpoint, cfg.APIKey, gw.HTTPClient())
	reg.Register(NewEditImageTool(imagesClient))
}

// RegisterListPublishedFilesTool registers the read-only list_my_published_files
// tool. Same gating as publish_to_web — it talks to the same /api/v1/uploads
// collection, so without cloud.enabled + api_key the endpoint returns 401.
// Tool is read-only and does not require approval.
func RegisterListPublishedFilesTool(reg *agent.ToolRegistry, gw *client.GatewayClient, cfg *config.Config) {
	if cfg == nil || !cfg.Cloud.Enabled || cfg.APIKey == "" || gw == nil {
		return
	}
	uploadsClient := uploads.NewClient(cfg.Endpoint, cfg.APIKey, gw.HTTPClient())
	reg.Register(NewListPublishedFilesTool(uploadsClient))
}

// RegisterRetractPublishedFileTool registers the destructive
// retract_published_file tool. Same gating as publish_to_web. Tool requires
// approval but is intentionally NOT on the high-risk DisallowsAutoApproval
// denylist — retract destroys public content rather than creating it, so
// always_allow is a legitimate user choice (see plan Q2).
func RegisterRetractPublishedFileTool(reg *agent.ToolRegistry, gw *client.GatewayClient, cfg *config.Config) {
	if cfg == nil || !cfg.Cloud.Enabled || cfg.APIKey == "" || gw == nil {
		return
	}
	uploadsClient := uploads.NewClient(cfg.Endpoint, cfg.APIKey, gw.HTTPClient())
	reg.Register(NewRetractPublishedFileTool(uploadsClient))
}

// buildPublishAllowlist merges user-supplied extensions onto the default
// allowlist. Extensions are normalised to lowercase and given a leading dot
// if missing. Empty / nil extra returns the default unmodified.
func buildPublishAllowlist(extra []string) map[string]bool {
	out := make(map[string]bool, len(defaultExtAllowlist)+len(extra))
	for k, v := range defaultExtAllowlist {
		out[k] = v
	}
	for _, e := range extra {
		e = strings.TrimSpace(strings.ToLower(e))
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		out[e] = true
	}
	return out
}

// ExtractGatewayTools returns all *ServerTool entries from a registry.
func ExtractGatewayTools(reg *agent.ToolRegistry) []agent.Tool {
	var result []agent.Tool
	for _, t := range reg.All() {
		if _, ok := t.(*ServerTool); ok {
			result = append(result, t)
		}
	}
	return result
}

// ExtractPostOverlays returns tools in full that are not in baseline,
// not *MCPTool, and not *ServerTool.
func ExtractPostOverlays(full, baseline *agent.ToolRegistry) []agent.Tool {
	var result []agent.Tool
	for _, t := range full.All() {
		name := t.Info().Name
		if _, inBaseline := baseline.Get(name); inBaseline {
			continue
		}
		if _, isMCP := t.(*MCPTool); isMCP {
			continue
		}
		if _, isGW := t.(*ServerTool); isGW {
			continue
		}
		result = append(result, t)
	}
	return result
}

// RebuildRegistryForHealth creates a new registry from cached layers,
// including tools from MCP servers. Exposure by health state:
//   - Healthy: tools work directly.
//   - Disconnected: tools exposed with on-demand reconnect (via supervisor) so
//     the LLM triggers reconnect only when it actually invokes a tool.
//   - Degraded: hidden by default — a failing capability probe means a tool
//     call would surface a broken cached tool (and, for playwright, strip the
//     working legacy fallback). The ONE exception is Playwright in CDP mode
//     with keep_alive=false: there Degraded is the expected idle state after a
//     prior turn's on-demand Chrome teardown, so its tools stay exposed with
//     on-demand reconnect and mcp_tool.go's pre-call ensureChromeDebugPort
//     relaunches Chrome when a browser tool is actually invoked.
//
// When Playwright tools are present (healthy or cached), the legacy browser
// tool is removed.
func RebuildRegistryForHealth(
	baseline *agent.ToolRegistry,
	gatewayOverlay []agent.Tool,
	postOverlays []agent.Tool,
	healthStates map[string]mcp.ServerHealth,
	mcpMgr *mcp.ClientManager,
	supervisor *mcp.Supervisor,
) *agent.ToolRegistry {
	reg := baseline.Clone()

	playwrightPresent := false
	if mcpMgr != nil {
		for serverName, health := range healthStates {
			// onDemandDegraded is the one narrow case where a Degraded server's
			// cached tools stay exposed: Playwright in CDP mode with
			// keep_alive=false, where Degraded is the expected idle state after a
			// prior turn's on-demand Chrome teardown. Its tools recover on demand
			// (mcp_tool.go ensureChromeDebugPort relaunches Chrome before the
			// call) the moment the agent invokes a browser tool.
			onDemandDegraded := false
			switch health.State {
			case mcp.StateHealthy, mcp.StateDisconnected:
				// Healthy works directly; Disconnected is exposed with on-demand
				// reconnect (handled below).
			case mcp.StateDegraded:
				// Any OTHER Degraded server (non-CDP playwright, keep_alive=true,
				// or a future capability-probed server) stays hidden — exposing a
				// server whose capability probe is failing would surface broken
				// cached tools and, for playwright, strip the working fallback.
				cfg, ok := mcpMgr.ConfigFor(serverName)
				if ok && serverName == "playwright" && mcp.IsPlaywrightCDPMode(cfg) && !cfg.KeepAlive {
					onDemandDegraded = true
				} else {
					continue
				}
			default:
				// Unknown/future state — re-evaluate exposure rules before adding one.
				continue
			}
			tools := mcpMgr.CachedTools(serverName)
			for _, t := range tools {
				if _, exists := reg.Get(t.Tool.Name); exists {
					continue
				}
				mt := NewMCPTool(t.ServerName, t.Tool, mcpMgr)
				// Disconnected and the scoped on-demand Degraded get the supervisor
				// for on-demand reconnect: Chrome only relaunches when the LLM
				// actually invokes a browser tool, never from the turn-start probe.
				if (health.State == mcp.StateDisconnected || onDemandDegraded) && supervisor != nil {
					mt.SetSupervisor(supervisor)
				}
				reg.Register(mt)
				if t.Tool.Name == "browser_navigate" {
					playwrightPresent = true
				}
			}
		}
	}

	// Do NOT call browserTool.Cleanup() — in-flight sessions share the instance.
	// Only remove from the NEW registry.
	if playwrightPresent {
		for _, legacy := range []string{"browser", "applescript", "accessibility", "screenshot", "wait_for"} {
			reg.Remove(legacy)
		}
	}

	for _, t := range gatewayOverlay {
		if _, exists := reg.Get(t.Info().Name); !exists {
			reg.Register(t)
		}
	}

	for _, t := range postOverlays {
		reg.Register(t)
	}

	return reg
}
