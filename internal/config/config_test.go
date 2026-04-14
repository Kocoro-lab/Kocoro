package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/permissions"
)

func TestAppendAllowedCommand(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte("endpoint: https://example.com\n"), 0644)

	err := AppendAllowedCommand(dir, "git status")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(cfgPath)
	content := string(data)
	if !strings.Contains(content, "git status") {
		t.Errorf("should contain 'git status', got:\n%s", content)
	}

	// Append another
	err = AppendAllowedCommand(dir, "ls -la")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ = os.ReadFile(cfgPath)
	content = string(data)
	if !strings.Contains(content, "ls -la") {
		t.Errorf("should contain 'ls -la', got:\n%s", content)
	}
	if !strings.Contains(content, "git status") {
		t.Errorf("should still contain 'git status', got:\n%s", content)
	}
}

func TestAppendAllowedCommand_NoDuplicates(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte("permissions:\n  allowed_commands:\n    - \"git status\"\n"), 0644)

	err := AppendAllowedCommand(dir, "git status")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(cfgPath)
	if strings.Count(string(data), "git status") > 1 {
		t.Error("should not duplicate existing command")
	}
}

func TestLoad_DoesNotApplyProjectOverlayFromProcessCWD(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	shannonDir := filepath.Join(home, ".shannon")
	if err := os.MkdirAll(shannonDir, 0700); err != nil {
		t.Fatalf("mkdir shannon dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte("model_tier: medium\n"), 0600); err != nil {
		t.Fatalf("write global config: %v", err)
	}

	projectDir := t.TempDir()
	projectConfigDir := filepath.Join(projectDir, ".shannon")
	if err := os.MkdirAll(projectConfigDir, 0755); err != nil {
		t.Fatalf("mkdir project config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectConfigDir, "config.yaml"), []byte("model_tier: low\n"), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	defer func() { _ = os.Chdir(oldWD) }()
	if err := os.Chdir(projectDir); err != nil {
		t.Fatalf("chdir project: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.ModelTier != "medium" {
		t.Fatalf("expected global model tier, got %q", cfg.ModelTier)
	}
}

func TestRuntimeConfigForCWD_AppliesOnlySessionSafeProjectOverrides(t *testing.T) {
	projectDir := t.TempDir()
	projectConfigDir := filepath.Join(projectDir, ".shannon")
	if err := os.MkdirAll(projectConfigDir, 0755); err != nil {
		t.Fatalf("mkdir project config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectConfigDir, "config.yaml"), []byte(strings.Join([]string{
		"endpoint: https://project.example",
		"model_tier: low",
		"tools:",
		"  bash_max_output: 4096",
		"permissions:",
		"  allowed_commands:",
		"    - make test",
		"daemon:",
		"  auto_approve: true",
	}, "\n")), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectConfigDir, "config.local.yaml"), []byte(strings.Join([]string{
		"permissions:",
		"  allowed_commands:",
		"    - go test ./...",
	}, "\n")), 0644); err != nil {
		t.Fatalf("write local project config: %v", err)
	}

	base := &Config{
		Endpoint:  "https://global.example",
		ModelTier: "medium",
		Permissions: permissions.PermissionsConfig{
			AllowedCommands: []string{"git status"},
		},
		Tools: ToolsConfig{
			BashMaxOutput: 30000,
		},
		Sources: buildDefaultSources(),
	}

	cfg, err := RuntimeConfigForCWD(base, projectDir)
	if err != nil {
		t.Fatalf("RuntimeConfigForCWD error: %v", err)
	}

	if cfg.Endpoint != "https://global.example" {
		t.Fatalf("expected endpoint to stay global, got %q", cfg.Endpoint)
	}
	if cfg.ModelTier != "low" {
		t.Fatalf("expected project model tier, got %q", cfg.ModelTier)
	}
	if cfg.Tools.BashMaxOutput != 4096 {
		t.Fatalf("expected project bash_max_output, got %d", cfg.Tools.BashMaxOutput)
	}
	if got := cfg.Permissions.AllowedCommands; len(got) != 3 || got[0] != "git status" || got[1] != "make test" || got[2] != "go test ./..." {
		t.Fatalf("unexpected allowed commands: %#v", got)
	}
	if cfg.Daemon.AutoApprove {
		t.Fatal("expected daemon config to remain global")
	}
	if src := cfg.Sources["model_tier"]; src.Level != "project" {
		t.Fatalf("expected project source for model_tier, got %#v", src)
	}
	if src := cfg.Sources["permissions.allowed_commands"]; src.Level != "local" {
		t.Fatalf("expected local source for allowed_commands, got %#v", src)
	}
}

func TestSkillsConfigDefault(t *testing.T) {
	// Use a scratch HOME so we don't touch the real ~/.shannon/config.yaml.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := "https://raw.githubusercontent.com/Kocoro-lab/shanclaw-skill-registry/main/index.json"
	if cfg.Skills.Marketplace.RegistryURL != want {
		t.Errorf("Skills.Marketplace.RegistryURL = %q, want %q", cfg.Skills.Marketplace.RegistryURL, want)
	}
}

// TestMergeRuntimeOverlayFile_MCPWorkspaceRoots guards the plumbing of
// mcp.workspace_roots from project/local overlay files into the merged
// Config. Before the fix the field was declared on overlayConfig but
// never read in mergeRuntimeOverlayFile, so project-level workspace
// roots were silently dropped.
func TestMergeRuntimeOverlayFile_MCPWorkspaceRoots(t *testing.T) {
	dir := t.TempDir()
	overlayPath := filepath.Join(dir, ".shannon", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(overlayPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	overlayYAML := `mcp:
  workspace_roots:
    - /workspace/project-a
    - /workspace/shared
`
	if err := os.WriteFile(overlayPath, []byte(overlayYAML), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Seed a baseline config with one existing root — overlay should
	// append rather than replace, and dedupe against what's already there.
	cfg := &Config{
		MCP:     MCPConfig{WorkspaceRoots: []string{"/workspace/shared"}},
		Sources: map[string]ConfigSource{},
	}

	mergeRuntimeOverlayFile(cfg, overlayPath, "project")

	got := cfg.MCP.WorkspaceRoots
	if len(got) != 2 {
		t.Fatalf("expected 2 workspace roots after dedup, got %d: %v", len(got), got)
	}
	seen := make(map[string]bool)
	for _, r := range got {
		seen[r] = true
	}
	for _, want := range []string{"/workspace/shared", "/workspace/project-a"} {
		if !seen[want] {
			t.Errorf("missing expected root %q in %v", want, got)
		}
	}
	if src, ok := cfg.Sources["mcp.workspace_roots"]; !ok || src.Level != "project" {
		t.Errorf("expected source to record project overlay, got %+v ok=%v", src, ok)
	}
}
