package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/fslock"
	"github.com/Kocoro-lab/ShanClaw/internal/hooks"
	"github.com/Kocoro-lab/ShanClaw/internal/keychain"
	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
	"github.com/Kocoro-lab/ShanClaw/internal/permissions"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// ConfigSource tracks which file a config value came from.
type ConfigSource struct {
	File  string
	Level string // "default", "global", "project", "local"
}

type Config struct {
	Provider           string `mapstructure:"provider"          yaml:"provider"          json:"provider"` // "gateway" (default) or "ollama"
	Endpoint           string `mapstructure:"endpoint"          yaml:"endpoint"          json:"endpoint"`
	APIKey             string `mapstructure:"api_key"           yaml:"api_key"           json:"api_key"`
	apiKeyFromKeychain bool
	ModelTier          string                         `mapstructure:"model_tier"        yaml:"model_tier"        json:"model_tier"`
	Ollama             OllamaConfig                   `mapstructure:"ollama"            yaml:"ollama"            json:"ollama"`
	AutoUpdateCheck    bool                           `mapstructure:"auto_update_check" yaml:"auto_update_check" json:"auto_update_check"`
	Permissions        permissions.PermissionsConfig  `mapstructure:"permissions"       yaml:"permissions"       json:"permissions"`
	Agent              AgentConfig                    `mapstructure:"agent"             yaml:"agent"             json:"agent"`
	Tools              ToolsConfig                    `mapstructure:"tools"             yaml:"tools"             json:"tools"`
	Cloud              CloudConfig                    `mapstructure:"cloud"             yaml:"cloud"             json:"cloud"`
	Daemon             DaemonConfig                   `mapstructure:"daemon"            yaml:"daemon"            json:"daemon"`
	Skills             SkillsConfig                   `mapstructure:"skills"            yaml:"skills"            json:"skills"`
	Memory             MemoryConfig                   `mapstructure:"memory"            yaml:"memory"            json:"memory"`
	Hooks              hooks.HookConfig               `mapstructure:"hooks"             yaml:"hooks"             json:"hooks"`
	MCPServers         map[string]mcp.MCPServerConfig `mapstructure:"mcp_servers"       yaml:"mcp_servers"       json:"mcp_servers"`
	MCP                MCPConfig                      `mapstructure:"mcp"               yaml:"mcp"               json:"mcp"`
	Koe                KoeConfig                      `mapstructure:"koe"               yaml:"koe"               json:"koe"`
	Sources            map[string]ConfigSource        `mapstructure:"-"                 yaml:"-"                 json:"-"`
}

// KoeConfig holds settings for the `shan koe` voice front brain. The daemon
// persists these so Kocoro Desktop's settings panel can read them (GET
// /config/status) and write them (PATCH /config), then pass them when it spawns
// `shan koe`. NO credential lives here — Koe mints its OpenAI ephemeral secret
// via the daemon relay (POST /koe/realtime/mint), never holding a long-lived key.
type KoeConfig struct {
	Enabled  bool   `mapstructure:"enabled"  yaml:"enabled,omitempty"  json:"enabled,omitempty"`
	Model    string `mapstructure:"model"    yaml:"model,omitempty"    json:"model,omitempty"`
	Voice    string `mapstructure:"voice"    yaml:"voice,omitempty"    json:"voice,omitempty"`
	Agent    string `mapstructure:"agent"    yaml:"agent,omitempty"    json:"agent,omitempty"`
	Language string `mapstructure:"language" yaml:"language,omitempty" json:"language,omitempty"`
}

// MCPConfig holds client-side settings shared across all MCP servers.
type MCPConfig struct {
	// WorkspaceRoots are extra directories advertised to MCP servers honoring
	// the client `roots` capability, on top of the Kocoro defaults
	// (attachment staging dir plus common user locations). Intended for
	// project directories the user wants browser_file_upload or similar
	// sandboxed tools to reach without manual copying.
	WorkspaceRoots []string `mapstructure:"workspace_roots" yaml:"workspace_roots,omitempty" json:"workspace_roots,omitempty"`
	// DefaultConnectTimeoutSecs caps per-server startup time when StartConnectAll
	// fans out connection goroutines. Per-server MCPServerConfig.ConnectTimeoutSeconds
	// overrides this. 0 keeps the hardcoded 60s fallback.
	DefaultConnectTimeoutSecs int `mapstructure:"default_connect_timeout_secs" yaml:"default_connect_timeout_secs,omitempty" json:"default_connect_timeout_secs,omitempty"`
	// DefaultAgentDisabled lists MCP server names the DEFAULT agent must not use.
	// Default-agent-only: named agents select servers via their per-agent
	// mcp_servers config and are unaffected. Empty = default agent uses every
	// globally-enabled server (back-compat). Written via POST/DELETE
	// /mcp/default-disabled.
	DefaultAgentDisabled []string `mapstructure:"default_agent_disabled" yaml:"default_agent_disabled,omitempty" json:"default_agent_disabled,omitempty"`
}

type AgentConfig struct {
	MaxIterations  int     `mapstructure:"max_iterations"   yaml:"max_iterations"   json:"max_iterations"`
	Temperature    float64 `mapstructure:"temperature"      yaml:"temperature"      json:"temperature"`
	MaxTokens      int     `mapstructure:"max_tokens"       yaml:"max_tokens"       json:"max_tokens"`
	Thinking       bool    `mapstructure:"thinking"         yaml:"thinking"         json:"thinking"`
	ThinkingMode   string  `mapstructure:"thinking_mode"    yaml:"thinking_mode"    json:"thinking_mode"` // "adaptive" (default) or "enabled" (fixed budget)
	ThinkingBudget int     `mapstructure:"thinking_budget"  yaml:"thinking_budget"  json:"thinking_budget"`
	// ForceThinkTool re-enables the local `think` tool even on paths where
	// native extended thinking is active (the default-skip case). The two
	// signals are redundant on Sonnet 4.6 / Opus 4.7 with adaptive thinking
	// — leaving both registered led to ritual `think({})` emissions that
	// could spin the agent loop. Set to true only if you have a workflow
	// that specifically depends on the explicit planning tool surface.
	ForceThinkTool  bool   `mapstructure:"force_think_tool" yaml:"force_think_tool" json:"force_think_tool"`
	ReasoningEffort string `mapstructure:"reasoning_effort" yaml:"reasoning_effort" json:"reasoning_effort"`
	Model           string `mapstructure:"model"            yaml:"model"            json:"model"`          // specific model override
	Language        string `mapstructure:"language"         yaml:"language"         json:"language"`       // locked reply language as a native name (e.g. "中文"); empty = mirror the user's current-message language
	ContextWindow   int    `mapstructure:"context_window"   yaml:"context_window"   json:"context_window"` // model context window in tokens
	// ObservationWindow keeps only the N most recent browser/GUI tool
	// observations at full fidelity; older ones are stubbed to bound the
	// page/DOM history a long browser loop re-sends each iteration. 0 disables.
	// Default 3 (see agent.defaultObservationWindow).
	ObservationWindow int `mapstructure:"observation_window" yaml:"observation_window" json:"observation_window"`
	// MaxRecentImages keeps the N most recent image-bearing messages before
	// older screenshots are replaced with a placeholder (all images). Default
	// 50; 0 disables the global filter (keep all).
	MaxRecentImages int `mapstructure:"max_recent_images" yaml:"max_recent_images" json:"max_recent_images"`
	// MaxRecentBrowserImages keeps only the N most recent browser/GUI
	// screenshots (scoped by tool); user uploads + non-GUI images stay under
	// MaxRecentImages. Default 1; 0 disables the browser-scoped filter.
	MaxRecentBrowserImages int `mapstructure:"max_recent_browser_images" yaml:"max_recent_browser_images" json:"max_recent_browser_images"`
	// IdleSoftTimeoutSecs / IdleHardTimeoutSecs: turn-level watchdog measured
	// against explicit "idle-counted" phases of the agent loop (waiting on an
	// LLM response). Other phases (tool execution, approval wait, compaction
	// wrappers) are not counted — they have their own bounded owners.
	//
	// IdleSoftTimeoutSecs: emit OnRunStatus("idle_soft", …) after this long
	// in an idle-counted phase. 0 = disabled. Default: 90.
	// IdleHardTimeoutSecs: cancel the run with ErrHardIdleTimeout after this
	// long. 0 = disabled (visibility-only mode; daemon logs a startup WARN).
	// Default: 540 — 60s headroom under the gateway transport ceiling (600s)
	// so cancellation can propagate + cleanup runs before the transport bails.
	IdleSoftTimeoutSecs int `mapstructure:"idle_soft_timeout_secs" yaml:"idle_soft_timeout_secs" json:"idle_soft_timeout_secs"`
	IdleHardTimeoutSecs int `mapstructure:"idle_hard_timeout_secs" yaml:"idle_hard_timeout_secs" json:"idle_hard_timeout_secs"`
	// StreamIdleTimeoutSecs: abort the SSE streaming body when no chunk has
	// arrived for this long. Closes the failure mode IdleHardTimeoutSecs
	// cannot catch: silent TCP-level connection drop mid-response, where the
	// kernel never returns from read() so the turn-elapsed watchdog can't
	// observe progress. 0 = disabled (legacy scanner path). Default: 90.
	StreamIdleTimeoutSecs int   `mapstructure:"stream_idle_timeout_secs" yaml:"stream_idle_timeout_secs" json:"stream_idle_timeout_secs"`
	SkillDiscovery        *bool `mapstructure:"skill_discovery" yaml:"skill_discovery,omitempty" json:"skill_discovery,omitempty"`

	// BashConcurrencyEnabled gates BashTool.IsConcurrencySafeCall. When true
	// (the Phase C default since 2026-05-15), bash invocations that pass the
	// static read-only analyzer (internal/tools/bash_concurrency.go) can share
	// a concurrent batch with other tools. Set to false to force the pre-Phase-A
	// serial-only behavior (e.g. for a project that surfaces UI client without
	// tool_use_id-aware pairing).
	BashConcurrencyEnabled bool `mapstructure:"bash_concurrency_enabled" yaml:"bash_concurrency_enabled" json:"bash_concurrency_enabled"`

	// TimeBasedCompact controls time-gated tool_result clearing. Disabled
	// by default. When enabled, an old tool_result is cleared to a short
	// marker only after the gap since the last assistant response exceeds
	// GapThresholdMinutes — i.e. only when Anthropic's prompt cache has
	// reliably expired so the prefix would be rewritten anyway. See
	// internal/agent/timebasedcompact.go.
	TimeBasedCompact TimeBasedCompactConfig `mapstructure:"time_based_compact" yaml:"time_based_compact" json:"time_based_compact"`

	// PromptSuggestion controls the ghost-text "next prompt" suggestion that
	// appears in the Desktop follow-up input after each assistant turn.
	// Disabled by default. When enabled, after each turn the daemon runs a
	// forked completion call to generate a single 2-12 word suggestion that
	// Desktop renders as ghost text. Acceptance fills the input but still
	// requires the user to press Enter — there is no speculative pre-run of
	// the next assistant reply (intentional design — see plan).
	PromptSuggestion PromptSuggestionConfig `mapstructure:"prompt_suggestion" yaml:"prompt_suggestion" json:"prompt_suggestion"`
}

// TimeBasedCompactConfig is the YAML/JSON-bindable view of the agent's
// time-based microcompact policy. The runtime view lives in
// internal/agent.TimeBasedCompactConfig — they are kept in lockstep.
type TimeBasedCompactConfig struct {
	Enabled             bool `mapstructure:"enabled"               yaml:"enabled"               json:"enabled"`
	GapThresholdMinutes int  `mapstructure:"gap_threshold_minutes" yaml:"gap_threshold_minutes" json:"gap_threshold_minutes"`
	KeepRecent          int  `mapstructure:"keep_recent"           yaml:"keep_recent"           json:"keep_recent"`
}

// PromptSuggestionConfig controls the post-turn next-prompt suggestion feature.
// The forked completion call is a thin specialization on top of
// internal/agent.BuildForkedRequest — see that package for the cache-safety
// invariant (byte-equal request prefix to the main turn).
type PromptSuggestionConfig struct {
	// Enabled is the master switch. When false the entire feature is dormant —
	// no forked calls, no SSE events, no Desktop ghost text. Default: false.
	Enabled bool `mapstructure:"enabled" yaml:"enabled" json:"enabled"`

	// CacheColdThresholdTokens skips suggestion generation when the previous
	// turn's uncached input token count exceeds this value — guards against
	// paying full-price input cost for a 50-token suggestion. Default: 10000.
	CacheColdThresholdTokens int `mapstructure:"cache_cold_threshold_tokens" yaml:"cache_cold_threshold_tokens" json:"cache_cold_threshold_tokens"`

	// MinTurns is the minimum number of completed assistant turns before
	// suggestions are emitted. Default: 2 (first reply is usually too sparse
	// to predict a useful follow-up).
	MinTurns int `mapstructure:"min_turns" yaml:"min_turns" json:"min_turns"`
}

// SkillDiscoveryEnabled returns whether skill discovery is enabled (default: true).
func (c *AgentConfig) SkillDiscoveryEnabled() bool {
	if c.SkillDiscovery == nil {
		return true
	}
	return *c.SkillDiscovery
}

type ToolsConfig struct {
	BashTimeout       int `mapstructure:"bash_timeout"        yaml:"bash_timeout"        json:"bash_timeout"`
	BashMaxTimeout    int `mapstructure:"bash_max_timeout"    yaml:"bash_max_timeout"    json:"bash_max_timeout"`
	BashMaxOutput     int `mapstructure:"bash_max_output"     yaml:"bash_max_output"     json:"bash_max_output"`
	ResultTruncation  int `mapstructure:"result_truncation"   yaml:"result_truncation"   json:"result_truncation"`
	ArgsTruncation    int `mapstructure:"args_truncation"     yaml:"args_truncation"     json:"args_truncation"`
	ServerToolTimeout int `mapstructure:"server_tool_timeout" yaml:"server_tool_timeout" json:"server_tool_timeout"`
	// BrowserResultTruncation caps a single browser/GUI observation at capture
	// time; smaller than ResultTruncation because page/DOM dumps are large and
	// front-loaded. 0 falls back to the generic cap. Default 24000.
	BrowserResultTruncation int `mapstructure:"browser_result_truncation" yaml:"browser_result_truncation" json:"browser_result_truncation"`
}

type CloudConfig struct {
	Enabled bool `mapstructure:"enabled" yaml:"enabled" json:"enabled"`
	Timeout int  `mapstructure:"timeout" yaml:"timeout" json:"timeout"` // seconds
	// StreamIdleTimeoutSecs aborts a cloud-delegate SSE connection when no line
	// (event or 10s heartbeat) arrives for this many seconds, then reconnects
	// via Last-Event-ID. Per-connection liveness probe, NOT a workflow time
	// limit (Timeout bounds total duration). 0 disables. Global-only — NOT a
	// project/local overlay field (see overlayCloudConfig: cloud overlays
	// accept only session-safe publish policy).
	StreamIdleTimeoutSecs int `mapstructure:"stream_idle_timeout_secs" yaml:"stream_idle_timeout_secs" json:"stream_idle_timeout_secs"`
	// PublishAllowedExtensions extends the publish_to_web extension allowlist.
	// Values are merged onto the built-in default set; there is no allowlist
	// override and no user-configurable denylist (denylist must not be widenable).
	PublishAllowedExtensions []string `mapstructure:"publish_allowed_extensions" yaml:"publish_allowed_extensions,omitempty" json:"publish_allowed_extensions,omitempty"`
}

type OllamaConfig struct {
	Endpoint string `mapstructure:"endpoint" yaml:"endpoint" json:"endpoint"`
	Model    string `mapstructure:"model"    yaml:"model"    json:"model"`
}

type MemoryConfig struct {
	Provider               string        `mapstructure:"provider"                  yaml:"provider"                  json:"provider"`
	Endpoint               string        `mapstructure:"endpoint"                  yaml:"endpoint"                  json:"endpoint"`
	APIKey                 string        `mapstructure:"api_key"                   yaml:"api_key"                   json:"api_key"`
	SocketPath             string        `mapstructure:"socket_path"               yaml:"socket_path"               json:"socket_path"`
	BundleRoot             string        `mapstructure:"bundle_root"               yaml:"bundle_root"               json:"bundle_root"`
	TLMPath                string        `mapstructure:"tlm_path"                  yaml:"tlm_path"                  json:"tlm_path"`
	BundlePullInterval     time.Duration `mapstructure:"bundle_pull_interval"      yaml:"bundle_pull_interval"      json:"bundle_pull_interval"`
	BundlePullStartupDelay time.Duration `mapstructure:"bundle_pull_startup_delay" yaml:"bundle_pull_startup_delay" json:"bundle_pull_startup_delay"`
	SidecarReadyTimeout    time.Duration `mapstructure:"sidecar_ready_timeout"     yaml:"sidecar_ready_timeout"     json:"sidecar_ready_timeout"`
	SidecarShutdownGrace   time.Duration `mapstructure:"sidecar_shutdown_grace"    yaml:"sidecar_shutdown_grace"    json:"sidecar_shutdown_grace"`
	SidecarRestartMax      int           `mapstructure:"sidecar_restart_max"       yaml:"sidecar_restart_max"       json:"sidecar_restart_max"`
	ClientRequestTimeout   time.Duration `mapstructure:"client_request_timeout"    yaml:"client_request_timeout"    json:"client_request_timeout"`
}

type DaemonConfig struct {
	AutoApprove   bool   `mapstructure:"auto_approve" yaml:"auto_approve" json:"auto_approve"`
	ChromeProfile string `mapstructure:"chrome_profile" yaml:"chrome_profile,omitempty" json:"chrome_profile,omitempty"`
	// ShareAsyncDefault controls whether POST /sessions/{id}/share returns
	// 202+task_id (true, default) or blocks until upload completes (false).
	// Operators on stacks where the UI has not yet learned to subscribe to
	// share_progress events can flip this to false to keep the legacy
	// synchronous contract until the UI ships. Per-request `?async=true|false`
	// always wins over this default.
	ShareAsyncDefault bool                `mapstructure:"share_async_default" yaml:"share_async_default" json:"share_async_default"`
	ShareMetadata     ShareMetadataConfig `mapstructure:"share_metadata"      yaml:"share_metadata"      json:"share_metadata"`
	// BrowserReloadBackstopSecs bounds how long a deprecated BrowserTool can
	// linger after config reload before the reload handler logs a structured
	// warning. Default 120s; not normally tuned. Raise for workloads with
	// legitimate hour-long browser sessions that span the watchdog window.
	// The watchdog only LOGS — it never calls Cleanup while leases remain
	// (would kill in-flight work), so this is purely a diagnostic threshold.
	BrowserReloadBackstopSecs int `mapstructure:"browser_reload_backstop_secs" yaml:"browser_reload_backstop_secs,omitempty" json:"browser_reload_backstop_secs,omitempty"`
}

// ShareMetadataConfig holds the social-meta defaults injected into the
// session share HTML <head>. Empty fields fall back to either built-in
// defaults (SiteName / SiteURL) or "skip emitting that tag" (DefaultOGImage
// / LogoURL) — see internal/share.buildViewData and the share template for
// the conditional rendering.
type ShareMetadataConfig struct {
	SiteName       string `mapstructure:"site_name"        yaml:"site_name"        json:"site_name"`
	SiteURL        string `mapstructure:"site_url"         yaml:"site_url"         json:"site_url"`
	DefaultOGImage string `mapstructure:"default_og_image" yaml:"default_og_image" json:"default_og_image"`
	// TwitterImage overrides DefaultOGImage specifically for the
	// twitter:image meta tag. Twitter's summary_large_image card wants a
	// 1.91:1 wide image; Facebook / LinkedIn / Slack / Teams render
	// reasonable thumbnails from a square logo, so the two are kept
	// independently configurable. Empty → falls back to DefaultOGImage.
	TwitterImage string `mapstructure:"twitter_image"    yaml:"twitter_image"    json:"twitter_image"`
	LogoURL      string `mapstructure:"logo_url"         yaml:"logo_url"         json:"logo_url"`
}

type SkillsConfig struct {
	Marketplace MarketplaceConfig `mapstructure:"marketplace" yaml:"marketplace" json:"marketplace"`
	// Disabled lists skill Name/Slug values the DEFAULT agent must not load.
	// Default-agent-only: named agents select skills via their _attached.yaml
	// allowlist and are never narrowed by this list. Empty/absent = load every
	// installed skill (back-compat — installs predating this field keep current
	// behavior). Written via POST/DELETE /skills/disabled.
	Disabled []string `mapstructure:"disabled" yaml:"disabled,omitempty" json:"disabled,omitempty"`
}

type MarketplaceConfig struct {
	// RegistryURL is the static skill-index URL backing the /skills/marketplace
	// endpoints (the contract the macOS Desktop consumes).
	RegistryURL string `mapstructure:"registry_url" yaml:"registry_url" json:"registry_url"`
	// ClawHubURL is the ClawHub base URL backing the separate /skills/clawhub
	// endpoints (ClawHub's live online catalog). It never affects the
	// /skills/marketplace contract.
	ClawHubURL string `mapstructure:"clawhub_url" yaml:"clawhub_url" json:"clawhub_url"`
}

func ShannonDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".shannon")
}

func Load() (*Config, error) {
	dir := ShannonDir()
	if dir == "" {
		return nil, fmt.Errorf("failed to resolve home directory")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create config directory %s: %w", dir, err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0700); err != nil {
		return nil, fmt.Errorf("failed to create sessions directory %s: %w", filepath.Join(dir, "sessions"), err)
	}
	if err := InitSettingsFile(dir); err != nil {
		return nil, fmt.Errorf("failed to init settings: %w", err)
	}

	// One-shot config migrations run before viper.ReadInConfig so the
	// rewritten yaml is what viper sees on this same launch. Each
	// migration is gated by ~/.shannon/migrations.json so reruns are no-ops.
	RunPendingMigrations(dir)

	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(dir)

	// Aliases so code that reads cloud.* resolves to the canonical top-level
	// keys. Top-level `endpoint` / `api_key` are the source of truth (see
	// migrateOldConfig + Save); the sync CLI and other newer callers read
	// `cloud.endpoint` / `cloud.api_key` for naming consistency. RegisterAlias
	// must run before ReadInConfig so the lookup redirect is in place when
	// callers query these keys.
	viper.RegisterAlias("cloud.endpoint", "endpoint")
	viper.RegisterAlias("cloud.api_key", "api_key")

	viper.SetDefault("provider", "gateway")
	viper.SetDefault("endpoint", "https://api-dev.shannon.run")
	viper.SetDefault("ollama.endpoint", "http://localhost:11434")
	viper.SetDefault("ollama.model", "")
	viper.SetDefault("api_key", "")
	viper.SetDefault("model_tier", "medium")
	viper.SetDefault("auto_update_check", true)
	// agent.max_iterations: bumped 25 → 40 — typical "refactor 12 files" or
	// "batch-process 20 attachments" tasks routinely need >25 iterations.
	// User-configurable per agent; this is just the default.
	viper.SetDefault("agent.max_iterations", 40)
	viper.SetDefault("agent.system_event_cap", 20)
	viper.SetDefault("agent.reply_route_index_cap", 256)
	viper.SetDefault("agent.temperature", 0)
	// agent.max_tokens: 0 = auto, resolved per request via
	// agent.MaxTokensForModel(specificModel). Explicit non-zero value here
	// (or in yaml / per-agent config) wins and applies to every request.
	// The legacy 32000 constant capped Sonnet 4.6 / Opus 4.6 / Haiku 4.5
	// at half their physical 64K output limit; the model-aware default
	// lifts that cap without forcing users to learn this knob.
	viper.SetDefault("agent.max_tokens", 0)
	viper.SetDefault("agent.thinking", true)
	viper.SetDefault("agent.thinking_mode", "adaptive")
	viper.SetDefault("agent.thinking_budget", 10000)
	viper.SetDefault("agent.force_think_tool", false)
	viper.SetDefault("agent.reasoning_effort", "")
	viper.SetDefault("agent.model", "")
	viper.SetDefault("agent.context_window", 1_000_000)
	// NOTE: if you change these idle/stream defaults, also update
	// docs/config-reference.md AND internal/skills/bundled/skills/kocoro/references/config.md
	// to match — the bundled skill reference is the AI-facing source of truth.
	viper.SetDefault("agent.idle_soft_timeout_secs", 90)
	viper.SetDefault("agent.idle_hard_timeout_secs", 540)    // 60s headroom under the 600s HTTP transport ceiling so cancel can propagate + cleanup runs before transport bails. Set to 0 in yaml to opt out (startup WARN logs).
	viper.SetDefault("agent.stream_idle_timeout_secs", 90)   // per-chunk gap watchdog inside CompleteStream. 0 disables (legacy scanner path).
	viper.SetDefault("agent.bash_concurrency_enabled", true) // Phase C: Desktop now consumes tool_use_id on tool_status events, safe to enable concurrent bash batches by default.
	// Time-based microcompact. Disabled by default — short sessions never
	// compact, and only sessions that idle past the gap threshold will
	// clear old tool_results. 60min matches Anthropic's 1h prompt-cache
	// TTL ceiling. KeepRecent=5 keeps a working tail visible to the model.
	viper.SetDefault("agent.time_based_compact.enabled", false)
	viper.SetDefault("agent.time_based_compact.gap_threshold_minutes", 60)
	viper.SetDefault("agent.time_based_compact.keep_recent", 5)
	// Browser/GUI context trimming (see internal/agent/observation_window.go).
	// Defaults ON: they bound the accumulated page/DOM history a long browser
	// loop re-sends each iteration. observation_window=0 disables the window.
	viper.SetDefault("agent.observation_window", 3)
	viper.SetDefault("agent.max_recent_images", 50)
	viper.SetDefault("agent.max_recent_browser_images", 1)
	// Prompt suggestion (post-turn ghost text). Enabled by default —
	// the daemon runs a forked completion call after each turn to
	// generate a single 2-12 word follow-up suggestion. See
	// internal/config.PromptSuggestionConfig.
	viper.SetDefault("agent.prompt_suggestion.enabled", true)
	viper.SetDefault("agent.prompt_suggestion.cache_cold_threshold_tokens", 10000)
	viper.SetDefault("agent.prompt_suggestion.min_turns", 2)
	viper.SetDefault("tools.bash_timeout", 120)
	viper.SetDefault("tools.bash_max_timeout", 600)
	viper.SetDefault("tools.bash_max_output", 30000)
	viper.SetDefault("tools.result_truncation", 30000)
	viper.SetDefault("tools.browser_result_truncation", 24000) // tighter per-observation cap for browser/GUI page/DOM dumps; 0 = fall back to result_truncation
	viper.SetDefault("tools.args_truncation", 200)
	viper.SetDefault("tools.server_tool_timeout", 5)
	viper.SetDefault("daemon.auto_approve", false)
	viper.SetDefault("daemon.browser_reload_backstop_secs", 120)
	viper.SetDefault("daemon.share_async_default", true)
	// Share HTML social-meta defaults. The default OG image is the same
	// Kocoro logo asset used in the JSON-LD publisher.logo field — not
	// the textbook 1200×630 OG ratio, but it lets share cards render
	// with a brand mark today; swap for a purpose-built 1200×630 hero
	// here when one ships. The share template intentionally omits the
	// og:image:width/height hints so each platform measures the image
	// itself rather than trusting a possibly-wrong static size.
	viper.SetDefault("daemon.share_metadata.site_name", "Kocoro")
	viper.SetDefault("daemon.share_metadata.site_url", "https://www.kocoro.ai/")
	viper.SetDefault("daemon.share_metadata.default_og_image", "https://static.kocoro.ai/public/Po09_46rjwAQoLhAvp-m52HNUCcViv6dx_uMiuUAzr4/logo-3x.png")
	viper.SetDefault("daemon.share_metadata.twitter_image", "https://static.kocoro.ai/public/cmrsQzsDWCJ3pGC989VtOQutwUeE1IQyTsGMJfSBjIk/kocoro-og-1200x630.png")
	viper.SetDefault("daemon.share_metadata.logo_url", "https://static.kocoro.ai/public/quTeFSunx6sZp_MXBBx50h_r9fhY39_tXyiKQJLHFF8/logo-1x.png")
	// mcp.default_connect_timeout_secs: fallback per-server connect timeout
	// used by StartConnectAll when MCPServerConfig.ConnectTimeoutSeconds is 0.
	// 60s matches the pre-async legacy hardcoded value; OAuth-bridged servers
	// (Intercom) override to ~300s in the built-in catalog.
	viper.SetDefault("mcp.default_connect_timeout_secs", 60)
	viper.SetDefault("skills.marketplace.clawhub_url", "https://clawhub.ai")
	viper.SetDefault("skills.marketplace.registry_url", "https://raw.githubusercontent.com/Kocoro-lab/shanclaw-skill-registry/main/index.json")
	// skills.marketplace.max_attempts / .retry_base_backoff_secs: in-client
	// retry of transient upstream failures (503/5xx/429 + network) on catalog
	// GETs. ClawHub returned ~22% 503s under a 50-request load test; with no
	// retry that surfaced as user-visible "marketplace unavailable". Symptom if
	// too low: occasional spurious browse/install failures. Symptom if too high:
	// slow failure when ClawHub is genuinely down (each attempt waits the 15s
	// client timeout). Override in ~/.shannon/config.yaml.
	viper.SetDefault("skills.marketplace.max_attempts", 3)
	viper.SetDefault("skills.marketplace.retry_base_backoff_secs", 1)
	// skills.marketplace.clawhub_cache_ttl_secs: TTL for the ClawHub live-catalog
	// response cache (browse/search/detail/files/file). Short by design — absorbs
	// request bursts/repeat browsing (cutting upstream calls and 503 exposure)
	// without serving a noticeably stale catalog. Symptom if too high: a newly
	// published/edited skill takes up to the TTL to appear. 0 disables caching.
	// Override in ~/.shannon/config.yaml.
	viper.SetDefault("skills.marketplace.clawhub_cache_ttl_secs", 60)
	viper.SetDefault("cloud.enabled", true)
	viper.SetDefault("cloud.timeout", 3600)
	viper.SetDefault("cloud.stream_idle_timeout_secs", 45) // per-connection SSE liveness probe; cloud pings every 10s. 0 disables.

	// sync.* defaults — MUST stay in sync with setSyncDefaults in
	// internal/sync/config.go. The duplicate exists so internal/sync unit
	// tests can establish defaults without importing internal/config.
	//
	// sync.enabled defaults to false: session sync is part of the Episodic
	// Memory pipeline (sessions → cloud training → bundle). The toggle is
	// opt-in; we don't upload conversation history by default.
	viper.SetDefault("sync.enabled", false)
	viper.SetDefault("sync.dry_run", false)
	viper.SetDefault("sync.endpoint", "")
	viper.SetDefault("sync.exclude_agents", []string{})
	viper.SetDefault("sync.exclude_sources", []string{})
	viper.SetDefault("sync.batch_max_sessions", 25)
	viper.SetDefault("sync.batch_max_bytes", 5*1024*1024)
	viper.SetDefault("sync.single_session_max_bytes", 4*1024*1024)
	viper.SetDefault("sync.daemon_interval", "24h")
	viper.SetDefault("sync.daemon_startup_delay", "60s")
	viper.SetDefault("sync.failed_max_attempts_transient", 5)
	viper.SetDefault("sync.lock_timeout", "30s")

	// Memory feature (Phase 2.3). Single source of truth for memory.* defaults;
	// internal/memory/config.go reads these via typed accessors but never
	// registers defaults itself.
	//
	// memory.provider defaults to "disabled": Episodic Memory is opt-in.
	// Once enabled by the user, the Desktop installs tlm.app on demand and
	// writes memory.tlm_path; sync.enabled is also flipped to true in the
	// same patchConfig call (see Kocoro Desktop SettingsView).
	viper.SetDefault("memory.provider", "disabled")
	viper.SetDefault("memory.endpoint", "")
	viper.SetDefault("memory.api_key", "")
	viper.SetDefault("memory.socket_path", "$TMPDIR/com.kocoro.tlm.sock")
	viper.SetDefault("memory.bundle_root", "$HOME/.shannon/memory")
	viper.SetDefault("memory.tlm_path", "")
	viper.SetDefault("memory.bundle_pull_interval", "24h")
	viper.SetDefault("memory.bundle_pull_startup_delay", "60s")
	viper.SetDefault("memory.sidecar_ready_timeout", "15s")
	viper.SetDefault("memory.sidecar_shutdown_grace", "5s")
	viper.SetDefault("memory.sidecar_restart_max", 5)
	viper.SetDefault("memory.client_request_timeout", "5s")

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			configPath := filepath.Join(dir, "config.yaml")
			if err := viper.SafeWriteConfigAs(configPath); err != nil {
				return nil, fmt.Errorf("failed to write config: %w", err)
			}
		} else {
			return nil, fmt.Errorf("failed to read config: %w", err)
		}
	}

	// Migrate old config keys
	migrateOldConfig()

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	hydrateAPIKeyFromKeychain(&cfg, dir)
	if cfg.apiKeyFromKeychain {
		// Keep the hydrated key in-process for older call sites that read
		// viper directly. Save still strips it back out, so Keychain remains
		// the only persistent credential store on macOS.
		viper.Set("api_key", cfg.APIKey)
	}

	// KOCORO_ENDPOINT env var overrides the yaml endpoint. Useful when an
	// external process (e.g. the macOS Desktop app's launch logic) keeps
	// resetting `endpoint` in config.yaml on every boot — exporting this
	// env var locks the daemon to a chosen Cloud URL regardless of yaml
	// state. Empty / unset env = no override (yaml wins as before).
	if override := strings.TrimSpace(os.Getenv("KOCORO_ENDPOINT")); override != "" {
		cfg.Endpoint = override
	}

	// Re-read MCP servers directly from YAML to preserve env var key casing.
	// Viper lowercases all map keys which breaks env vars like API_KEY → api_key.
	globalFile := filepath.Join(dir, "config.yaml")
	fixMCPEnvKeyCasing(&cfg, globalFile)
	mergeBuiltinMCPServers(&cfg)
	cfg.Sources = buildDefaultSources()
	markGlobalSources(&cfg, globalFile)

	if err := validateConfig(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// Clone returns a deep copy of cfg so callers can derive per-run settings
// without mutating the shared base config.
func Clone(cfg *Config) *Config {
	if cfg == nil {
		return nil
	}

	cloned := *cfg

	cloned.Permissions.AllowedDirs = append([]string(nil), cfg.Permissions.AllowedDirs...)
	cloned.Permissions.AllowedCommands = append([]string(nil), cfg.Permissions.AllowedCommands...)
	cloned.Permissions.DeniedCommands = append([]string(nil), cfg.Permissions.DeniedCommands...)
	cloned.Permissions.SensitivePatterns = append([]string(nil), cfg.Permissions.SensitivePatterns...)
	cloned.Permissions.NetworkAllowlist = append([]string(nil), cfg.Permissions.NetworkAllowlist...)

	cloned.Hooks.PreToolUse = append([]hooks.HookEntry(nil), cfg.Hooks.PreToolUse...)
	cloned.Hooks.PostToolUse = append([]hooks.HookEntry(nil), cfg.Hooks.PostToolUse...)
	cloned.Hooks.SessionStart = append([]hooks.HookEntry(nil), cfg.Hooks.SessionStart...)
	cloned.Hooks.Stop = append([]hooks.HookEntry(nil), cfg.Hooks.Stop...)

	if cfg.MCPServers != nil {
		cloned.MCPServers = make(map[string]mcp.MCPServerConfig, len(cfg.MCPServers))
		for name, serverCfg := range cfg.MCPServers {
			serverCopy := serverCfg
			serverCopy.Args = append([]string(nil), serverCfg.Args...)
			if serverCfg.Env != nil {
				serverCopy.Env = make(map[string]string, len(serverCfg.Env))
				for k, v := range serverCfg.Env {
					serverCopy.Env[k] = v
				}
			}
			cloned.MCPServers[name] = serverCopy
		}
	}

	if cfg.Sources != nil {
		cloned.Sources = make(map[string]ConfigSource, len(cfg.Sources))
		for key, src := range cfg.Sources {
			cloned.Sources[key] = src
		}
	}

	// Per-agent denylists: deep-copy so a per-run Clone never aliases the live
	// config's backing array. The /skills/disabled + /mcp/default-disabled DELETE
	// handlers rewrite these in place (slice[:0]); without this copy a run reading
	// the denylist races with a concurrent delete.
	cloned.Skills.Disabled = append([]string(nil), cfg.Skills.Disabled...)
	cloned.MCP.DefaultAgentDisabled = append([]string(nil), cfg.MCP.DefaultAgentDisabled...)

	return &cloned
}

// RuntimeConfigForCWD returns a per-run config view for cwd by applying only
// session-safe project overlays from cwd/.shannon/*.yaml on top of base.
func RuntimeConfigForCWD(base *Config, cwd string) (*Config, error) {
	if base == nil {
		return nil, fmt.Errorf("base config is nil")
	}

	cfg := Clone(base)

	if cwd != "" {
		projectFile := filepath.Join(cwd, ".shannon", "config.yaml")
		mergeRuntimeOverlayFile(cfg, projectFile, "project")

		localFile := filepath.Join(cwd, ".shannon", "config.local.yaml")
		mergeRuntimeOverlayFile(cfg, localFile, "local")
	}

	if err := validateConfig(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// migrateOldConfig handles the transition from llm_url+gateway_url to single endpoint.
// Bypasses viper for writing since viper can't delete keys.
func migrateOldConfig() {
	if !viper.IsSet("llm_url") && !viper.IsSet("gateway_url") {
		return
	}

	// Migrate gateway_url → endpoint if endpoint wasn't explicitly set
	if gw := viper.GetString("gateway_url"); gw != "" && !viper.IsSet("endpoint") {
		viper.Set("endpoint", gw)
	}

	// Write clean config directly, bypassing viper (which would keep old keys)
	clean := map[string]any{
		"endpoint":          viper.GetString("endpoint"),
		"api_key":           viper.GetString("api_key"),
		"model_tier":        viper.GetString("model_tier"),
		"auto_update_check": viper.GetBool("auto_update_check"),
	}

	dir := ShannonDir()
	if dir == "" {
		return
	}
	data, err := yaml.Marshal(clean)
	if err != nil {
		return
	}
	configPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(configPath, data, 0600)

	// Re-read so viper state matches the cleaned file
	viper.ReadInConfig()
}

func Save(cfg *Config) error {
	viper.Set("provider", cfg.Provider)
	viper.Set("endpoint", cfg.Endpoint)
	apiKey := strings.TrimSpace(cfg.APIKey)
	if keychain.Supported() && cfg.apiKeyFromKeychain {
		apiKey = ""
	}
	viper.Set("api_key", apiKey)
	viper.Set("model_tier", cfg.ModelTier)
	viper.Set("auto_update_check", cfg.AutoUpdateCheck)
	if cfg.Ollama.Endpoint != "" {
		viper.Set("ollama.endpoint", cfg.Ollama.Endpoint)
	}
	if cfg.Ollama.Model != "" {
		viper.Set("ollama.model", cfg.Ollama.Model)
	}
	return viper.WriteConfig()
}

// overlayConfig is a partial config used for YAML overlay merging.
// Pointer fields distinguish "not set" (nil) from "set to zero value".
type overlayConfig struct {
	Provider        *string                        `yaml:"provider"`
	Endpoint        *string                        `yaml:"endpoint"`
	APIKey          *string                        `yaml:"api_key"`
	ModelTier       *string                        `yaml:"model_tier"`
	AutoUpdateCheck *bool                          `yaml:"auto_update_check"`
	Ollama          *overlayOllamaConfig           `yaml:"ollama"`
	Permissions     *permissions.PermissionsConfig `yaml:"permissions"`
	Agent           *overlayAgentConfig            `yaml:"agent"`
	Tools           *overlayToolsConfig            `yaml:"tools"`
	Cloud           *overlayCloudConfig            `yaml:"cloud"`
	Daemon          *overlayDaemonConfig           `yaml:"daemon"`
	MCPServers      map[string]mcp.MCPServerConfig `yaml:"mcp_servers"`
	MCP             *overlayMCPConfig              `yaml:"mcp"`
	Memory          *overlayMemoryConfig           `yaml:"memory"`
}

type overlayCloudConfig struct {
	PublishAllowedExtensions []string `yaml:"publish_allowed_extensions"`
}

type overlayMCPConfig struct {
	WorkspaceRoots []string `yaml:"workspace_roots"`
}

type overlayOllamaConfig struct {
	Endpoint *string `yaml:"endpoint"`
	Model    *string `yaml:"model"`
}

type overlayDaemonConfig struct {
	AutoApprove *bool `yaml:"auto_approve"`
}

type overlayMemoryConfig struct {
	Provider               *string        `yaml:"provider"`
	SocketPath             *string        `yaml:"socket_path"`
	BundleRoot             *string        `yaml:"bundle_root"`
	TLMPath                *string        `yaml:"tlm_path"`
	BundlePullInterval     *time.Duration `yaml:"bundle_pull_interval"`
	BundlePullStartupDelay *time.Duration `yaml:"bundle_pull_startup_delay"`
	SidecarReadyTimeout    *time.Duration `yaml:"sidecar_ready_timeout"`
	SidecarShutdownGrace   *time.Duration `yaml:"sidecar_shutdown_grace"`
	SidecarRestartMax      *int           `yaml:"sidecar_restart_max"`
	ClientRequestTimeout   *time.Duration `yaml:"client_request_timeout"`
}

type overlayAgentConfig struct {
	MaxIterations          *int     `yaml:"max_iterations"`
	Temperature            *float64 `yaml:"temperature"`
	MaxTokens              *int     `yaml:"max_tokens"`
	Thinking               *bool    `yaml:"thinking"`
	ThinkingMode           *string  `yaml:"thinking_mode"`
	ThinkingBudget         *int     `yaml:"thinking_budget"`
	ForceThinkTool         *bool    `yaml:"force_think_tool"`
	ReasoningEffort        *string  `yaml:"reasoning_effort"`
	Model                  *string  `yaml:"model"`
	ContextWindow          *int     `yaml:"context_window"`
	ObservationWindow      *int     `yaml:"observation_window"`
	MaxRecentImages        *int     `yaml:"max_recent_images"`
	MaxRecentBrowserImages *int     `yaml:"max_recent_browser_images"`

	IdleSoftTimeoutSecs   *int  `yaml:"idle_soft_timeout_secs"`
	IdleHardTimeoutSecs   *int  `yaml:"idle_hard_timeout_secs"`
	StreamIdleTimeoutSecs *int  `yaml:"stream_idle_timeout_secs"`
	SkillDiscovery        *bool `yaml:"skill_discovery"`

	// BashConcurrencyEnabled is a pointer so unset overlays leave the value
	// alone — distinguishing "not specified" from "explicitly false".
	BashConcurrencyEnabled *bool `yaml:"bash_concurrency_enabled"`

	TimeBasedCompact *overlayTimeBasedCompactConfig `yaml:"time_based_compact"`

	PromptSuggestion *overlayPromptSuggestionConfig `yaml:"prompt_suggestion"`
}

type overlayTimeBasedCompactConfig struct {
	Enabled             *bool `yaml:"enabled"`
	GapThresholdMinutes *int  `yaml:"gap_threshold_minutes"`
	KeepRecent          *int  `yaml:"keep_recent"`
}

type overlayPromptSuggestionConfig struct {
	Enabled                  *bool `yaml:"enabled"`
	CacheColdThresholdTokens *int  `yaml:"cache_cold_threshold_tokens"`
	MinTurns                 *int  `yaml:"min_turns"`
}

type overlayToolsConfig struct {
	BashTimeout             *int `yaml:"bash_timeout"`
	BashMaxTimeout          *int `yaml:"bash_max_timeout"`
	BashMaxOutput           *int `yaml:"bash_max_output"`
	ResultTruncation        *int `yaml:"result_truncation"`
	ArgsTruncation          *int `yaml:"args_truncation"`
	ServerToolTimeout       *int `yaml:"server_tool_timeout"`
	BrowserResultTruncation *int `yaml:"browser_result_truncation"`
}

// buildDefaultSources returns source entries for all config keys set to "default".
func buildDefaultSources() map[string]ConfigSource {
	return map[string]ConfigSource{
		"endpoint":                        {Level: "default"},
		"api_key":                         {Level: "default"},
		"model_tier":                      {Level: "default"},
		"auto_update_check":               {Level: "default"},
		"agent.max_iterations":            {Level: "default"},
		"agent.temperature":               {Level: "default"},
		"agent.max_tokens":                {Level: "default"},
		"agent.thinking":                  {Level: "default"},
		"agent.thinking_mode":             {Level: "default"},
		"agent.thinking_budget":           {Level: "default"},
		"agent.force_think_tool":          {Level: "default"},
		"agent.reasoning_effort":          {Level: "default"},
		"agent.model":                     {Level: "default"},
		"agent.context_window":            {Level: "default"},
		"agent.observation_window":        {Level: "default"},
		"agent.max_recent_images":         {Level: "default"},
		"agent.max_recent_browser_images": {Level: "default"},
		"agent.idle_soft_timeout_secs":    {Level: "default"},
		"agent.idle_hard_timeout_secs":    {Level: "default"},
		"agent.stream_idle_timeout_secs":  {Level: "default"},
		"agent.bash_concurrency_enabled":  {Level: "default"},
		"tools.bash_timeout":              {Level: "default"},
		"tools.bash_max_timeout":          {Level: "default"},
		"tools.bash_max_output":           {Level: "default"},
		"tools.result_truncation":         {Level: "default"},
		"tools.browser_result_truncation": {Level: "default"},
		"tools.args_truncation":           {Level: "default"},
		"tools.server_tool_timeout":       {Level: "default"},
	}
}

// markGlobalSources marks keys that viper resolved from the global config file.
func markGlobalSources(cfg *Config, file string) {
	src := ConfigSource{File: file, Level: "global"}
	// Mark scalar fields that viper loaded (non-default values)
	if viper.IsSet("endpoint") {
		cfg.Sources["endpoint"] = src
	}
	if viper.IsSet("api_key") {
		cfg.Sources["api_key"] = src
	}
	if viper.IsSet("model_tier") {
		cfg.Sources["model_tier"] = src
	}
	if viper.IsSet("auto_update_check") {
		cfg.Sources["auto_update_check"] = src
	}
	if viper.IsSet("agent.max_iterations") {
		cfg.Sources["agent.max_iterations"] = src
	}
	if viper.IsSet("agent.temperature") {
		cfg.Sources["agent.temperature"] = src
	}
	if viper.IsSet("agent.max_tokens") {
		cfg.Sources["agent.max_tokens"] = src
	}
	if viper.IsSet("agent.thinking") {
		cfg.Sources["agent.thinking"] = src
	}
	if viper.IsSet("agent.thinking_mode") {
		cfg.Sources["agent.thinking_mode"] = src
	}
	if viper.IsSet("agent.thinking_budget") {
		cfg.Sources["agent.thinking_budget"] = src
	}
	if viper.IsSet("agent.force_think_tool") {
		cfg.Sources["agent.force_think_tool"] = src
	}
	if viper.IsSet("agent.reasoning_effort") {
		cfg.Sources["agent.reasoning_effort"] = src
	}
	if viper.IsSet("agent.model") {
		cfg.Sources["agent.model"] = src
	}
	if viper.IsSet("agent.context_window") {
		cfg.Sources["agent.context_window"] = src
	}
	if viper.IsSet("agent.observation_window") {
		cfg.Sources["agent.observation_window"] = src
	}
	if viper.IsSet("agent.max_recent_images") {
		cfg.Sources["agent.max_recent_images"] = src
	}
	if viper.IsSet("agent.max_recent_browser_images") {
		cfg.Sources["agent.max_recent_browser_images"] = src
	}
	if viper.IsSet("agent.idle_soft_timeout_secs") {
		cfg.Sources["agent.idle_soft_timeout_secs"] = src
	}
	if viper.IsSet("agent.idle_hard_timeout_secs") {
		cfg.Sources["agent.idle_hard_timeout_secs"] = src
	}
	if viper.IsSet("agent.stream_idle_timeout_secs") {
		cfg.Sources["agent.stream_idle_timeout_secs"] = src
	}
	if viper.IsSet("agent.bash_concurrency_enabled") {
		cfg.Sources["agent.bash_concurrency_enabled"] = src
	}
	if viper.IsSet("tools.bash_timeout") {
		cfg.Sources["tools.bash_timeout"] = src
	}
	if viper.IsSet("tools.bash_max_timeout") {
		cfg.Sources["tools.bash_max_timeout"] = src
	}
	if viper.IsSet("tools.bash_max_output") {
		cfg.Sources["tools.bash_max_output"] = src
	}
	if viper.IsSet("tools.result_truncation") {
		cfg.Sources["tools.result_truncation"] = src
	}
	if viper.IsSet("tools.browser_result_truncation") {
		cfg.Sources["tools.browser_result_truncation"] = src
	}
	if viper.IsSet("tools.args_truncation") {
		cfg.Sources["tools.args_truncation"] = src
	}
	if viper.IsSet("tools.server_tool_timeout") {
		cfg.Sources["tools.server_tool_timeout"] = src
	}
	// List fields from global
	if len(cfg.Permissions.AllowedDirs) > 0 {
		cfg.Sources["permissions.allowed_dirs"] = src
	}
	if len(cfg.Permissions.AllowedCommands) > 0 {
		cfg.Sources["permissions.allowed_commands"] = src
	}
	if len(cfg.Permissions.DeniedCommands) > 0 {
		cfg.Sources["permissions.denied_commands"] = src
	}
	if len(cfg.Permissions.SensitivePatterns) > 0 {
		cfg.Sources["permissions.sensitive_patterns"] = src
	}
	if len(cfg.Permissions.NetworkAllowlist) > 0 {
		cfg.Sources["permissions.network_allowlist"] = src
	}
}

// mergeOverlayFile reads a YAML file and merges it on top of cfg.
// mergeRuntimeOverlayFile merges session-safe fields from a project config
// overlay file. Process-global fields (endpoint, api_key, auto_update_check,
// daemon, mcp_servers) are intentionally skipped — they stay process-scoped.
// Scalars override; lists are merged and deduplicated.
func mergeRuntimeOverlayFile(cfg *Config, file string, level string) {
	if cfg == nil {
		return
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return // file doesn't exist or unreadable — skip silently
	}

	var overlay overlayConfig
	if err := yaml.Unmarshal(data, &overlay); err != nil {
		return // malformed — skip silently
	}

	src := ConfigSource{File: file, Level: level}

	// Scalar overrides
	if overlay.Provider != nil {
		cfg.Provider = *overlay.Provider
		cfg.Sources["provider"] = src
	}
	if overlay.ModelTier != nil {
		cfg.ModelTier = *overlay.ModelTier
		cfg.Sources["model_tier"] = src
	}

	// Ollama field-level merge
	if overlay.Ollama != nil {
		if overlay.Ollama.Endpoint != nil {
			cfg.Ollama.Endpoint = *overlay.Ollama.Endpoint
		}
		if overlay.Ollama.Model != nil {
			cfg.Ollama.Model = *overlay.Ollama.Model
		}
	}

	// Agent field-level merge
	if overlay.Agent != nil {
		if overlay.Agent.MaxIterations != nil {
			cfg.Agent.MaxIterations = *overlay.Agent.MaxIterations
			cfg.Sources["agent.max_iterations"] = src
		}
		if overlay.Agent.Temperature != nil {
			cfg.Agent.Temperature = *overlay.Agent.Temperature
			cfg.Sources["agent.temperature"] = src
		}
		if overlay.Agent.MaxTokens != nil {
			cfg.Agent.MaxTokens = *overlay.Agent.MaxTokens
			cfg.Sources["agent.max_tokens"] = src
		}
		if overlay.Agent.Thinking != nil {
			cfg.Agent.Thinking = *overlay.Agent.Thinking
			cfg.Sources["agent.thinking"] = src
		}
		if overlay.Agent.ThinkingMode != nil {
			cfg.Agent.ThinkingMode = *overlay.Agent.ThinkingMode
			cfg.Sources["agent.thinking_mode"] = src
		}
		if overlay.Agent.ThinkingBudget != nil {
			cfg.Agent.ThinkingBudget = *overlay.Agent.ThinkingBudget
			cfg.Sources["agent.thinking_budget"] = src
		}
		if overlay.Agent.ForceThinkTool != nil {
			cfg.Agent.ForceThinkTool = *overlay.Agent.ForceThinkTool
			cfg.Sources["agent.force_think_tool"] = src
		}
		if overlay.Agent.ReasoningEffort != nil {
			cfg.Agent.ReasoningEffort = *overlay.Agent.ReasoningEffort
			cfg.Sources["agent.reasoning_effort"] = src
		}
		if overlay.Agent.Model != nil {
			cfg.Agent.Model = *overlay.Agent.Model
			cfg.Sources["agent.model"] = src
		}
		if overlay.Agent.ContextWindow != nil {
			cfg.Agent.ContextWindow = *overlay.Agent.ContextWindow
			cfg.Sources["agent.context_window"] = src
		}
		if overlay.Agent.ObservationWindow != nil {
			cfg.Agent.ObservationWindow = *overlay.Agent.ObservationWindow
			cfg.Sources["agent.observation_window"] = src
		}
		if overlay.Agent.MaxRecentImages != nil {
			cfg.Agent.MaxRecentImages = *overlay.Agent.MaxRecentImages
			cfg.Sources["agent.max_recent_images"] = src
		}
		if overlay.Agent.MaxRecentBrowserImages != nil {
			cfg.Agent.MaxRecentBrowserImages = *overlay.Agent.MaxRecentBrowserImages
			cfg.Sources["agent.max_recent_browser_images"] = src
		}
		if overlay.Agent.IdleSoftTimeoutSecs != nil {
			cfg.Agent.IdleSoftTimeoutSecs = *overlay.Agent.IdleSoftTimeoutSecs
			cfg.Sources["agent.idle_soft_timeout_secs"] = src
		}
		if overlay.Agent.IdleHardTimeoutSecs != nil {
			cfg.Agent.IdleHardTimeoutSecs = *overlay.Agent.IdleHardTimeoutSecs
			cfg.Sources["agent.idle_hard_timeout_secs"] = src
		}
		if overlay.Agent.StreamIdleTimeoutSecs != nil {
			cfg.Agent.StreamIdleTimeoutSecs = *overlay.Agent.StreamIdleTimeoutSecs
			cfg.Sources["agent.stream_idle_timeout_secs"] = src
		}
		if overlay.Agent.SkillDiscovery != nil {
			cfg.Agent.SkillDiscovery = overlay.Agent.SkillDiscovery
			cfg.Sources["agent.skill_discovery"] = src
		}
		if overlay.Agent.BashConcurrencyEnabled != nil {
			cfg.Agent.BashConcurrencyEnabled = *overlay.Agent.BashConcurrencyEnabled
			cfg.Sources["agent.bash_concurrency_enabled"] = src
		}
		if overlay.Agent.TimeBasedCompact != nil {
			if overlay.Agent.TimeBasedCompact.Enabled != nil {
				cfg.Agent.TimeBasedCompact.Enabled = *overlay.Agent.TimeBasedCompact.Enabled
				cfg.Sources["agent.time_based_compact.enabled"] = src
			}
			if overlay.Agent.TimeBasedCompact.GapThresholdMinutes != nil {
				cfg.Agent.TimeBasedCompact.GapThresholdMinutes = *overlay.Agent.TimeBasedCompact.GapThresholdMinutes
				cfg.Sources["agent.time_based_compact.gap_threshold_minutes"] = src
			}
			if overlay.Agent.TimeBasedCompact.KeepRecent != nil {
				cfg.Agent.TimeBasedCompact.KeepRecent = *overlay.Agent.TimeBasedCompact.KeepRecent
				cfg.Sources["agent.time_based_compact.keep_recent"] = src
			}
		}
		if overlay.Agent.PromptSuggestion != nil {
			ps := overlay.Agent.PromptSuggestion
			if ps.Enabled != nil {
				cfg.Agent.PromptSuggestion.Enabled = *ps.Enabled
				cfg.Sources["agent.prompt_suggestion.enabled"] = src
			}
			if ps.CacheColdThresholdTokens != nil {
				cfg.Agent.PromptSuggestion.CacheColdThresholdTokens = *ps.CacheColdThresholdTokens
				cfg.Sources["agent.prompt_suggestion.cache_cold_threshold_tokens"] = src
			}
			if ps.MinTurns != nil {
				cfg.Agent.PromptSuggestion.MinTurns = *ps.MinTurns
				cfg.Sources["agent.prompt_suggestion.min_turns"] = src
			}
		}
	}

	// Tools field-level merge
	if overlay.Tools != nil {
		if overlay.Tools.BashTimeout != nil {
			cfg.Tools.BashTimeout = *overlay.Tools.BashTimeout
			cfg.Sources["tools.bash_timeout"] = src
		}
		if overlay.Tools.BashMaxTimeout != nil {
			cfg.Tools.BashMaxTimeout = *overlay.Tools.BashMaxTimeout
			cfg.Sources["tools.bash_max_timeout"] = src
		}
		if overlay.Tools.BashMaxOutput != nil {
			cfg.Tools.BashMaxOutput = *overlay.Tools.BashMaxOutput
			cfg.Sources["tools.bash_max_output"] = src
		}
		if overlay.Tools.ResultTruncation != nil {
			cfg.Tools.ResultTruncation = *overlay.Tools.ResultTruncation
			cfg.Sources["tools.result_truncation"] = src
		}
		if overlay.Tools.BrowserResultTruncation != nil {
			cfg.Tools.BrowserResultTruncation = *overlay.Tools.BrowserResultTruncation
			cfg.Sources["tools.browser_result_truncation"] = src
		}
		if overlay.Tools.ArgsTruncation != nil {
			cfg.Tools.ArgsTruncation = *overlay.Tools.ArgsTruncation
			cfg.Sources["tools.args_truncation"] = src
		}
		if overlay.Tools.ServerToolTimeout != nil {
			cfg.Tools.ServerToolTimeout = *overlay.Tools.ServerToolTimeout
			cfg.Sources["tools.server_tool_timeout"] = src
		}
	}

	// Cloud field-level merge. Only session-safe publish policy is accepted
	// from project/local overlays; endpoint, API key, and enablement stay global.
	if overlay.Cloud != nil && len(overlay.Cloud.PublishAllowedExtensions) > 0 {
		cfg.Cloud.PublishAllowedExtensions = dedup(append(cfg.Cloud.PublishAllowedExtensions, overlay.Cloud.PublishAllowedExtensions...))
		cfg.Sources["cloud.publish_allowed_extensions"] = src
	}

	// Permissions: merge and deduplicate lists
	if overlay.Permissions != nil {
		if len(overlay.Permissions.AllowedDirs) > 0 {
			cfg.Permissions.AllowedDirs = dedup(append(cfg.Permissions.AllowedDirs, overlay.Permissions.AllowedDirs...))
			cfg.Sources["permissions.allowed_dirs"] = src
		}
		if len(overlay.Permissions.AllowedCommands) > 0 {
			cfg.Permissions.AllowedCommands = dedup(append(cfg.Permissions.AllowedCommands, overlay.Permissions.AllowedCommands...))
			cfg.Sources["permissions.allowed_commands"] = src
		}
		if len(overlay.Permissions.DeniedCommands) > 0 {
			cfg.Permissions.DeniedCommands = dedup(append(cfg.Permissions.DeniedCommands, overlay.Permissions.DeniedCommands...))
			cfg.Sources["permissions.denied_commands"] = src
		}
		if len(overlay.Permissions.SensitivePatterns) > 0 {
			cfg.Permissions.SensitivePatterns = dedup(append(cfg.Permissions.SensitivePatterns, overlay.Permissions.SensitivePatterns...))
			cfg.Sources["permissions.sensitive_patterns"] = src
		}
		if len(overlay.Permissions.NetworkAllowlist) > 0 {
			cfg.Permissions.NetworkAllowlist = dedup(append(cfg.Permissions.NetworkAllowlist, overlay.Permissions.NetworkAllowlist...))
			cfg.Sources["permissions.network_allowlist"] = src
		}
	}

	// MCP: merge and deduplicate workspace roots. Project- or local-scoped
	// config can add extra directories on top of the global set without
	// replacing it.
	if overlay.MCP != nil && len(overlay.MCP.WorkspaceRoots) > 0 {
		cfg.MCP.WorkspaceRoots = dedup(append(cfg.MCP.WorkspaceRoots, overlay.MCP.WorkspaceRoots...))
		cfg.Sources["mcp.workspace_roots"] = src
	}

	// Memory attach settings are session-safe: CLI/TUI use them to decide
	// whether to attach to an already-running sidecar for this cwd. Cloud
	// endpoint/API key remain process-scoped and are intentionally not merged.
	if overlay.Memory != nil {
		if overlay.Memory.Provider != nil {
			cfg.Memory.Provider = *overlay.Memory.Provider
			cfg.Sources["memory.provider"] = src
		}
		if overlay.Memory.SocketPath != nil {
			cfg.Memory.SocketPath = *overlay.Memory.SocketPath
			cfg.Sources["memory.socket_path"] = src
		}
		if overlay.Memory.BundleRoot != nil {
			cfg.Memory.BundleRoot = *overlay.Memory.BundleRoot
			cfg.Sources["memory.bundle_root"] = src
		}
		if overlay.Memory.TLMPath != nil {
			cfg.Memory.TLMPath = *overlay.Memory.TLMPath
			cfg.Sources["memory.tlm_path"] = src
		}
		if overlay.Memory.BundlePullInterval != nil {
			cfg.Memory.BundlePullInterval = *overlay.Memory.BundlePullInterval
			cfg.Sources["memory.bundle_pull_interval"] = src
		}
		if overlay.Memory.BundlePullStartupDelay != nil {
			cfg.Memory.BundlePullStartupDelay = *overlay.Memory.BundlePullStartupDelay
			cfg.Sources["memory.bundle_pull_startup_delay"] = src
		}
		if overlay.Memory.SidecarReadyTimeout != nil {
			cfg.Memory.SidecarReadyTimeout = *overlay.Memory.SidecarReadyTimeout
			cfg.Sources["memory.sidecar_ready_timeout"] = src
		}
		if overlay.Memory.SidecarShutdownGrace != nil {
			cfg.Memory.SidecarShutdownGrace = *overlay.Memory.SidecarShutdownGrace
			cfg.Sources["memory.sidecar_shutdown_grace"] = src
		}
		if overlay.Memory.SidecarRestartMax != nil {
			cfg.Memory.SidecarRestartMax = *overlay.Memory.SidecarRestartMax
			cfg.Sources["memory.sidecar_restart_max"] = src
		}
		if overlay.Memory.ClientRequestTimeout != nil {
			cfg.Memory.ClientRequestTimeout = *overlay.Memory.ClientRequestTimeout
			cfg.Sources["memory.client_request_timeout"] = src
		}
	}

	// Process-global fields (endpoint, api_key, auto_update_check, daemon,
	// cloud enablement/timeout, mcp_servers) are intentionally NOT merged here —
	// they stay process-scoped.
}

func validateConfig(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	if cfg.Agent.Thinking {
		switch cfg.Agent.ThinkingMode {
		case "adaptive", "enabled":
			// valid
		default:
			return fmt.Errorf("invalid agent.thinking_mode %q: must be \"adaptive\" or \"enabled\"", cfg.Agent.ThinkingMode)
		}
	}
	// agent.model is a specific model id forwarded to the Gateway as
	// specific_model. A routing-tier word belongs in model_tier; if one lands
	// here it is sent verbatim and fails every run with "model_id_unknown".
	// Normalize case/whitespace — no real model id is a bare tier word, so this
	// only catches copy-paste/typo variants (`Large`, ` large`).
	switch strings.ToLower(strings.TrimSpace(cfg.Agent.Model)) {
	case "small", "medium", "large":
		return fmt.Errorf("agent.model expects a specific model id (e.g. \"claude-opus-4-8\"), not the tier %q; use model_tier for tiers", cfg.Agent.Model)
	}
	if cfg.Agent.IdleSoftTimeoutSecs < 0 {
		return fmt.Errorf("agent.idle_soft_timeout_secs (%d) must be >= 0 (0 = disabled)", cfg.Agent.IdleSoftTimeoutSecs)
	}
	if cfg.Agent.IdleHardTimeoutSecs < 0 {
		return fmt.Errorf("agent.idle_hard_timeout_secs (%d) must be >= 0 (0 = disabled)", cfg.Agent.IdleHardTimeoutSecs)
	}
	if cfg.Agent.IdleHardTimeoutSecs > 0 && cfg.Agent.IdleHardTimeoutSecs < 30 {
		return fmt.Errorf("agent.idle_hard_timeout_secs (%d) too aggressive; must be >= 30 or 0 to disable", cfg.Agent.IdleHardTimeoutSecs)
	}
	if cfg.Agent.IdleSoftTimeoutSecs > 0 && cfg.Agent.IdleHardTimeoutSecs > 0 &&
		cfg.Agent.IdleHardTimeoutSecs < cfg.Agent.IdleSoftTimeoutSecs {
		return fmt.Errorf("agent.idle_hard_timeout_secs (%d) must be >= agent.idle_soft_timeout_secs (%d)",
			cfg.Agent.IdleHardTimeoutSecs, cfg.Agent.IdleSoftTimeoutSecs)
	}
	if cfg.Agent.StreamIdleTimeoutSecs < 0 {
		return fmt.Errorf("agent.stream_idle_timeout_secs (%d) must be >= 0 (0 = disabled)", cfg.Agent.StreamIdleTimeoutSecs)
	}
	if cfg.Cloud.StreamIdleTimeoutSecs < 0 {
		return fmt.Errorf("cloud.stream_idle_timeout_secs (%d) must be >= 0 (0 = disabled)", cfg.Cloud.StreamIdleTimeoutSecs)
	}
	// Browser/GUI context-trimming knobs: 0 is a valid "disabled/fallback"
	// sentinel; negative is a typo that would silently disable the feature.
	if cfg.Agent.ObservationWindow < 0 {
		return fmt.Errorf("agent.observation_window (%d) must be >= 0 (0 = disabled)", cfg.Agent.ObservationWindow)
	}
	if cfg.Agent.MaxRecentImages < 0 {
		return fmt.Errorf("agent.max_recent_images (%d) must be >= 0 (0 = disabled)", cfg.Agent.MaxRecentImages)
	}
	if cfg.Agent.MaxRecentBrowserImages < 0 {
		return fmt.Errorf("agent.max_recent_browser_images (%d) must be >= 0 (0 = disabled)", cfg.Agent.MaxRecentBrowserImages)
	}
	if cfg.Tools.BrowserResultTruncation < 0 {
		return fmt.Errorf("tools.browser_result_truncation (%d) must be >= 0 (0 = fall back to result_truncation)", cfg.Tools.BrowserResultTruncation)
	}
	return nil
}

// mergeBuiltinMCPServers folds the in-binary BuiltinMCPServers catalog onto
// cfg.MCPServers using a field-level merge: command / args / type / url /
// context always come from the Go source (daemon owns them, upgrades pick
// up changes automatically), while disabled / env / keep_alive are taken
// from the user's yaml so their preferences persist. When the user has no
// entry for a built-in name yet, a fresh row is injected with disabled=true
// so first-launch shows the server as available-but-off.
//
// Must run AFTER fixMCPEnvKeyCasing so the env map written here is the
// case-preserved version, not viper's lowercased copy.
func mergeBuiltinMCPServers(cfg *Config) {
	if len(mcp.BuiltinMCPServers) == 0 {
		return
	}
	if cfg.MCPServers == nil {
		cfg.MCPServers = make(map[string]mcp.MCPServerConfig, len(mcp.BuiltinMCPServers))
	}
	for name, builtin := range mcp.BuiltinMCPServers {
		// Deep-copy Args + Env so a downstream mutation through cfg.MCPServers
		// can't reach into BuiltinMCPServers' backing storage. Type/URL/
		// Command/Context are value types and copy via struct assignment.
		merged := builtin.Config
		if len(builtin.Config.Args) > 0 {
			merged.Args = append([]string(nil), builtin.Config.Args...)
		}
		merged.Env = nil
		if len(builtin.Config.Env) > 0 {
			merged.Env = make(map[string]string, len(builtin.Config.Env))
			for k, v := range builtin.Config.Env {
				merged.Env[k] = v
			}
		}
		if existing, ok := cfg.MCPServers[name]; ok {
			// Preserve user-controlled fields. Default for Disabled when the
			// user has an entry is whatever they wrote (zero-value = false,
			// matching yaml semantics). ConnectTimeoutSeconds is also user-
			// tunable — keep the user override when non-zero, fall through
			// to the catalog default otherwise.
			//
			// Env merges key-by-key with user winning on conflicts: this
			// preserves any default env vars the catalog ships (currently
			// none, but a future builtin might) while still letting the
			// user override specific keys via yaml.
			merged.Disabled = existing.Disabled
			merged.KeepAlive = existing.KeepAlive
			if existing.ConnectTimeoutSeconds > 0 {
				merged.ConnectTimeoutSeconds = existing.ConnectTimeoutSeconds
			}
			for k, v := range existing.Env {
				if merged.Env == nil {
					merged.Env = make(map[string]string, len(existing.Env))
				}
				merged.Env[k] = v
			}
		} else {
			// First-launch default: shipped off until the user opts in.
			merged.Disabled = true
		}
		merged.Builtin = true
		cfg.MCPServers[name] = merged
	}
}

// fixMCPEnvKeyCasing re-reads MCP servers from YAML to restore env var key casing.
// Viper normalizes all map keys to lowercase, which breaks env vars (API_KEY → api_key).
func fixMCPEnvKeyCasing(cfg *Config, configPath string) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return
	}
	var raw struct {
		MCPServers map[string]mcp.MCPServerConfig `yaml:"mcp_servers"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return
	}
	for name, srv := range raw.MCPServers {
		if existing, ok := cfg.MCPServers[name]; ok && len(srv.Env) > 0 {
			existing.Env = srv.Env
			cfg.MCPServers[name] = existing
		}
	}
}

// AppendAllowedCommand adds a command pattern to permissions.allowed_commands
// in the config file at shannonDir/config.yaml. Skips if already present.
// Uses flock for concurrent write safety (matches schedules.json pattern).
func AppendAllowedCommand(shannonDir, pattern string) error {
	cfgPath := filepath.Join(shannonDir, "config.yaml")
	lockPath := cfgPath + ".lock"

	// Acquire exclusive lock on persistent lock file.
	// Do NOT delete the lock file — see schedule.go for rationale.
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open lock file: %w", err)
	}
	defer lockFile.Close()
	if err := fslock.Lock(lockFile.Fd()); err != nil {
		return fmt.Errorf("lock config: %w", err)
	}
	defer fslock.Unlock(lockFile.Fd())

	data, err := os.ReadFile(cfgPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read config: %w", err)
	}

	var raw map[string]interface{}
	if len(data) > 0 {
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("parse config: %w", err)
		}
	}
	if raw == nil {
		raw = make(map[string]interface{})
	}

	perms, _ := raw["permissions"].(map[string]interface{})
	if perms == nil {
		perms = make(map[string]interface{})
		raw["permissions"] = perms
	}

	var allowed []interface{}
	if existing, ok := perms["allowed_commands"].([]interface{}); ok {
		allowed = existing
	}

	for _, v := range allowed {
		if s, ok := v.(string); ok && s == pattern {
			return nil // already present
		}
	}

	allowed = append(allowed, pattern)
	perms["allowed_commands"] = allowed

	out, err := yaml.Marshal(raw)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	tmpPath := cfgPath + ".tmp"
	if err := os.WriteFile(tmpPath, out, 0600); err != nil {
		return fmt.Errorf("write temp: %w", err)
	}
	return os.Rename(tmpPath, cfgPath)
}

// AppendGlobalAlwaysAllowTool adds a tool name to the GLOBAL
// permissions.always_allow_tools list in shannonDir/config.yaml. Mirrors
// AppendAllowedCommand's flock + RMW + atomic-rename pattern; the same lock
// file (config.yaml.lock) is reused so concurrent writers across both
// allowlists serialize correctly. Idempotent (duplicate add is a no-op).
//
// Callers must reject high-risk tools (agent.DisallowsAutoApproval) BEFORE
// invoking this — this helper trusts its input. The runtime gate enforces
// the denylist independently as defense-in-depth.
func AppendGlobalAlwaysAllowTool(shannonDir, tool string) error {
	if tool == "" {
		return fmt.Errorf("tool name is empty")
	}
	cfgPath := filepath.Join(shannonDir, "config.yaml")
	lockPath := cfgPath + ".lock"

	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open lock file: %w", err)
	}
	defer lockFile.Close()
	if err := fslock.Lock(lockFile.Fd()); err != nil {
		return fmt.Errorf("lock config: %w", err)
	}
	defer fslock.Unlock(lockFile.Fd())

	data, err := os.ReadFile(cfgPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read config: %w", err)
	}

	var raw map[string]interface{}
	if len(data) > 0 {
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("parse config: %w", err)
		}
	}
	if raw == nil {
		raw = make(map[string]interface{})
	}

	perms, _ := raw["permissions"].(map[string]interface{})
	if perms == nil {
		perms = make(map[string]interface{})
		raw["permissions"] = perms
	}

	var existing []interface{}
	if v, ok := perms["always_allow_tools"].([]interface{}); ok {
		existing = v
	}
	for _, v := range existing {
		if s, ok := v.(string); ok && s == tool {
			return nil // already present
		}
	}

	existing = append(existing, tool)
	perms["always_allow_tools"] = existing

	out, err := yaml.Marshal(raw)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	tmpPath := cfgPath + ".tmp"
	if err := os.WriteFile(tmpPath, out, 0600); err != nil {
		return fmt.Errorf("write temp: %w", err)
	}
	return os.Rename(tmpPath, cfgPath)
}

// RemoveGlobalAlwaysAllowTool is the symmetric delete for
// AppendGlobalAlwaysAllowTool. No-op if the tool is not present, the list is
// empty, or config.yaml doesn't exist. Drops the always_allow_tools key when
// the list becomes empty to keep the YAML clean.
func RemoveGlobalAlwaysAllowTool(shannonDir, tool string) error {
	if tool == "" {
		return fmt.Errorf("tool name is empty")
	}
	cfgPath := filepath.Join(shannonDir, "config.yaml")
	lockPath := cfgPath + ".lock"

	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open lock file: %w", err)
	}
	defer lockFile.Close()
	if err := fslock.Lock(lockFile.Fd()); err != nil {
		return fmt.Errorf("lock config: %w", err)
	}
	defer fslock.Unlock(lockFile.Fd())

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read config: %w", err)
	}

	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if raw == nil {
		return nil
	}
	perms, _ := raw["permissions"].(map[string]interface{})
	if perms == nil {
		return nil
	}
	existing, _ := perms["always_allow_tools"].([]interface{})
	if len(existing) == 0 {
		return nil
	}
	filtered := make([]interface{}, 0, len(existing))
	removed := false
	for _, v := range existing {
		if s, ok := v.(string); ok && s == tool {
			removed = true
			continue
		}
		filtered = append(filtered, v)
	}
	if !removed {
		return nil
	}
	if len(filtered) == 0 {
		delete(perms, "always_allow_tools")
	} else {
		perms["always_allow_tools"] = filtered
	}

	out, err := yaml.Marshal(raw)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	tmpPath := cfgPath + ".tmp"
	if err := os.WriteFile(tmpPath, out, 0600); err != nil {
		return fmt.Errorf("write temp: %w", err)
	}
	return os.Rename(tmpPath, cfgPath)
}

// AppendGlobalDisabledSkill adds a skill identifier (Name or Slug) to the global
// config.yaml skills.disabled list, so the DEFAULT agent stops loading it. Named
// agents are unaffected (they select skills via _attached.yaml). Idempotent;
// creates config.yaml if absent. Mirrors AppendGlobalAlwaysAllowTool: flock +
// read-modify-write on the raw YAML so unrelated keys (and casing) survive.
func AppendGlobalDisabledSkill(shannonDir, skill string) error {
	if skill == "" {
		return fmt.Errorf("skill name is empty")
	}
	return AppendGlobalDisabledSkills(shannonDir, []string{skill})
}

// AppendGlobalDisabledSkills adds one or more skills to config.skills.disabled
// in a SINGLE flock + read-modify-write, deduping against existing entries and
// within the batch. Empty names are skipped; empty/all-present input is a no-op.
// The batch form exists so an agent disabling a large skill family (e.g. a 100+
// longbridge-* set) writes config once instead of once-per-skill — the
// per-skill path caused a 126-call http spin that bloated context and tripped
// the loop detector.
func AppendGlobalDisabledSkills(shannonDir string, skills []string) error {
	want := make([]string, 0, len(skills))
	for _, s := range skills {
		if s != "" {
			want = append(want, s)
		}
	}
	if len(want) == 0 {
		return nil
	}
	cfgPath := filepath.Join(shannonDir, "config.yaml")
	lockPath := cfgPath + ".lock"

	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open lock file: %w", err)
	}
	defer lockFile.Close()
	if err := fslock.Lock(lockFile.Fd()); err != nil {
		return fmt.Errorf("lock config: %w", err)
	}
	defer fslock.Unlock(lockFile.Fd())

	data, err := os.ReadFile(cfgPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read config: %w", err)
	}

	var raw map[string]interface{}
	if len(data) > 0 {
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("parse config: %w", err)
		}
	}
	if raw == nil {
		raw = make(map[string]interface{})
	}

	sk, _ := raw["skills"].(map[string]interface{})
	if sk == nil {
		sk = make(map[string]interface{})
		raw["skills"] = sk
	}

	var existing []interface{}
	if v, ok := sk["disabled"].([]interface{}); ok {
		existing = v
	}
	have := make(map[string]bool, len(existing))
	for _, v := range existing {
		if s, ok := v.(string); ok {
			have[s] = true
		}
	}

	changed := false
	for _, s := range want {
		if !have[s] {
			existing = append(existing, s)
			have[s] = true
			changed = true
		}
	}
	if !changed {
		return nil // all already present
	}
	sk["disabled"] = existing

	out, err := yaml.Marshal(raw)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	tmpPath := cfgPath + ".tmp"
	if err := os.WriteFile(tmpPath, out, 0600); err != nil {
		return fmt.Errorf("write temp: %w", err)
	}
	return os.Rename(tmpPath, cfgPath)
}

// RemoveGlobalDisabledSkill is the symmetric delete for
// AppendGlobalDisabledSkill. No-op if the skill is absent, the list is empty, or
// config.yaml doesn't exist. Drops the skills.disabled key (and the skills block
// if it becomes empty) to keep the YAML clean.
func RemoveGlobalDisabledSkill(shannonDir, skill string) error {
	if skill == "" {
		return fmt.Errorf("skill name is empty")
	}
	return RemoveGlobalDisabledSkills(shannonDir, []string{skill})
}

// RemoveGlobalDisabledSkills removes one or more skills from
// config.skills.disabled in a SINGLE flock, re-enabling them for the default
// agent. No-op for absent names / empty input. Drops the disabled key (and the
// skills block) when it becomes empty. Symmetric to AppendGlobalDisabledSkills.
func RemoveGlobalDisabledSkills(shannonDir string, skills []string) error {
	toRemove := make(map[string]bool, len(skills))
	for _, s := range skills {
		if s != "" {
			toRemove[s] = true
		}
	}
	if len(toRemove) == 0 {
		return nil
	}
	cfgPath := filepath.Join(shannonDir, "config.yaml")
	lockPath := cfgPath + ".lock"

	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open lock file: %w", err)
	}
	defer lockFile.Close()
	if err := fslock.Lock(lockFile.Fd()); err != nil {
		return fmt.Errorf("lock config: %w", err)
	}
	defer fslock.Unlock(lockFile.Fd())

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read config: %w", err)
	}

	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if raw == nil {
		return nil
	}
	sk, _ := raw["skills"].(map[string]interface{})
	if sk == nil {
		return nil
	}
	existing, _ := sk["disabled"].([]interface{})
	if len(existing) == 0 {
		return nil
	}
	filtered := make([]interface{}, 0, len(existing))
	removed := false
	for _, v := range existing {
		if s, ok := v.(string); ok && toRemove[s] {
			removed = true
			continue
		}
		filtered = append(filtered, v)
	}
	if !removed {
		return nil
	}
	if len(filtered) == 0 {
		delete(sk, "disabled")
		if len(sk) == 0 {
			delete(raw, "skills")
		}
	} else {
		sk["disabled"] = filtered
	}

	out, err := yaml.Marshal(raw)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	tmpPath := cfgPath + ".tmp"
	if err := os.WriteFile(tmpPath, out, 0600); err != nil {
		return fmt.Errorf("write temp: %w", err)
	}
	return os.Rename(tmpPath, cfgPath)
}

// AppendDefaultAgentDisabledMCPServer adds an MCP server name to the global
// config.mcp.default_agent_disabled list so the DEFAULT agent stops using it.
// Named agents are unaffected (they select servers via per-agent mcp_servers).
// Idempotent; creates config.yaml if absent. Mirrors AppendGlobalDisabledSkill.
func AppendDefaultAgentDisabledMCPServer(shannonDir, server string) error {
	if server == "" {
		return fmt.Errorf("server name is empty")
	}
	cfgPath := filepath.Join(shannonDir, "config.yaml")
	lockPath := cfgPath + ".lock"

	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open lock file: %w", err)
	}
	defer lockFile.Close()
	if err := fslock.Lock(lockFile.Fd()); err != nil {
		return fmt.Errorf("lock config: %w", err)
	}
	defer fslock.Unlock(lockFile.Fd())

	data, err := os.ReadFile(cfgPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read config: %w", err)
	}

	var raw map[string]interface{}
	if len(data) > 0 {
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("parse config: %w", err)
		}
	}
	if raw == nil {
		raw = make(map[string]interface{})
	}

	mcpBlk, _ := raw["mcp"].(map[string]interface{})
	if mcpBlk == nil {
		mcpBlk = make(map[string]interface{})
		raw["mcp"] = mcpBlk
	}

	var existing []interface{}
	if v, ok := mcpBlk["default_agent_disabled"].([]interface{}); ok {
		existing = v
	}
	for _, v := range existing {
		if s, ok := v.(string); ok && s == server {
			return nil // already present
		}
	}

	existing = append(existing, server)
	mcpBlk["default_agent_disabled"] = existing

	out, err := yaml.Marshal(raw)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	tmpPath := cfgPath + ".tmp"
	if err := os.WriteFile(tmpPath, out, 0600); err != nil {
		return fmt.Errorf("write temp: %w", err)
	}
	return os.Rename(tmpPath, cfgPath)
}

// RemoveDefaultAgentDisabledMCPServer is the symmetric delete. No-op if the
// server is absent, the list is empty, or config.yaml doesn't exist. Drops the
// mcp.default_agent_disabled key (and the mcp block if it becomes empty).
func RemoveDefaultAgentDisabledMCPServer(shannonDir, server string) error {
	if server == "" {
		return fmt.Errorf("server name is empty")
	}
	cfgPath := filepath.Join(shannonDir, "config.yaml")
	lockPath := cfgPath + ".lock"

	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open lock file: %w", err)
	}
	defer lockFile.Close()
	if err := fslock.Lock(lockFile.Fd()); err != nil {
		return fmt.Errorf("lock config: %w", err)
	}
	defer fslock.Unlock(lockFile.Fd())

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read config: %w", err)
	}

	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if raw == nil {
		return nil
	}
	mcpBlk, _ := raw["mcp"].(map[string]interface{})
	if mcpBlk == nil {
		return nil
	}
	existing, _ := mcpBlk["default_agent_disabled"].([]interface{})
	if len(existing) == 0 {
		return nil
	}
	filtered := make([]interface{}, 0, len(existing))
	removed := false
	for _, v := range existing {
		if s, ok := v.(string); ok && s == server {
			removed = true
			continue
		}
		filtered = append(filtered, v)
	}
	if !removed {
		return nil
	}
	if len(filtered) == 0 {
		delete(mcpBlk, "default_agent_disabled")
		if len(mcpBlk) == 0 {
			delete(raw, "mcp")
		}
	} else {
		mcpBlk["default_agent_disabled"] = filtered
	}

	out, err := yaml.Marshal(raw)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	tmpPath := cfgPath + ".tmp"
	if err := os.WriteFile(tmpPath, out, 0600); err != nil {
		return fmt.Errorf("write temp: %w", err)
	}
	return os.Rename(tmpPath, cfgPath)
}

// dedup returns a slice with duplicate strings removed, preserving order.
func dedup(items []string) []string {
	seen := make(map[string]bool, len(items))
	result := make([]string, 0, len(items))
	for _, item := range items {
		if !seen[item] {
			seen[item] = true
			result = append(result, item)
		}
	}
	return result
}
