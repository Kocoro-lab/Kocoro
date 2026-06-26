package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendGlobalDisabledSkill(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte("endpoint: https://example.com\n"), 0644)

	if err := AppendGlobalDisabledSkill(dir, "pdf-reader"); err != nil {
		t.Fatalf("append: %v", err)
	}
	data, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(data), "disabled") {
		t.Errorf("config should have skills.disabled block, got:\n%s", data)
	}
	if !strings.Contains(string(data), "pdf-reader") {
		t.Errorf("config should contain 'pdf-reader', got:\n%s", data)
	}

	// Idempotent
	if err := AppendGlobalDisabledSkill(dir, "pdf-reader"); err != nil {
		t.Fatalf("re-append: %v", err)
	}
	data, _ = os.ReadFile(cfgPath)
	if strings.Count(string(data), "- pdf-reader") > 1 {
		t.Errorf("duplicate pdf-reader entry not deduped, got:\n%s", data)
	}

	// Second skill — both must survive
	if err := AppendGlobalDisabledSkill(dir, "security-review"); err != nil {
		t.Fatalf("append second: %v", err)
	}
	data, _ = os.ReadFile(cfgPath)
	if !strings.Contains(string(data), "pdf-reader") || !strings.Contains(string(data), "security-review") {
		t.Errorf("expected both skills, got:\n%s", data)
	}
	// Pre-existing config keys must be preserved
	if !strings.Contains(string(data), "endpoint") {
		t.Errorf("endpoint key lost on append:\n%s", data)
	}
}

func TestAppendGlobalDisabledSkill_NoConfigFile(t *testing.T) {
	dir := t.TempDir()
	// No config.yaml exists yet — Append should create one.
	if err := AppendGlobalDisabledSkill(dir, "pdf-reader"); err != nil {
		t.Fatalf("append on missing config: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if !strings.Contains(string(data), "pdf-reader") {
		t.Errorf("expected pdf-reader in config after first-create, got:\n%s", data)
	}
}

func TestAppendGlobalDisabledSkill_EmptyName(t *testing.T) {
	dir := t.TempDir()
	if err := AppendGlobalDisabledSkill(dir, ""); err == nil {
		t.Errorf("expected error for empty skill name")
	}
}

func TestRemoveGlobalDisabledSkill(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := AppendGlobalDisabledSkill(dir, "pdf-reader"); err != nil {
		t.Fatal(err)
	}
	if err := AppendGlobalDisabledSkill(dir, "security-review"); err != nil {
		t.Fatal(err)
	}

	// Remove one
	if err := RemoveGlobalDisabledSkill(dir, "pdf-reader"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	data, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(data), "- pdf-reader") {
		t.Errorf("pdf-reader should be removed, got:\n%s", data)
	}
	if !strings.Contains(string(data), "security-review") {
		t.Errorf("security-review should remain, got:\n%s", data)
	}

	// Remove the last one — block should be cleaned up
	if err := RemoveGlobalDisabledSkill(dir, "security-review"); err != nil {
		t.Fatalf("remove last: %v", err)
	}
	data, _ = os.ReadFile(cfgPath)
	if strings.Contains(string(data), "- security-review") || strings.Contains(string(data), "disabled") {
		t.Errorf("empty disabled key should be dropped, got:\n%s", data)
	}

	// Removing absent skill is a no-op
	if err := RemoveGlobalDisabledSkill(dir, "never_added"); err != nil {
		t.Errorf("removing absent skill should not error: %v", err)
	}

	// Removing from non-existent config is a no-op
	if err := RemoveGlobalDisabledSkill(t.TempDir(), "pdf-reader"); err != nil {
		t.Errorf("removing from non-existent config should be no-op: %v", err)
	}
}

// TestClone_IsolatesDenylists guards the data race where Clone shallow-copies
// the per-agent denylists. RuntimeConfigForCWD→Clone must hand a run its own
// copy of Skills.Disabled and MCP.DefaultAgentDisabled — otherwise a concurrent
// DELETE /skills/disabled (which rewrites the backing array in place via [:0])
// races with a run reading that slice. Mirrors the Permissions/Hooks deep-copies
// already in Clone.
func TestClone_IsolatesDenylists(t *testing.T) {
	base := &Config{
		Skills: SkillsConfig{Disabled: []string{"a", "b"}},
		MCP:    MCPConfig{DefaultAgentDisabled: []string{"x", "y"}},
	}
	cloned := Clone(base)

	// Simulate the DELETE handler rewriting the backing array in place.
	base.Skills.Disabled = append(base.Skills.Disabled[:0], "OVERWRITTEN")
	base.MCP.DefaultAgentDisabled = append(base.MCP.DefaultAgentDisabled[:0], "OVERWRITTEN")

	if len(cloned.Skills.Disabled) != 2 || cloned.Skills.Disabled[0] != "a" {
		t.Errorf("Clone aliases Skills.Disabled backing array: got %v, want [a b]", cloned.Skills.Disabled)
	}
	if len(cloned.MCP.DefaultAgentDisabled) != 2 || cloned.MCP.DefaultAgentDisabled[0] != "x" {
		t.Errorf("Clone aliases MCP.DefaultAgentDisabled backing array: got %v, want [x y]", cloned.MCP.DefaultAgentDisabled)
	}
}
