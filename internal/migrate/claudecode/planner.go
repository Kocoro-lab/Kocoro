package claudecode

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// BuildPlan converts a ScanResult into an immutable Plan ready for apply.
//
// SourceHashes uses SourceFingerprint values from ComputeSkillFingerprint /
// ComputeFileFingerprint so that the applier's TOCTOU re-check dispatches to
// the correct hash algorithm — file SHA256 for flat skills/agents/commands/
// rules/the MCP source JSON, and tree hash for dir-layout skills. Do NOT
// store a raw SKILL.md hash for dir-layout skills; that would miss script
// changes mid-flight.
func BuildPlan(scan *ScanResult, src SourcePaths, target, homeDir string, now time.Time) (*Plan, error) {
	id := newPlanID(now)
	p := &Plan{
		ID:           id,
		CreatedAt:    now,
		ExpiresAt:    now.Add(PlanTTL),
		SourcePaths:  src,
		Symbolic:     SymbolicForPlan(src, target, homeDir),
		TargetPath:   target,
		SourceHashes: map[string]SourceFingerprint{},
		MCPDisabled:  map[string]bool{},
	}

	// Skills. SrcAbsPath is the .md path for flat layout, the skill dir for dir layout.
	for _, s := range scan.Skills {
		if s.Status != "ok" {
			continue
		}
		dst := filepath.Join(target, "skills", s.Name)
		if _, err := os.Stat(filepath.Join(dst, "SKILL.md")); err == nil {
			p.Conflicts = append(p.Conflicts, Conflict{Category: "skills", Name: s.Name, Reason: "exists_in_target"})
			continue
		}
		fp, err := ComputeSkillFingerprint(s)
		if err != nil {
			return nil, fmt.Errorf("fingerprint skill %q: %w", s.Name, err)
		}
		p.PlannedActions = append(p.PlannedActions, PlannedAction{
			Category: "skills", Name: s.Name, SrcAbs: s.SrcAbsPath, DstAbs: dst,
		})
		p.SourceHashes[s.SrcAbsPath] = fp
	}

	// Agents — single-file source.
	for _, a := range scan.Agents {
		if a.Status != "ok" {
			continue
		}
		dst := filepath.Join(target, "agents", a.Name)
		if _, err := os.Stat(filepath.Join(dst, "AGENT.md")); err == nil {
			p.Conflicts = append(p.Conflicts, Conflict{Category: "agents", Name: a.Name, Reason: "exists_in_target"})
			continue
		}
		fp, err := ComputeFileFingerprint(a.SrcAbsPath)
		if err != nil {
			return nil, fmt.Errorf("fingerprint agent %q: %w", a.Name, err)
		}
		p.PlannedActions = append(p.PlannedActions, PlannedAction{
			Category: "agents", Name: a.Name, SrcAbs: a.SrcAbsPath, DstAbs: dst,
		})
		p.SourceHashes[a.SrcAbsPath] = fp
	}

	// Commands → skills with slug claude-command-<name>. Single-file source.
	for _, c := range scan.Commands {
		if c.Status != "ok" {
			continue
		}
		slug := commandSkillSlug(c.Name)
		dst := filepath.Join(target, "skills", slug)
		if _, err := os.Stat(filepath.Join(dst, "SKILL.md")); err == nil {
			p.Conflicts = append(p.Conflicts, Conflict{Category: "commands", Name: c.Name, Reason: "exists_in_target"})
			continue
		}
		fp, err := ComputeFileFingerprint(c.SrcAbsPath)
		if err != nil {
			return nil, fmt.Errorf("fingerprint command %q: %w", c.Name, err)
		}
		p.PlannedActions = append(p.PlannedActions, PlannedAction{
			Category: "commands", Name: c.Name, SrcAbs: c.SrcAbsPath, DstAbs: dst,
		})
		p.SourceHashes[c.SrcAbsPath] = fp
	}

	// Global rules — single-file source.
	if scan.GlobalRules != nil && scan.GlobalRules.Status == "ok" {
		dst := filepath.Join(target, "instructions.md")
		if _, err := os.Stat(dst); err == nil {
			p.Conflicts = append(p.Conflicts, Conflict{Category: "global_rules", Name: "instructions.md", Reason: "exists_in_target"})
		} else {
			fp, err := ComputeFileFingerprint(scan.GlobalRules.SrcAbsPath)
			if err != nil {
				return nil, fmt.Errorf("fingerprint global_rules: %w", err)
			}
			p.PlannedActions = append(p.PlannedActions, PlannedAction{
				Category: "global_rules", Name: "instructions.md", SrcAbs: scan.GlobalRules.SrcAbsPath, DstAbs: dst,
			})
			p.SourceHashes[scan.GlobalRules.SrcAbsPath] = fp
		}
	}

	existingMCP, err := existingMCPServerNames(target)
	if err != nil {
		return nil, err
	}

	// MCP servers — disable when missing env keys OR unsupported fields non-empty.
	// All MCP servers derive from a single source file (src.ClaudeUserConfig);
	// record one fingerprint covering it so a mid-flight edit to ~/.claude.json
	// triggers plan_stale at apply.
	if len(scan.MCPServers) > 0 {
		if fp, err := ComputeFileFingerprint(src.ClaudeUserConfig); err == nil {
			p.SourceHashes[src.ClaudeUserConfig] = fp
		}
	}
	for _, m := range scan.MCPServers {
		if m.Status != "ok" {
			continue
		}
		if existingMCP[m.Name] {
			p.Conflicts = append(p.Conflicts, Conflict{Category: "mcp_servers", Name: m.Name, Reason: "exists_in_target"})
			continue
		}
		if len(m.EnvKeys) > 0 || len(m.UnsupportedFields) > 0 {
			p.MCPDisabled[m.Name] = true
		}
		p.PlannedActions = append(p.PlannedActions, PlannedAction{
			Category: "mcp_servers", Name: m.Name, DstAbs: filepath.Join(target, "config.yaml"),
		})
		if len(m.EnvKeys) > 0 {
			p.PlannedWarnings = append(p.PlannedWarnings, Warning{Kind: "missing_env_keys", Server: m.Name, Keys: m.EnvKeys})
		}
		if len(m.UnsupportedFields) > 0 {
			p.PlannedWarnings = append(p.PlannedWarnings, Warning{Kind: "unsupported_fields", Server: m.Name, Fields: m.UnsupportedFields})
		}
	}

	p.Hash = computePlanHash(p)
	return p, nil
}

func existingMCPServerNames(target string) (map[string]bool, error) {
	cfgPath := filepath.Join(target, "config.yaml")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var raw struct {
		MCPServers map[string]struct{} `yaml:"mcp_servers"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse existing config.yaml mcp_servers: %w", err)
	}
	out := make(map[string]bool, len(raw.MCPServers))
	for name := range raw.MCPServers {
		out[name] = true
	}
	return out, nil
}

func newPlanID(now time.Time) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return fmt.Sprintf("mig-%s-%s", now.UTC().Format("2006-01-02"), hex.EncodeToString(b))
}

func computePlanHash(p *Plan) string {
	h := sha256.New()
	// Stable: actions sorted by (category, name), plus sorted source hashes.
	actions := append([]PlannedAction(nil), p.PlannedActions...)
	sort.Slice(actions, func(i, j int) bool {
		if actions[i].Category != actions[j].Category {
			return actions[i].Category < actions[j].Category
		}
		return actions[i].Name < actions[j].Name
	})
	for _, a := range actions {
		fmt.Fprintf(h, "%s|%s|%s|%s\n", a.Category, a.Name, a.SrcAbs, a.DstAbs)
	}
	keys := make([]string, 0, len(p.SourceHashes))
	for k := range p.SourceHashes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fp := p.SourceHashes[k]
		fmt.Fprintf(h, "%s=%s:%s\n", k, fp.Kind, fp.Hash)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
