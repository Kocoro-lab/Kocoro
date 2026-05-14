// Package claudecode implements one-way migration of Claude Code user-scope
// configuration into Kocoro's ~/.shannon/ tree. The package never reads,
// retains, or copies secrets — see the spec at
// docs/superpowers/specs/2026-05-14-claude-migrate-design.md §7 for the
// privacy invariants this code is required to uphold.
package claudecode

import "time"

// SourcePaths holds resolved absolute paths to Claude Code's two source roots.
type SourcePaths struct {
	ClaudeHome       string // e.g. /Users/wayland/.claude
	ClaudeUserConfig string // e.g. /Users/wayland/.claude.json
}

// SymbolicPaths holds ~/-prefixed forms suitable for default API responses.
type SymbolicPaths struct {
	ClaudeHome       string // ~/.claude
	ClaudeUserConfig string // ~/.claude.json
	Target           string // ~/.shannon
}

// ScanResult is the raw output of scanning ~/.claude/ and ~/.claude.json.
// It is never returned over the wire — the planner converts it into a Plan
// + PreviewResponse with conflict detection and warnings.
type ScanResult struct {
	Skills       []ScannedSkill
	Agents       []ScannedAgent
	Commands     []ScannedCommand
	GlobalRules  *ScannedRules
	MCPServers   []ScannedMCPServer
	Warnings     []Warning
	SourceErrors map[string]string // "claude_home" or "claude_user_config" → error
}

type ScannedSkill struct {
	Name        string
	SrcRelPath  string // relative to ClaudeHome
	SrcAbsPath  string
	Layout      string // "flat" (single .md) | "dir" (<name>/SKILL.md + scripts)
	SizeBytes   int64
	ContentHash string // sha256 of SKILL.md (or flat .md)
	Status      string // "ok" | "error"
	ErrorReason string
}

type ScannedAgent struct {
	Name        string
	SrcRelPath  string
	SrcAbsPath  string
	SizeBytes   int64
	ContentHash string
	Status      string
	ErrorReason string
}

type ScannedCommand struct {
	Name        string
	SrcRelPath  string
	SrcAbsPath  string
	SizeBytes   int64
	ContentHash string
	Status      string
	ErrorReason string
}

type ScannedRules struct {
	SrcAbsPath  string
	SizeBytes   int64
	ContentHash string
	Status      string
	ErrorReason string
}

// ScannedMCPServer never contains env values — only the key list.
type ScannedMCPServer struct {
	Name              string
	Transport         string // "stdio" | "http" | "sse"
	Command           string
	Args              []string
	URL               string
	EnvKeys           []string // names only, never values
	Disabled          bool     // from source
	UnsupportedFields []string // e.g. ["headers"]
	Status            string
	ErrorReason       string
}

type Warning struct {
	Kind   string // "missing_env_keys" | "unsupported_fields" | "symlink_escape" | "size_limit" | "source_unavailable" | "parse_failed"
	Server string // for MCP warnings
	Keys   []string
	Fields []string
	Path   string // symbolic path for symlink/size warnings
}

type Conflict struct {
	Category string
	Name     string
	Reason   string // "exists_in_target"
}

// PlannedAction is one item the applier will commit during Phase B.
type PlannedAction struct {
	Category string // "skills" | "agents" | "commands" | "global_rules" | "mcp_servers"
	Name     string
	SrcAbs   string
	DstAbs   string
}

// Plan is the immutable preview snapshot kept in PlanStore until apply or expiry.
type Plan struct {
	ID              string
	Hash            string
	CreatedAt       time.Time
	ExpiresAt       time.Time
	SourcePaths     SourcePaths
	Symbolic        SymbolicPaths
	TargetPath      string            // absolute, e.g. /Users/wayland/.shannon
	SourceHashes    map[string]string // src_abs_path → sha256, for TOCTOU re-check
	PlannedActions  []PlannedAction
	PlannedWarnings []Warning
	Conflicts       []Conflict
	// MCPDisabled lists server names that must be written with disabled:true
	// (either missing_env_keys or unsupported_fields non-empty).
	MCPDisabled map[string]bool
}

const (
	PlanTTL = 30 * time.Minute

	MaxFileBytes     = 5 * 1024 * 1024
	MaxSkillDirBytes = 50 * 1024 * 1024
	MaxPreviewBytes  = 2 * 1024 * 1024
)
