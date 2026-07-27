package tools

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/daemon/desktop_rpc"
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
	reg.Register(&PresentDeliverableTool{})
	reg.Register(&ProcessTool{})
	reg.Register(&AppleScriptTool{})
	axClient := SharedAXClient()
	reg.Register(&AccessibilityTool{client: axClient})
	reg.Register(&ComputerUseTool{
		client:                        axClient,
		coordinateExecutor:            axClient.CoordinateMouseEventV1,
		coordinateDragExecutor:        axClient.CoordinateDragV1,
		coordinatePixelScrollExecutor: axClient.CoordinatePixelScrollV1,
		semanticTextSelectionExecutor: axClient.SemanticTextSelectionV2,
		semanticPressExecutor:         axClient.SemanticPressV2,
		targetBoundInputExecutor:      axClient.TargetBoundInputV1,
	})
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

	// CLI, TUI, and daemon all share this registry. Guard every local GUI
	// describer at the final Tool.Run seam; only the daemon coordinator can
	// mint the opaque authority required for mutations. Observations retain
	// their existing direct behavior.
	guardRegisteredGUIExecution(reg)

	cleanup := func() {
		if !browser.IsDeprecated() {
			browser.Cleanup()
		}
		axClient.Close()
	}
	return reg, skillsPtr, cleanup
}

// CloneWithRuntimeConfig returns a registry clone with session-scoped local tool
// settings applied. Tools with per-run mutable state are deep-copied so
// concurrent routes don't share refs, state IDs, dimensions, or handlers.
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

	// AX-backed GUI tools share the process-wide transport (ax_server accepts
	// one client) but never their run-local observations or cached dimensions.
	if raw, ok := cloned.Get("accessibility"); ok {
		if existing, ok := unwrapGUIExecutionGate(raw).(*AccessibilityTool); ok {
			toolCopy := *existing
			toolCopy.refs = nil
			toolCopy.lastPID = 0
			toolCopy.lastBundleID = ""
			toolCopy.lastAppName = ""
			cloned.Register(wrapGUIExecutionGate(&toolCopy))
		}
	}
	if raw, ok := cloned.Get("computer_use"); ok {
		if existing, ok := unwrapGUIExecutionGate(raw).(*ComputerUseTool); ok {
			toolCopy := *existing
			toolCopy.initialTarget = nil
			toolCopy.snapshot = nil
			toolCopy.refs = nil
			toolCopy.coordinateArtifact = nil
			toolCopy.coordinateFocus = nil
			toolCopy.navigationCommit = nil
			cloned.Register(wrapGUIExecutionGate(&toolCopy))
		}
	}
	if raw, ok := cloned.Get("computer"); ok {
		if existing, ok := unwrapGUIExecutionGate(raw).(*ComputerTool); ok {
			toolCopy := *existing
			toolCopy.screenW = 0
			toolCopy.screenH = 0
			cloned.Register(wrapGUIExecutionGate(&toolCopy))
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

func rawComputerUseToolForRun(tool agent.Tool) *ComputerUseTool {
	for tool != nil {
		switch typed := tool.(type) {
		case *ComputerUseTool:
			return typed
		case *AnthropicComputerAdapter:
			return typed.raw
		case *axOnlyComputerUseTool:
			tool = typed.inner
		case guiExecutionGuarded:
			tool = typed.guiExecutionInner()
		default:
			return nil
		}
	}
	return nil
}

// BindComputerUseInitialTargetForRun binds the Desktop-captured foreground app
// to one already-cloned registry. Provider adapters keep this metadata behind
// their native wire contract.
func BindComputerUseInitialTargetForRun(
	reg *agent.ToolRegistry,
	target ComputerUseInitialTargetV1,
) error {
	if reg == nil {
		return fmt.Errorf("cannot bind computer-use target to a nil registry")
	}
	target.AppName = strings.TrimSpace(target.AppName)
	target.BundleID = strings.TrimSpace(target.BundleID)
	if target.PID <= 0 || !ValidAppNamePattern.MatchString(target.AppName) ||
		!consequentialRiskBundleIDPatternV1.MatchString(target.BundleID) {
		return fmt.Errorf("computer-use initial target is invalid")
	}
	for _, name := range []string{"computer_use", client.NativeComputerToolName} {
		registered, ok := reg.Get(name)
		if !ok {
			continue
		}
		if raw := rawComputerUseToolForRun(registered); raw != nil {
			copy := target
			raw.initialTarget = &copy
			return nil
		}
	}
	return fmt.Errorf("run registry has no computer-use core")
}

// RequireExplicitComputerUseTargetForRun makes the first observation on a
// run-local generic tool require an app name (or a Desktop-bound initial
// target). It is used for schedules and other unattended sources so they never
// inherit an unrelated app that happens to be frontmost at execution time.
func RequireExplicitComputerUseTargetForRun(reg *agent.ToolRegistry) error {
	if reg == nil {
		return fmt.Errorf("cannot scope computer-use target on a nil registry")
	}
	registered, ok := reg.Get("computer_use")
	if !ok {
		return nil
	}
	raw := rawComputerUseToolForRun(registered)
	if raw == nil {
		return fmt.Errorf("run registry computer_use has no configurable core")
	}
	raw.targetScope = computerUseTargetScopeExplicitV1
	return nil
}

// CloneWithGenericComputerUseForRun creates the provider-neutral function-tool
// surface for a resolved generic route or a safe old-Cloud fallback. The legacy
// `computer` function is deliberately absent so fallback can never silently
// re-enter its fixed-canvas coordinate contract.
//
// supportsToolResultImages must come from a trusted resolved execution profile.
// When false (or when no profile exists), the public computer_use identity is
// retained but wrapped in an AX-only gate that rejects screenshot/coordinate
// inputs and strips any unexpected image result fail-closed.
func CloneWithGenericComputerUseForRun(
	reg *agent.ToolRegistry,
	cfg *config.Config,
	supportsToolResultImages bool,
) (*agent.ToolRegistry, error) {
	cloned := CloneWithRuntimeConfig(reg, cfg)
	if cloned == nil {
		return nil, fmt.Errorf("cannot create generic computer_use clone from nil registry")
	}
	cloned.Remove(client.NativeComputerToolName)
	disableLegacyGUIFallbacksForComputerUseRun(cloned)
	public, ok := cloned.Get("computer_use")
	if !ok {
		// Narrow test/embedded registries may intentionally contain no GUI
		// tools. Preserve that absence rather than inventing capability or
		// failing an otherwise unrelated run.
		return cloned, nil
	}
	if !supportsToolResultImages {
		cloned.Register(&axOnlyComputerUseTool{inner: public})
	}
	return cloned, nil
}

func disableLegacyGUIFallbacksForComputerUseRun(reg *agent.ToolRegistry) {
	if reg == nil {
		return
	}
	reg.Remove("accessibility")
	reg.Remove("applescript")
	if raw, ok := reg.Get("bash"); ok {
		if bash, ok := raw.(*BashTool); ok {
			bash.LegacyGUIAutomationDisabled = true
		}
	}
}

// CloneWithResolvedComputerUseProfileForRun selects exactly one public
// computer-use contract from a profile minted by the authenticated Cloud
// resolve call. Callers cannot synthesize the trusted seal by decoding JSON.
func CloneWithResolvedComputerUseProfileForRun(
	reg *agent.ToolRegistry,
	cfg *config.Config,
	profile *client.ExecutionProfile,
) (*agent.ToolRegistry, error) {
	if profile == nil || !profile.IsTrustedResolution() {
		return nil, fmt.Errorf("computer-use execution profile is not a trusted resolution")
	}
	switch profile.ExecutionMode() {
	case client.ExecutionModeFunctionComputerUse:
		cloned, err := CloneWithGenericComputerUseForRun(
			reg,
			cfg,
			profile.SupportsToolResultImages(),
		)
		if err != nil {
			return nil, err
		}
		return cloned, nil
	case client.ExecutionModeNativeComputer:
		if profile.Provider() == client.NativeComputerProviderAnthropic &&
			profile.APISurface() == client.APISurfaceAnthropicMessages &&
			profile.ToolContract() == client.ToolContractAnthropicComputer20251124 &&
			profile.BetaContract() == client.AnthropicComputerBetaContract {
			// The provider declaration remains stable. Exact target-window
			// screenshots are captured lazily and letterboxed into this canvas.
			capability := newAnthropicComputerRunCapabilityAfterVerification(1280, 800)
			cloned, err := CloneWithAnthropicComputerForRun(reg, cfg, capability)
			if err != nil {
				return nil, err
			}
			disableLegacyGUIFallbacksForComputerUseRun(cloned)
			// The adapter owns the same clone-local ComputerUseTool core; it must
			// not also be advertised as a competing generic function schema.
			cloned.Remove("computer_use")
			return cloned, nil
		}
		if profile.Provider() == client.OpenAIComputerProvider &&
			profile.APISurface() == client.APISurfaceOpenAIResponses &&
			profile.ToolContract() == client.ToolContractOpenAIComputerV1 &&
			profile.BetaContract() == "" &&
			profile.SupportsImageInput() &&
			profile.SupportsToolResultImages() &&
			!profile.SupportsFunctionTools() &&
			profile.SupportsBatchedActions() {
			cloned, err := CloneWithOpenAIComputerForRun(reg, cfg)
			if err != nil {
				return nil, err
			}
			disableLegacyGUIFallbacksForComputerUseRun(cloned)
			// computer_use remains registered only as the daemon-private action
			// core. AgentLoop exposes solely {"type":"computer"} for this exact
			// trusted profile.
			return cloned, nil
		}
		{
			return nil, fmt.Errorf(
				"native computer profile %q has no Kocoro executor in this build",
				profile.ToolContract(),
			)
		}
	case client.ExecutionModeUnavailable:
		return nil, fmt.Errorf("resolved model does not support computer use")
	default:
		return nil, fmt.Errorf(
			"unsupported computer-use execution mode %q",
			profile.ExecutionMode(),
		)
	}
}

// CloneWithOpenAIComputerForRun replaces the clone-local legacy computer tool
// with the Responses native schema marker while retaining the guarded
// computer_use core for daemon-private per-action planning and execution.
func CloneWithOpenAIComputerForRun(
	reg *agent.ToolRegistry,
	cfg *config.Config,
) (*agent.ToolRegistry, error) {
	if reg == nil {
		return nil, fmt.Errorf("cannot enable OpenAI computer adapter for a nil registry")
	}
	cloned := CloneWithRuntimeConfig(reg, cfg)
	raw, ok := cloned.Get("computer_use")
	if !ok {
		return nil, fmt.Errorf("OpenAI computer adapter requires clone-local computer_use")
	}
	if _, err := NewOpenAIComputerActionRuntimeV1(raw); err != nil {
		return nil, err
	}
	legacy, ok := cloned.Get(client.NativeComputerToolName)
	if !ok {
		return nil, fmt.Errorf("OpenAI computer adapter requires legacy %q entry",
			client.NativeComputerToolName)
	}
	if _, ok := unwrapGUIExecutionGate(legacy).(*ComputerTool); !ok {
		return nil, fmt.Errorf(
			"OpenAI computer adapter may replace only legacy ComputerTool, got %T",
			unwrapGUIExecutionGate(legacy),
		)
	}
	marker := NewOpenAIComputerAdapterV1(nil)
	definition := marker.NativeToolDef()
	if definition == nil {
		return nil, fmt.Errorf("OpenAI computer adapter produced no native definition")
	}
	if err := definition.Validate(); err != nil {
		return nil, fmt.Errorf("OpenAI computer adapter native definition: %w", err)
	}
	cloned.Register(marker)
	return cloned, nil
}

// AnthropicComputerRunCapability is an opaque, run-local authorization minted
// only after a caller has verified the exact provider/model/profile contract.
//
// The zero value is deliberately invalid, its fields are not caller-settable,
// and no public mint exists while production provider attestation is absent.
// This keeps the adapter seam testable without turning a config bool into an
// accidental production enable path.
type AnthropicComputerRunCapability struct {
	seal                   *anthropicComputerRunCapabilitySeal
	initialDisplayWidthPX  int
	initialDisplayHeightPX int
}

type anthropicComputerRunCapabilitySeal struct {
	marker byte
}

var trustedAnthropicComputerRunCapabilitySeal = &anthropicComputerRunCapabilitySeal{marker: 1}

// newAnthropicComputerRunCapabilityAfterVerification is intentionally private.
// A future provider/profile verifier in this package may call it only after it
// has attested the whole native contract. For now it is used solely by focused
// seam tests, so CLI, TUI, and daemon production paths cannot mint capability.
func newAnthropicComputerRunCapabilityAfterVerification(
	initialDisplayWidthPX int,
	initialDisplayHeightPX int,
) AnthropicComputerRunCapability {
	return AnthropicComputerRunCapability{
		seal:                   trustedAnthropicComputerRunCapabilitySeal,
		initialDisplayWidthPX:  initialDisplayWidthPX,
		initialDisplayHeightPX: initialDisplayHeightPX,
	}
}

// CloneWithAnthropicComputerForRun creates a normal run-local clone, then
// atomically replaces only the clone's legacy public `computer` entry with the
// provider-native adapter. The adapter always delegates to that same clone's
// private `computer_use` core.
//
// The input registry is never mutated. Missing or unexpected tools, an
// untrusted capability, or an invalid native definition return nil and an
// error without exposing a partially replaced clone.
func CloneWithAnthropicComputerForRun(
	reg *agent.ToolRegistry,
	cfg *config.Config,
	capability AnthropicComputerRunCapability,
) (*agent.ToolRegistry, error) {
	if reg == nil {
		return nil, fmt.Errorf("cannot enable Anthropic computer adapter for a nil registry")
	}
	if capability.seal != trustedAnthropicComputerRunCapabilitySeal {
		return nil, fmt.Errorf("Anthropic computer adapter requires verified run capability")
	}
	if capability.initialDisplayWidthPX <= 0 || capability.initialDisplayHeightPX <= 0 {
		return nil, fmt.Errorf("Anthropic computer adapter capability requires positive display dimensions")
	}

	cloned := CloneWithRuntimeConfig(reg, cfg)
	rawRegistered, ok := cloned.Get("computer_use")
	if !ok {
		return nil, fmt.Errorf("Anthropic computer adapter requires clone-local computer_use")
	}
	raw, ok := unwrapGUIExecutionGate(rawRegistered).(*ComputerUseTool)
	if !ok {
		return nil, fmt.Errorf("Anthropic computer adapter requires ComputerUseTool, got %T",
			unwrapGUIExecutionGate(rawRegistered))
	}
	legacyRegistered, ok := cloned.Get(client.NativeComputerToolName)
	if !ok {
		return nil, fmt.Errorf("Anthropic computer adapter requires legacy %q entry",
			client.NativeComputerToolName)
	}
	if _, ok := unwrapGUIExecutionGate(legacyRegistered).(*ComputerTool); !ok {
		return nil, fmt.Errorf("Anthropic computer adapter may replace only legacy ComputerTool, got %T",
			unwrapGUIExecutionGate(legacyRegistered))
	}

	adapter := NewAnthropicComputerAdapter(
		raw,
		capability.initialDisplayWidthPX,
		capability.initialDisplayHeightPX,
	)
	definition := adapter.NativeToolDef()
	if definition == nil {
		return nil, fmt.Errorf("Anthropic computer adapter produced no native definition")
	}
	if err := definition.Validate(); err != nil {
		return nil, fmt.Errorf("Anthropic computer adapter native definition: %w", err)
	}
	guarded := wrapGUIExecutionGate(adapter)
	if _, ok := guarded.(agent.NativeToolProvider); !ok {
		return nil, fmt.Errorf("Anthropic computer adapter execution gate lost native provider trait")
	}

	cloned.Register(guarded)
	return cloned, nil
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

// RegisterIntegrationTools fetches the caller's active third-party integration
// tools from the gateway and (re)registers them on reg. Unlike gateway tools
// there is NO local allowlist — Cloud already filters by the user's active
// connections and its own whitelist, so we trust whatever it returns. Local
// tools keep priority (a name collision is skipped). Idempotent: this doubles
// as a refresh when the user connects/disconnects an account.
//
// Fetch-then-replace: the list is fetched BEFORE any existing integration tools
// are removed, so a failed Cloud round-trip leaves the registry untouched
// (previously registered tools survive the outage) rather than wiping the tools
// and returning empty. Callers must still serialize concurrent refreshes so two
// overlapping runs can't apply stale snapshots out of order (see
// Server.toolRefreshMu).
func RegisterIntegrationTools(ctx context.Context, gw *client.GatewayClient, reg *agent.ToolRegistry) error {
	if reg == nil {
		return fmt.Errorf("tool registry is nil")
	}
	if gw == nil {
		return nil
	}

	schemas, err := gw.ListIntegrationTools(ctx)
	if err != nil {
		// Registry left as-is: keep the previously registered integration tools
		// through a transient integration-endpoint outage.
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == 404 {
			// Older or feature-disabled Cloud deployments do not expose this
			// optional endpoint. Treat that as "integration discovery is not
			// available" instead of logging an error on every agent run.
			return nil
		}
		return fmt.Errorf("integration tools unavailable: %w", err)
	}

	// Fetch succeeded — now replace the integration subset. Drop stale tools
	// (a disconnected provider's tools disappear) then register the current set.
	for _, t := range reg.All() {
		if sourcer, ok := t.(agent.ToolSourcer); ok && sourcer.ToolSource() == agent.SourceIntegration {
			reg.Remove(t.Info().Name)
		}
	}
	for _, schema := range schemas {
		if _, exists := reg.Get(schema.Name); exists {
			continue // local/gateway tool takes priority
		}
		reg.Register(NewIntegrationTool(schema, gw))
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

// ApplyMCPServerScope removes MCP tools whose server is not in the agent's
// resolved, enabled MCP server set (per-agent mcp_servers config). Returns a
// new registry, or the original if nothing is scoped out. Local and gateway
// tools are never touched — only *MCPTool, which is the sole agent.SourceMCP
// implementation (see ToolRegistry.partitionBySourceLocked). The default agent
// (no agentDef) resolves to the full global set, so this is a no-op there.
//
// This is the ENFORCEMENT half of per-agent MCP config: resolveMCPServers
// previously only fed the system-prompt context string, so on the daemon path
// the shared registry let every agent call every connected server's tools
// regardless of its mcp_servers config. Scoping the registry here closes that.
func ApplyMCPServerScope(reg *agent.ToolRegistry, cfg *config.Config, agentDef ...*agents.Agent) *agent.ToolRegistry {
	// Default agent with an empty denylist: nothing to subtract, so return the
	// registry untouched. Keeps the default path a true no-op even for an MCP
	// tool whose server is not a key in cfg.MCPServers (allowed[] below is built
	// only from cfg.MCPServers, so such a tool would otherwise be silently
	// stripped). Named agents and a non-empty default denylist fall through.
	isDefaultAgent := len(agentDef) == 0 || agentDef[0] == nil
	if isDefaultAgent && (cfg == nil || len(cfg.MCP.DefaultAgentDisabled) == 0) {
		return reg
	}
	resolved := resolveMCPServers(cfg, agentDef...)
	allowed := make(map[string]bool, len(resolved))
	for name, srv := range resolved {
		if !srv.Disabled {
			allowed[name] = true
		}
	}
	// Default agent (no agentDef): subtract the default-agent MCP denylist
	// (config.mcp.default_agent_disabled). Named agents already express their
	// selection through mcp_servers above and must not be touched by it.
	if cfg != nil && (len(agentDef) == 0 || agentDef[0] == nil) {
		for _, name := range cfg.MCP.DefaultAgentDisabled {
			delete(allowed, name)
		}
	}
	var deny []string
	for _, t := range reg.All() {
		mt, ok := t.(*MCPTool)
		if !ok {
			continue
		}
		if !allowed[mt.ServerName()] {
			deny = append(deny, mt.Info().Name)
		}
	}
	if len(deny) == 0 {
		return reg
	}
	return reg.FilterByDeny(deny)
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
		// Disable only the legacy browser when Playwright MCP is available.
		// AppleScript, screenshot, and wait_for are native-app tools rather than
		// browser fallbacks. Removing them made advertised built-ins undiscoverable
		// and forced models into rough shell fallbacks such as osascript via bash.
		if hasPlaywright {
			// Shut down any chromedp Chrome instance before removing the tool
			if bt, ok := reg.Get("browser"); ok {
				if browserTool, ok := bt.(*BrowserTool); ok {
					browserTool.Cleanup()
				}
			}
			reg.Remove("browser")
			log.Printf("Playwright MCP connected — disabled legacy browser tool")
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
		if ierr := RegisterIntegrationTools(ctx, gw, reg); ierr != nil {
			log.Printf("integration tools registration failed (continuing): %v", ierr)
		}
	}

	// Re-assert the execution boundary after composing an arbitrary baseline
	// with remote tools. Production baselines already arrive guarded, but this
	// keeps registry rebuilds and future local GUI describers fail-closed even
	// if their caller supplied a raw ToolRegistry.
	guardRegisteredGUIExecution(reg)

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
		if ierr := RegisterIntegrationTools(ctx, gw, reg); ierr != nil {
			log.Printf("integration tools registration failed (continuing): %v", ierr)
		}
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
//  1. daemon startup → async connect playwright → tools cached
//  2. PostConnectDisconnectIfDiscoveryOnly intentionally Disconnects
//  3. user POSTs /config/reload (e.g. Desktop's startup sync ping)
//  4. retry sees playwright "not connected" → StartConnectAll again
//  5. successful connect → ProbeNow → serverLoop probeNowCh handler →
//     maybeRelaunchDegradedCDPChrome relaunches Chrome because the
//     capability probe ran without Chrome on the previous cycle and
//     left state=Degraded
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

// RegisterCalendarTools registers the calendar.* tool family if a
// DesktopRPCBroker is available. Without a broker (TUI / one-shot CLI / MCP
// server / scheduled task modes that don't have a Desktop spawn relationship),
// calendar tools are not registered at all — the model can fall back naturally
// to the `applescript` tool + Calendar.app per spec §4.3.
//
// MUST be called before ExtractPostOverlays in cmd/daemon.go so the calendar
// tools land in the PostOverlays layer and survive the registry rebuilds
// triggered by MCP supervisor health changes (see
// TestCalendarTools_SurviveExtractPostOverlays).
func RegisterCalendarTools(reg *agent.ToolRegistry, broker *desktop_rpc.DesktopRPCBroker) {
	if reg == nil || broker == nil {
		return
	}
	reg.Register(&CalendarCheckPermissionTool{Broker: broker})
	reg.Register(&CalendarRequestPermissionTool{Broker: broker})
	reg.Register(&CalendarListSourcesTool{Broker: broker})
	reg.Register(&CalendarListEventsTool{Broker: broker})
	reg.Register(&CalendarGetEventTool{Broker: broker})
	reg.Register(&CalendarCreateEventTool{Broker: broker})
	reg.Register(&CalendarUpdateEventTool{Broker: broker})
	reg.Register(&CalendarDeleteEventTool{Broker: broker})
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
		reg.Remove("browser")
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
