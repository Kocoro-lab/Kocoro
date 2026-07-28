package cmd

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/permissions"
)

func TestResolveOneShotCWD_DefaultsToProcessCWD(t *testing.T) {
	got, err := resolveOneShotCWD(nil)
	if err != nil {
		t.Fatalf("resolveOneShotCWD error: %v", err)
	}
	want, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd error: %v", err)
	}
	if got != want {
		t.Fatalf("expected process CWD %q, got %q", want, got)
	}
}

func TestResolveOneShotCWD_UsesAgentConfig(t *testing.T) {
	want := t.TempDir()
	got, err := resolveOneShotCWD(&agents.Agent{
		Config: &agents.AgentConfig{CWD: want},
	})
	if err != nil {
		t.Fatalf("resolveOneShotCWD error: %v", err)
	}
	if got != want {
		t.Fatalf("expected agent CWD %q, got %q", want, got)
	}
}

func TestResolveOneShotCWD_RejectsInvalidAgentCWD(t *testing.T) {
	_, err := resolveOneShotCWD(&agents.Agent{
		Config: &agents.AgentConfig{CWD: "/nonexistent/path/for/one-shot"},
	})
	if err == nil {
		t.Fatal("expected invalid cwd error")
	}
}

func TestOneShotRuntimeConfig_UsesResolvedProjectCWD(t *testing.T) {
	projectDir := t.TempDir()
	projectConfigDir := filepath.Join(projectDir, ".shannon")
	if err := os.MkdirAll(projectConfigDir, 0755); err != nil {
		t.Fatalf("mkdir project config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectConfigDir, "config.yaml"), []byte("model_tier: low\npermissions:\n  allowed_commands:\n    - make test\n"), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}

	agentOverride := &agents.Agent{
		Config: &agents.AgentConfig{CWD: projectDir},
	}
	effectiveCWD, err := resolveOneShotCWD(agentOverride)
	if err != nil {
		t.Fatalf("resolveOneShotCWD error: %v", err)
	}

	runCfg, err := config.RuntimeConfigForCWD(&config.Config{
		ModelTier: "medium",
		Permissions: permissions.PermissionsConfig{
			AllowedCommands: []string{"git status"},
		},
		Sources: map[string]config.ConfigSource{},
	}, effectiveCWD)
	if err != nil {
		t.Fatalf("RuntimeConfigForCWD error: %v", err)
	}

	if runCfg.ModelTier != "low" {
		t.Fatalf("expected project model tier, got %q", runCfg.ModelTier)
	}
	if got := runCfg.Permissions.AllowedCommands; len(got) != 2 || got[0] != "git status" || got[1] != "make test" {
		t.Fatalf("unexpected allowed commands: %#v", got)
	}
}

func TestCLIEventHandlerOnPreambleWritesText(t *testing.T) {
	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = writer
	t.Cleanup(func() {
		os.Stdout = original
		_ = reader.Close()
		_ = writer.Close()
	})

	handler := &cliEventHandler{}
	handler.OnPreamble("Reading the relevant files.")
	if err := writer.Close(); err != nil {
		t.Fatalf("close stdout writer: %v", err)
	}
	os.Stdout = original

	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	if strings.TrimSpace(string(got)) != "Reading the relevant files." {
		t.Fatalf("stdout = %q, want preamble text", string(got))
	}
}
