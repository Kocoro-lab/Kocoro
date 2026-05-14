package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConvertAgent_FrontmatterSplit(t *testing.T) {
	src := filepath.Join("testdata", "claude_home_basic", "agents", "code-reviewer.md")
	staging := t.TempDir()
	scanned := ScannedAgent{Name: "code-reviewer", SrcAbsPath: src}

	warns, err := ConvertAgent(scanned, staging, "2026-05-14T11:22:00Z")
	if err != nil {
		t.Fatalf("ConvertAgent: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warnings on standard frontmatter: %+v", warns)
	}

	agentBody, err := os.ReadFile(filepath.Join(staging, "AGENT.md"))
	if err != nil {
		t.Fatalf("AGENT.md missing: %v", err)
	}
	if !strings.Contains(string(agentBody), "expert code reviewer") {
		t.Errorf("body lost: %s", agentBody)
	}
	if strings.Contains(string(agentBody), "name: code-reviewer") {
		t.Errorf("AGENT.md must not retain raw YAML frontmatter: %s", agentBody)
	}
	if !strings.Contains(string(agentBody), "imported from ~/.claude/agents/code-reviewer.md") {
		t.Errorf("import banner missing: %s", agentBody)
	}

	cfg, err := os.ReadFile(filepath.Join(staging, "config.yaml"))
	if err != nil {
		t.Fatalf("config.yaml missing: %v", err)
	}
	cfgStr := string(cfg)
	for _, want := range []string{"Read", "Grep", "Glob", "claude-sonnet-4-6"} {
		if !strings.Contains(cfgStr, want) {
			t.Errorf("config.yaml missing %q: %s", want, cfgStr)
		}
	}
}

func TestConvertAgent_UnknownFrontmatterEmitsWarning(t *testing.T) {
	home := t.TempDir()
	agentsDir := filepath.Join(home, "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(agentsDir, "weird.md")
	body := `---
name: weird
description: has odd fields
tools: Read
model: claude-sonnet-4-6
mystery_field: 42
another_unknown: hello
---
body content
`
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	scanned := ScannedAgent{Name: "weird", SrcAbsPath: src}

	warns, err := ConvertAgent(scanned, staging, "2026-05-14T11:22:00Z")
	if err != nil {
		t.Fatalf("ConvertAgent: %v", err)
	}

	gotWarning := false
	gotFields := map[string]bool{}
	for _, w := range warns {
		if w.Kind == "unsupported_fields" && w.Server == "weird" {
			gotWarning = true
			for _, f := range w.Fields {
				gotFields[f] = true
			}
		}
	}
	if !gotWarning {
		t.Errorf("expected unsupported_fields warning for agent 'weird', got %+v", warns)
	}
	if !gotFields["mystery_field"] || !gotFields["another_unknown"] {
		t.Errorf("warning should list unknown keys; got %v", gotFields)
	}

	// Body + known fields still land.
	cfg, _ := os.ReadFile(filepath.Join(staging, "config.yaml"))
	if !strings.Contains(string(cfg), "Read") {
		t.Errorf("known frontmatter fields should still be written: %s", cfg)
	}
	body2, _ := os.ReadFile(filepath.Join(staging, "AGENT.md"))
	if !strings.Contains(string(body2), "body content") {
		t.Errorf("body should still be written: %s", body2)
	}
}

func TestConvertAgent_ToolsAsCommaString(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(home, "agents", "a.md")
	if err := os.WriteFile(src, []byte("---\nname: a\ndescription: x\ntools: Read, Grep, Glob\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	if _, err := ConvertAgent(ScannedAgent{Name: "a", SrcAbsPath: src}, staging, "2026-05-14T11:22:00Z"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := os.ReadFile(filepath.Join(staging, "config.yaml"))
	for _, t2 := range []string{"Read", "Grep", "Glob"} {
		if !strings.Contains(string(cfg), t2) {
			t.Errorf("comma-string tool %q not normalized: %s", t2, cfg)
		}
	}
}

func TestConvertAgent_NoFrontmatter(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(home, "agents", "plain.md")
	if err := os.WriteFile(src, []byte("just body, no frontmatter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	if _, err := ConvertAgent(ScannedAgent{Name: "plain", SrcAbsPath: src}, staging, "2026-05-14T11:22:00Z"); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(staging, "AGENT.md"))
	if !strings.Contains(string(body), "just body, no frontmatter") {
		t.Errorf("body lost on no-frontmatter input: %s", body)
	}
}
