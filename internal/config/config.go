package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/hooks"
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
	Provider        string                         `mapstructure:"provider"          yaml:"provider"          json:"provider"` // "gateway" (default) or "ollama"
	Endpoint        string                         `mapstructure:"endpoint"          yaml:"endpoint"          json:"endpoint"`
	APIKey          string                         `mapstructure:"api_key"           yaml:"api_key"           json:"api_key"`
	ModelTier       string                         `mapstructure:"model_tier"        yaml:"model_tier"        json:"model_tier"`
	Ollama          OllamaConfig                   `mapstructure:"ollama"            yaml:"ollama"            json:"ollama"`
	AutoUpdateCheck bool                           `mapstructure:"auto_update_check" yaml:"auto_update_check" json:"auto_update_check"`
	Permissions     permissions.PermissionsConfig  `mapstructure:"permissions"       yaml:"permissions"       json:"permissions"`
	Agent           AgentConfig                    `mapstructure:"agent"             yaml:"agent"             json:"agent"`
	Tools           ToolsConfig                    `mapstructure:"tools"             yaml:"tools"             json:"tools"`
	Cloud           CloudConfig                    `mapstructure:"cloud"             yaml:"cloud"             json:"cloud"`
	Daemon          DaemonConfig                   `mapstructure:"daemon"            yaml:"daemon"            json:"daemon"`
	Skills          SkillsConfig                   `mapstructure:"skills"            yaml:"skills"            json:"skills"`
	Memory          MemoryConfig                   `mapstructure:"memory"            yaml:"memory"            json:"memory"`
	Hooks           hooks.HookConfig               `mapstructure:"hooks"             yaml:"hooks"             json:"hooks"`
	MCPServers      map[string]mcp.MCPServerConfig `mapstructure:"mcp_servers"       yaml:"mcp_servers"       json:"mcp_servers"`
	MCP             MCPConfig                      `mapstructure:"mcp"               yaml:"mcp"               json:"mcp"`
	Sources         map[string]ConfigSource        `mapstructure:"-"                 yaml:"-"                 json:"-"`
}

// MCPConfig holds client-side settings shared across all MCP servers.
type MCPConfig struct {
	// WorkspaceRoots are extra directories advertised to MCP servers honoring
	// the client `roots` capability, on top of the Kocoro defaults
	// (attachment staging dir plus common user locations). Intended for
	// project directories the user wants browser_file_upload or similar
	// sandboxed tools to reach without manual copying.
	WorkspaceRoots []string `mapstructure:"workspace_roots" yaml:"workspace_roots,omitempty" json:"workspace_roots,omitempty"`
}

type AgentConfig struct {
	MaxIterations     int     `mapstructure:"max_iterations"   yaml:"max_iterations"   json:"max_iterations"`
	Temperature       float64 `mapstructure:"temperature"      yaml:"temperature"      json:"temperature"`
	MaxTokens         int     `mapstructure:"max_tokens"       yaml:"max_tokens"       json:"max_tokens"`
	Thinking          bool    `mapstructure:"thinking"         yaml:"thinking"         json:"thinking"`
	ThinkingMode      string  `mapstructure:"thinking_mode"    yaml:"thinking_mode"    json:"thinking_mode"` // "adaptive" (default) or "enabled" (fixed budget)
	ThinkingBudget    int     `mapstructure:"thinking_budget"  yaml:"thinking_budget"  json:"thinking_budget"`
	ReasoningEffort   string  `mapstructure:"reasoning_effort" yaml:"reasoning_effort" json:"reasoning_effort"`
	Model             string  `mapstructure:"model"            yaml:"model"            json:"model"`                     // specific model override
	ContextWindow     int     `mapstructure:"context_window"   yaml:"context_window"   json:"context_window"`            // model context window in tokens
	// IdleSoftTimeoutSecs / IdleHardTimeoutSecs: turn-level watchdog measured
	// against explicit "idle-counted" phases of the agent loop (waiting on an
	// LLM response). Other phases (tool execution, approval wait, compaction
	// wrappers) are not counted — they have their own bounded owners.
	//
	// IdleSoftTimeoutSecs: emit OnRunStatus("idle_soft", …) after this long
	// in an idle-counted phase. 0 = disabled. Default: 90.
	// IdleHardTimeoutSecs: cancel the run with ErrHardIdleTimeout after this
	// long. 0 = disabled (visibility-only mode). Default: 0 while the
	// watchdog is opt-in; will flip to a value strictly less than the
	// gateway transport ceiling (600s) after dogfood.
	IdleSoftTimeoutSecs int   `mapstructure:"idle_soft_timeout_secs" yaml:"idle_soft_timeout_secs" json:"idle_soft_timeout_secs"`
	IdleHardTimeoutSecs int   `mapstructure:"idle_hard_timeout_secs" yaml:"idle_hard_timeout_secs" json:"idle_hard_timeout_secs"`
	SkillDiscovery      *bool `mapstructure:"skill_discovery" yaml:"skill_discovery,omitempty" json:"skill_discovery,omitempty"`

	// TimeBasedCompact controls time-gated tool_result clearing. Disabled
	// by default. When enabled, an old tool_result is cleared to a short
	// marker only after the gap since the last assistant response exceeds
	// GapThresholdMinutes — i.e. only when Anthropic's prompt cache has
	// reliably expired so the prefix would be rewritten anyway. See
	// internal/agent/timebasedcompact.go.
	TimeBasedCompact TimeBasedCompactConfig `mapstructure:"time_based_compact" yaml:"time_based_compact" json:"time_based_compact"`
}

// TimeBasedCompactConfig is the YAML/JSON-bindable view of the agent's
// time-based microcompact policy. The runtime view lives in
// internal/agent.TimeBasedCompactConfig — they are kept in lockstep.
type TimeBasedCompactConfig struct {
	Enabled             bool `mapstructure:"enabled"               yaml:"enabled"               json:"enabled"`
	GapThresholdMinutes int  `mapstructure:"gap_threshold_minutes" yaml:"gap_threshold_minutes" json:"gap_threshold_minutes"`
	KeepRecent          int  `mapstructure:"keep_recent"           yaml:"keep_recent"           json:"keep_recent"`
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
	BashMaxOutput     int `mapstructure:"bash_max_output"     yaml:"bash_max_output"     json:"bash_max_output"`
	ResultTruncation  int `mapstructure:"result_truncation"   yaml:"result_truncation"   json:"result_truncation"`
	ArgsTruncation    int `mapstructure:"args_truncation"     yaml:"args_truncation"     json:"args_truncation"`
	ServerToolTimeout int `mapstructure:"server_tool_timeout" yaml:"server_tool_timeout" json:"server_tool_timeout"`
	GrepMaxResults    int `mapstructure:"grep_max_results"    yaml:"grep_max_results"    json:"grep_max_results"`
}

type CloudConfig struct {
	Enabled bool `mapstructure:"enabled" yaml:"enabled" json:"enabled"`
	Timeout int  `mapstructure:"timeout" yaml:"timeout" json:"timeout"` // seconds
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
}

type SkillsConfig struct {
	Marketplace MarketplaceConfig `mapstructure:"marketplace" yaml:"marketplace" json:"marketplace"`
}

type MarketplaceConfig struct {
	RegistryURL string `mapstructure:"registry_url" yaml:"registry_url" json:"registry_url"`
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
	viper.SetDefault("agent.max_iterations", 25)
	viper.SetDefault("agent.temperature", 0)
	viper.SetDefault("agent.max_tokens", 32000)
	viper.SetDefault("agent.thinking", true)
	viper.SetDefault("agent.thinking_mode", "adaptive")
	viper.SetDefault("agent.thinking_budget", 10000)
	viper.SetDefault("agent.reasoning_effort", "")
	viper.SetDefault("agent.model", "")
	viper.SetDefault("agent.context_window", 200000)
	viper.SetDefault("agent.idle_soft_timeout_secs", 90)
	viper.SetDefault("agent.idle_hard_timeout_secs", 0) // 0 = disabled; flip to <600 after dogfood
	// Time-based microcompact. Disabled by default — short sessions never
	// compact, and only sessions that idle past the gap threshold will
	// clear old tool_results. 60min matches Anthropic's 1h prompt-cache
	// TTL ceiling. KeepRecent=5 keeps a working tail visible to the model.
	viper.SetDefault("agent.time_based_compact.enabled", false)
	viper.SetDefault("agent.time_based_compact.gap_threshold_minutes", 60)
	viper.SetDefault("agent.time_based_compact.keep_recent", 5)
	viper.SetDefault("tools.bash_timeout", 120)
	viper.SetDefault("tools.bash_max_output", 30000)
	viper.SetDefault("tools.result_truncation", 30000)
	viper.SetDefault("tools.args_truncation", 200)
	viper.SetDefault("tools.server_tool_timeout", 5)
	viper.SetDefault("tools.grep_max_results", 100)
	viper.SetDefault("daemon.auto_approve", false)
	viper.SetDefault("skills.marketplace.registry_url", "https://raw.githubusercontent.com/Kocoro-lab/shanclaw-skill-registry/main/index.json")
	viper.SetDefault("cloud.enabled", true)
	viper.SetDefault("cloud.timeout", 3600)

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

	// Re-read MCP servers directly from YAML to preserve env var key casing.
	// Viper lowercases all map keys which breaks env vars like API_KEY → api_key.
	globalFile := filepath.Join(dir, "config.yaml")
	fixMCPEnvKeyCasing(&cfg, globalFile)
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
	viper.Set("api_key", strings.TrimSpace(cfg.APIKey))
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
	MaxIterations     *int     `yaml:"max_iterations"`
	Temperature       *float64 `yaml:"temperature"`
	MaxTokens         *int     `yaml:"max_tokens"`
	Thinking          *bool    `yaml:"thinking"`
	ThinkingMode      *string  `yaml:"thinking_mode"`
	ThinkingBudget    *int     `yaml:"thinking_budget"`
	ReasoningEffort   *string  `yaml:"reasoning_effort"`
	Model             *string  `yaml:"model"`
	ContextWindow     *int     `yaml:"context_window"`

	IdleSoftTimeoutSecs *int  `yaml:"idle_soft_timeout_secs"`
	IdleHardTimeoutSecs *int  `yaml:"idle_hard_timeout_secs"`
	SkillDiscovery      *bool `yaml:"skill_discovery"`

	TimeBasedCompact *overlayTimeBasedCompactConfig `yaml:"time_based_compact"`
}

type overlayTimeBasedCompactConfig struct {
	Enabled             *bool `yaml:"enabled"`
	GapThresholdMinutes *int  `yaml:"gap_threshold_minutes"`
	KeepRecent          *int  `yaml:"keep_recent"`
}

type overlayToolsConfig struct {
	BashTimeout       *int `yaml:"bash_timeout"`
	BashMaxOutput     *int `yaml:"bash_max_output"`
	ResultTruncation  *int `yaml:"result_truncation"`
	ArgsTruncation    *int `yaml:"args_truncation"`
	ServerToolTimeout *int `yaml:"server_tool_timeout"`
	GrepMaxResults    *int `yaml:"grep_max_results"`
}

// buildDefaultSources returns source entries for all config keys set to "default".
func buildDefaultSources() map[string]ConfigSource {
	return map[string]ConfigSource{
		"endpoint":                     {Level: "default"},
		"api_key":                      {Level: "default"},
		"model_tier":                   {Level: "default"},
		"auto_update_check":            {Level: "default"},
		"agent.max_iterations":         {Level: "default"},
		"agent.temperature":            {Level: "default"},
		"agent.max_tokens":             {Level: "default"},
		"agent.thinking":               {Level: "default"},
		"agent.thinking_mode":          {Level: "default"},
		"agent.thinking_budget":        {Level: "default"},
		"agent.reasoning_effort":       {Level: "default"},
		"agent.model":                  {Level: "default"},
		"agent.context_window":         {Level: "default"},
		"agent.idle_soft_timeout_secs": {Level: "default"},
		"agent.idle_hard_timeout_secs": {Level: "default"},
		"tools.bash_timeout":           {Level: "default"},
		"tools.bash_max_output":        {Level: "default"},
		"tools.result_truncation":      {Level: "default"},
		"tools.args_truncation":        {Level: "default"},
		"tools.server_tool_timeout":    {Level: "default"},
		"tools.grep_max_results":       {Level: "default"},
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
	if viper.IsSet("agent.reasoning_effort") {
		cfg.Sources["agent.reasoning_effort"] = src
	}
	if viper.IsSet("agent.model") {
		cfg.Sources["agent.model"] = src
	}
	if viper.IsSet("agent.context_window") {
		cfg.Sources["agent.context_window"] = src
	}
	if viper.IsSet("agent.idle_soft_timeout_secs") {
		cfg.Sources["agent.idle_soft_timeout_secs"] = src
	}
	if viper.IsSet("agent.idle_hard_timeout_secs") {
		cfg.Sources["agent.idle_hard_timeout_secs"] = src
	}
	if viper.IsSet("tools.bash_timeout") {
		cfg.Sources["tools.bash_timeout"] = src
	}
	if viper.IsSet("tools.bash_max_output") {
		cfg.Sources["tools.bash_max_output"] = src
	}
	if viper.IsSet("tools.result_truncation") {
		cfg.Sources["tools.result_truncation"] = src
	}
	if viper.IsSet("tools.args_truncation") {
		cfg.Sources["tools.args_truncation"] = src
	}
	if viper.IsSet("tools.server_tool_timeout") {
		cfg.Sources["tools.server_tool_timeout"] = src
	}
	if viper.IsSet("tools.grep_max_results") {
		cfg.Sources["tools.grep_max_results"] = src
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
		if overlay.Agent.IdleSoftTimeoutSecs != nil {
			cfg.Agent.IdleSoftTimeoutSecs = *overlay.Agent.IdleSoftTimeoutSecs
			cfg.Sources["agent.idle_soft_timeout_secs"] = src
		}
		if overlay.Agent.IdleHardTimeoutSecs != nil {
			cfg.Agent.IdleHardTimeoutSecs = *overlay.Agent.IdleHardTimeoutSecs
			cfg.Sources["agent.idle_hard_timeout_secs"] = src
		}
		if overlay.Agent.SkillDiscovery != nil {
			cfg.Agent.SkillDiscovery = overlay.Agent.SkillDiscovery
			cfg.Sources["agent.skill_discovery"] = src
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
	}

	// Tools field-level merge
	if overlay.Tools != nil {
		if overlay.Tools.BashTimeout != nil {
			cfg.Tools.BashTimeout = *overlay.Tools.BashTimeout
			cfg.Sources["tools.bash_timeout"] = src
		}
		if overlay.Tools.BashMaxOutput != nil {
			cfg.Tools.BashMaxOutput = *overlay.Tools.BashMaxOutput
			cfg.Sources["tools.bash_max_output"] = src
		}
		if overlay.Tools.ResultTruncation != nil {
			cfg.Tools.ResultTruncation = *overlay.Tools.ResultTruncation
			cfg.Sources["tools.result_truncation"] = src
		}
		if overlay.Tools.ArgsTruncation != nil {
			cfg.Tools.ArgsTruncation = *overlay.Tools.ArgsTruncation
			cfg.Sources["tools.args_truncation"] = src
		}
		if overlay.Tools.ServerToolTimeout != nil {
			cfg.Tools.ServerToolTimeout = *overlay.Tools.ServerToolTimeout
			cfg.Sources["tools.server_tool_timeout"] = src
		}
		if overlay.Tools.GrepMaxResults != nil {
			cfg.Tools.GrepMaxResults = *overlay.Tools.GrepMaxResults
			cfg.Sources["tools.grep_max_results"] = src
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
	return nil
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
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("flock: %w", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

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
