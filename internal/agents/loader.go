package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
)

var agentNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
var mentionRe = regexp.MustCompile(`^@([a-zA-Z0-9][a-zA-Z0-9_-]*)(?:\s|$)`)

// AgentToolsFilter controls which local tools an agent can access.
// If Allow is non-empty, only those tools are available.
// If Deny is non-empty, all tools except those are available.
// If both are empty, all tools are available (backwards-compatible).
type AgentToolsFilter struct {
	Allow []string `yaml:"allow,omitempty" json:"allow,omitempty"`
	Deny  []string `yaml:"deny,omitempty" json:"deny,omitempty"`
}

// AgentMCPConfig holds MCP server configs with an optional inherit flag.
// When Inherit is false (default), only the servers listed here are used.
// When Inherit is true, these servers are merged on top of the global set.
// This struct is populated programmatically by parseAgentConfig, not by
// direct YAML unmarshaling.
type AgentMCPConfig struct {
	Inherit bool
	Servers map[string]AgentMCPServerRef
}

// AgentMCPServerRef mirrors the fields needed for per-agent MCP server config.
// We keep it simple — the full MCPServerConfig is resolved at merge time.
type AgentMCPServerRef struct {
	Command   string            `yaml:"command" json:"command,omitempty"`
	Args      []string          `yaml:"args,omitempty" json:"args,omitempty"`
	Env       map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Type      string            `yaml:"type,omitempty" json:"type,omitempty"`
	URL       string            `yaml:"url,omitempty" json:"url,omitempty"`
	Disabled  bool              `yaml:"disabled,omitempty" json:"disabled,omitempty"`
	Context   string            `yaml:"context,omitempty" json:"context,omitempty"`
	KeepAlive bool              `yaml:"keep_alive,omitempty" json:"keep_alive,omitempty"`
}

// WatchEntry defines a single file system watch path for an agent.
type WatchEntry struct {
	Path string `yaml:"path" json:"path"`
	Glob string `yaml:"glob,omitempty" json:"glob,omitempty"`
}

// HeartbeatConfig configures periodic heartbeat checks for an agent.
// IsolatedSession defaults to true (nil = true). Use pointer for YAML omit-means-default.
type HeartbeatConfig struct {
	Every           string `yaml:"every" json:"every"`
	ActiveHours     string `yaml:"active_hours,omitempty" json:"active_hours,omitempty"`
	Model           string `yaml:"model,omitempty" json:"model,omitempty"`
	IsolatedSession *bool  `yaml:"isolated_session,omitempty" json:"isolated_session,omitempty"`
}

// IsIsolatedSession returns the effective value (default true).
func (h *HeartbeatConfig) IsIsolatedSession() bool {
	if h.IsolatedSession == nil {
		return true
	}
	return *h.IsolatedSession
}

// AgentConfig is the per-agent config overlay loaded from config.yaml.
type AgentConfig struct {
	CWD         string            `yaml:"cwd"`
	MCPServers  *AgentMCPConfig   `yaml:"-"` // parsed manually for _inherit
	Tools       *AgentToolsFilter `yaml:"tools"`
	Agent       *AgentModelConfig `yaml:"agent"`
	AutoApprove *bool             `yaml:"auto_approve"`
	Watch       []WatchEntry      `yaml:"watch,omitempty"`
	Heartbeat   *HeartbeatConfig  `yaml:"heartbeat,omitempty"`
}

// AgentModelConfig holds per-agent model/iteration overrides.
type AgentModelConfig struct {
	Model         *string  `yaml:"model" json:"model,omitempty"`
	MaxIterations *int     `yaml:"max_iterations" json:"max_iterations,omitempty"`
	Temperature   *float64 `yaml:"temperature" json:"temperature,omitempty"`
	MaxTokens     *int     `yaml:"max_tokens" json:"max_tokens,omitempty"`
	ContextWindow *int     `yaml:"context_window" json:"context_window,omitempty"`

	IdleSoftTimeoutSecs *int `yaml:"idle_soft_timeout_secs" json:"idle_soft_timeout_secs,omitempty"`
	IdleHardTimeoutSecs *int `yaml:"idle_hard_timeout_secs" json:"idle_hard_timeout_secs,omitempty"`
}

// Agent represents a loaded agent definition.
type Agent struct {
	Name     string
	Prompt   string
	Memory   string
	Config   *AgentConfig      // nil = inherit everything (backwards-compatible)
	Commands map[string]string // agent-scoped slash commands (name → content)
	Skills   []*skills.Skill   // agent-scoped skills (prompt, tool_chain, sub_agent)
}

func ValidateAgentName(name string) error {
	if !agentNameRe.MatchString(name) {
		return fmt.Errorf("invalid agent name %q: must match %s", name, agentNameRe.String())
	}
	return nil
}

func LoadAgent(agentsDir, name string) (*Agent, error) {
	if err := ValidateAgentName(name); err != nil {
		return nil, err
	}

	// Two-step resolution: user dir first, then _builtin fallback
	dir := filepath.Join(agentsDir, name)
	if _, err := os.Stat(filepath.Join(dir, "AGENT.md")); err != nil {
		builtinDir := filepath.Join(agentsDir, "_builtin", name)
		if _, err := os.Stat(filepath.Join(builtinDir, "AGENT.md")); err != nil {
			return nil, fmt.Errorf("agent %q: missing AGENT.md: %w", name, err)
		}
		dir = builtinDir
	}

	promptData, err := os.ReadFile(filepath.Join(dir, "AGENT.md"))
	if err != nil {
		return nil, fmt.Errorf("agent %q: missing AGENT.md: %w", name, err)
	}

	// MEMORY.md always from top-level runtime dir, not definition dir
	runtimeDir := filepath.Join(agentsDir, name)
	var memory string
	if data, err := os.ReadFile(filepath.Join(runtimeDir, "MEMORY.md")); err == nil {
		memory = string(data)
	}

	ag := &Agent{Name: name, Prompt: string(promptData), Memory: memory}

	// Load per-agent config overlay (optional)
	if cfgData, err := os.ReadFile(filepath.Join(dir, "config.yaml")); err == nil {
		agCfg, err := parseAgentConfig(cfgData)
		if err != nil {
			return nil, fmt.Errorf("agent %q: bad config.yaml: %w", name, err)
		}
		if agCfg.CWD != "" {
			if err := cwdctx.ValidateCWD(agCfg.CWD); err != nil {
				return nil, fmt.Errorf("agent %s: %w", name, err)
			}
		}
		ag.Config = agCfg
	}

	// Load agent-scoped commands (optional)
	ag.Commands = loadAgentCommands(filepath.Join(dir, "commands"))

	// Load skills from _attached.yaml manifest
	shannonDir := filepath.Dir(agentsDir)
	attachedNames, hasManifest := loadAttachedSkills(filepath.Join(dir, "_attached.yaml"))
	if hasManifest && len(attachedNames) > 0 {
		globalSkillsDir := filepath.Join(shannonDir, "skills")
		allSkills, err := skills.LoadSkills(
			skills.SkillSource{Dir: globalSkillsDir, Source: "global"},
		)
		if err != nil {
			return nil, fmt.Errorf("agent %q: bad skills: %w", name, err)
		}
		attached := make(map[string]bool, len(attachedNames))
		for _, n := range attachedNames {
			attached[n] = true
		}
		for _, s := range allSkills {
			if attached[s.Name] {
				ag.Skills = append(ag.Skills, s)
			}
		}
	}

	return ag, nil
}

// loadAttachedSkills reads the _attached.yaml manifest listing skill names.
// Returns (names, true) if the file exists, (nil, false) if not.
func loadAttachedSkills(path string) ([]string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var names []string
	if err := yaml.Unmarshal(data, &names); err != nil {
		return nil, false
	}
	return names, true
}

// ReadAttachedSkills reads an agent's attached-skill manifest.
// Returns (nil, nil) when the manifest does not exist.
func ReadAttachedSkills(agentsDir, agentName string) ([]string, error) {
	if err := ValidateAgentName(agentName); err != nil {
		return nil, err
	}
	path := filepath.Join(agentsDir, agentName, "_attached.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	if err := yaml.Unmarshal(data, &names); err != nil {
		return nil, err
	}
	return names, nil
}

// WriteAttachedSkills writes the _attached.yaml manifest for an agent.
func WriteAttachedSkills(agentsDir, agentName string, names []string) error {
	dir := filepath.Join(agentsDir, agentName)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := yaml.Marshal(names)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "_attached.yaml"), data, 0600)
}

// DeleteAttachedSkills removes the _attached.yaml manifest.
func DeleteAttachedSkills(agentsDir, agentName string) error {
	path := filepath.Join(agentsDir, agentName, "_attached.yaml")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// SetAttachedSkills replaces an agent's attached-skill manifest with a normalized set.
// Names are deduplicated and sorted; an empty set removes the manifest.
func SetAttachedSkills(agentsDir, agentName string, names []string) error {
	seen := make(map[string]bool, len(names))
	normalized := make([]string, 0, len(names))
	for _, name := range names {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		normalized = append(normalized, name)
	}
	sort.Strings(normalized)
	if len(normalized) == 0 {
		return DeleteAttachedSkills(agentsDir, agentName)
	}
	return WriteAttachedSkills(agentsDir, agentName, normalized)
}

// AttachSkill adds a skill name to an agent's attached-skill manifest.
func AttachSkill(agentsDir, agentName, skillName string) error {
	names, err := ReadAttachedSkills(agentsDir, agentName)
	if err != nil {
		return err
	}
	names = append(names, skillName)
	return SetAttachedSkills(agentsDir, agentName, names)
}

// DetachSkill removes a skill name from an agent's attached-skill manifest.
func DetachSkill(agentsDir, agentName, skillName string) error {
	names, err := ReadAttachedSkills(agentsDir, agentName)
	if err != nil {
		return err
	}
	filtered := make([]string, 0, len(names))
	for _, name := range names {
		if name != skillName {
			filtered = append(filtered, name)
		}
	}
	return SetAttachedSkills(agentsDir, agentName, filtered)
}

// parseAgentConfig parses the per-agent config.yaml, handling the special
// _inherit key inside mcp_servers.
func parseAgentConfig(data []byte) (*AgentConfig, error) {
	var cfg AgentConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	// Parse mcp_servers manually to handle _inherit key
	var raw struct {
		MCPServers map[string]yaml.Node `yaml:"mcp_servers"`
	}
	if err := yaml.Unmarshal(data, &raw); err == nil && len(raw.MCPServers) > 0 {
		mcpCfg := &AgentMCPConfig{
			Servers: make(map[string]AgentMCPServerRef),
		}
		for key, node := range raw.MCPServers {
			if key == "_inherit" {
				var inherit bool
				if err := node.Decode(&inherit); err == nil {
					mcpCfg.Inherit = inherit
				}
				continue
			}
			var srv AgentMCPServerRef
			if err := node.Decode(&srv); err == nil {
				mcpCfg.Servers[key] = srv
			}
		}
		cfg.MCPServers = mcpCfg
	}

	return &cfg, nil
}

const maxAgentCommandChars = 8000

// loadAgentCommands loads .md files from the agent's commands/ directory.
func loadAgentCommands(dir string) map[string]string {
	entries, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil || len(entries) == 0 {
		return nil
	}
	sort.Strings(entries)
	commands := make(map[string]string)
	for _, path := range entries {
		name := strings.TrimSuffix(filepath.Base(path), ".md")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		content := string(data)
		if utf8.RuneCountInString(content) > maxAgentCommandChars {
			content = string([]rune(content)[:maxAgentCommandChars])
		}
		commands[name] = content
	}
	return commands
}

// LoadGlobalSkills loads skills from the global skills directory (~/.shannon/skills/).
// Only installed (global) skills are returned — bundled skills must be explicitly installed first.
func LoadGlobalSkills(shannonDir string) ([]*skills.Skill, error) {
	globalSkillsDir := filepath.Join(shannonDir, "skills")
	return skills.LoadSkills(
		skills.SkillSource{Dir: globalSkillsDir, Source: "global"},
	)
}

// AgentEntry represents an agent in the listing with source metadata.
type AgentEntry struct {
	Name     string `json:"name"`
	Builtin  bool   `json:"builtin"`  // loaded from _builtin
	Override bool   `json:"override"` // user-defined agent overrides a builtin
}

func ListAgents(agentsDir string) ([]AgentEntry, error) {
	userNames, err := listAgentNames(agentsDir)
	if err != nil {
		return nil, err
	}
	builtinNames, err2 := listAgentNames(filepath.Join(agentsDir, "_builtin"))
	if err2 != nil {
		return nil, fmt.Errorf("scanning builtin agents: %w", err2)
	}

	builtinSet := make(map[string]bool, len(builtinNames))
	for _, n := range builtinNames {
		builtinSet[n] = true
	}

	seen := make(map[string]bool)
	var entries []AgentEntry

	// User-defined agents first (they win on dedup)
	for _, name := range userNames {
		if name == "_builtin" {
			continue
		}
		seen[name] = true
		entries = append(entries, AgentEntry{
			Name:     name,
			Builtin:  false,
			Override: builtinSet[name],
		})
	}

	// Builtin agents not overridden
	for _, name := range builtinNames {
		if seen[name] {
			continue
		}
		entries = append(entries, AgentEntry{
			Name:    name,
			Builtin: true,
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})
	return entries, nil
}

// listAgentNames scans a directory for valid agent subdirectories.
// Returns an error for I/O failures other than "directory does not exist".
func listAgentNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if err := ValidateAgentName(e.Name()); err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "AGENT.md")); err == nil {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func ParseAgentMention(msg string) (string, string) {
	m := mentionRe.FindStringSubmatch(msg)
	if m == nil {
		return "", msg
	}
	name := strings.ToLower(m[1])
	if err := ValidateAgentName(name); err != nil {
		return "", msg
	}
	rest := strings.TrimSpace(msg[len(m[0]):])
	return name, rest
}
