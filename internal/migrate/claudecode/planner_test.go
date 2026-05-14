package claudecode

import (
	"path/filepath"
	"testing"
	"time"
)

func TestPlan_BuildsActionsAndConflicts(t *testing.T) {
	src := SourcePaths{
		ClaudeHome:       filepath.Join("testdata", "claude_home_basic"),
		ClaudeUserConfig: filepath.Join("testdata", "claude_user_config_basic.json"),
	}
	scan, _ := Scan(src)
	target := t.TempDir()

	p, err := BuildPlan(scan, src, target, "/Users/wayland", time.Now())
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(p.PlannedActions) == 0 {
		t.Fatal("expected planned actions")
	}
	if len(p.Conflicts) != 0 {
		t.Errorf("unexpected conflicts on empty target: %+v", p.Conflicts)
	}
	if p.ExpiresAt.Sub(p.CreatedAt) != PlanTTL {
		t.Errorf("TTL = %v, want %v", p.ExpiresAt.Sub(p.CreatedAt), PlanTTL)
	}
	if p.Hash == "" {
		t.Error("plan Hash should be set")
	}

	// MCP servers with missing env keys or unsupported fields → disabled set.
	if !p.MCPDisabled["anthropic"] {
		t.Error("anthropic should be disabled (missing ANTHROPIC_API_KEY)")
	}
	if !p.MCPDisabled["internal-api"] {
		t.Error("internal-api should be disabled (unsupported headers)")
	}
	if p.MCPDisabled["command-only"] {
		t.Error("command-only has no missing/unsupported → should NOT be disabled")
	}

	// SourceHashes contract: every PlannedAction's source path should have a
	// SourceFingerprint entry whose Kind correctly reflects file vs tree.
	for _, a := range p.PlannedActions {
		if a.Category == "mcp_servers" {
			continue // MCP shares one fingerprint, not per-server
		}
		fp, ok := p.SourceHashes[a.SrcAbs]
		if !ok {
			t.Errorf("missing SourceHashes entry for %s/%s at %s", a.Category, a.Name, a.SrcAbs)
			continue
		}
		if fp.Hash == "" {
			t.Errorf("empty hash for %s/%s", a.Category, a.Name)
		}
		// Skills can be either "file" (flat) or "skill_tree" (dir).
		// Everything else must be "file".
		if a.Category == "skills" {
			if fp.Kind != "file" && fp.Kind != "skill_tree" {
				t.Errorf("skill %q: Kind=%q, want file or skill_tree", a.Name, fp.Kind)
			}
		} else {
			if fp.Kind != "file" {
				t.Errorf("%s/%s: Kind=%q, want file", a.Category, a.Name, fp.Kind)
			}
		}
	}
}

func TestPlan_DetectsSkillConflict(t *testing.T) {
	src := SourcePaths{
		ClaudeHome:       filepath.Join("testdata", "claude_home_basic"),
		ClaudeUserConfig: filepath.Join("testdata", "claude_user_config_basic.json"),
	}
	scan, _ := Scan(src)
	target := t.TempDir()

	if len(scan.Skills) == 0 {
		t.Fatal("test fixture has no skills")
	}
	preexisting := scan.Skills[0].Name
	dir := filepath.Join(target, "skills", preexisting)
	if err := makeDir(t, dir); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(t, filepath.Join(dir, "SKILL.md"), "existing"); err != nil {
		t.Fatal(err)
	}

	p, _ := BuildPlan(scan, src, target, "/Users/wayland", time.Now())
	gotConflict := false
	for _, c := range p.Conflicts {
		if c.Category == "skills" && c.Name == preexisting {
			gotConflict = true
		}
	}
	if !gotConflict {
		t.Errorf("expected conflict for skill %q", preexisting)
	}
	for _, a := range p.PlannedActions {
		if a.Category == "skills" && a.Name == preexisting {
			t.Errorf("conflicting skill %q should not appear in PlannedActions", preexisting)
		}
	}
}

func TestPlan_DetectsMCPConflict(t *testing.T) {
	src := SourcePaths{
		ClaudeHome:       filepath.Join("testdata", "claude_home_basic"),
		ClaudeUserConfig: filepath.Join("testdata", "claude_user_config_basic.json"),
	}
	scan, _ := Scan(src)
	target := t.TempDir()
	if err := writeFile(t, filepath.Join(target, "config.yaml"), `mcp_servers:
  anthropic:
    command: existing
`); err != nil {
		t.Fatal(err)
	}

	p, err := BuildPlan(scan, src, target, "/Users/wayland", time.Now())
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	gotConflict := false
	for _, c := range p.Conflicts {
		if c.Category == "mcp_servers" && c.Name == "anthropic" {
			gotConflict = true
		}
	}
	if !gotConflict {
		t.Errorf("expected MCP conflict for anthropic, got %+v", p.Conflicts)
	}
	for _, a := range p.PlannedActions {
		if a.Category == "mcp_servers" && a.Name == "anthropic" {
			t.Errorf("conflicting MCP server should not appear in PlannedActions: %+v", a)
		}
	}
}

func TestPlan_HashStable(t *testing.T) {
	src := SourcePaths{
		ClaudeHome:       filepath.Join("testdata", "claude_home_basic"),
		ClaudeUserConfig: filepath.Join("testdata", "claude_user_config_basic.json"),
	}
	scan, _ := Scan(src)
	target := t.TempDir()

	now := time.Now()
	p1, _ := BuildPlan(scan, src, target, "/Users/wayland", now)
	p2, _ := BuildPlan(scan, src, target, "/Users/wayland", now)
	if p1.Hash != p2.Hash {
		t.Errorf("hash not stable across builds: %q vs %q", p1.Hash, p2.Hash)
	}
}

func TestPlan_MCPSourceFingerprintRecorded(t *testing.T) {
	src := SourcePaths{
		ClaudeHome:       filepath.Join("testdata", "claude_home_basic"),
		ClaudeUserConfig: filepath.Join("testdata", "claude_user_config_basic.json"),
	}
	scan, _ := Scan(src)
	target := t.TempDir()

	p, _ := BuildPlan(scan, src, target, "/Users/wayland", time.Now())
	fp, ok := p.SourceHashes[src.ClaudeUserConfig]
	if !ok {
		t.Fatal("MCP source file fingerprint not recorded — TOCTOU re-check would miss claude.json edits")
	}
	if fp.Kind != "file" {
		t.Errorf("MCP fingerprint Kind=%q, want file", fp.Kind)
	}
}
