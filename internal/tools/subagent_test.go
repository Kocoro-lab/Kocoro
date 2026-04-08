package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agents"
)

// fakeRunner returns a SubAgentRunner that records calls and returns a canned result.
func fakeRunner(output string) (SubAgentRunner, *int) {
	callCount := 0
	runner := func(ctx context.Context, ag *agents.Agent, prompt string, reg *agent.ToolRegistry, handler agent.EventHandler) SubAgentResult {
		callCount++
		return SubAgentResult{Output: output, TotalTokens: 100, LLMCalls: 3, Duration: time.Second}
	}
	return runner, &callCount
}

func setupAgentsDir(t *testing.T) string {
	t.Helper()
	agentsDir := filepath.Join(t.TempDir(), "agents")
	os.MkdirAll(filepath.Join(agentsDir, "scout"), 0755)
	os.WriteFile(filepath.Join(agentsDir, "scout", "AGENT.md"),
		[]byte("You are Scout, a read-only reconnaissance specialist."), 0644)
	os.MkdirAll(filepath.Join(agentsDir, "checker"), 0755)
	os.WriteFile(filepath.Join(agentsDir, "checker", "AGENT.md"),
		[]byte("You are Checker, a verification specialist."), 0644)
	return agentsDir
}

func TestSubAgentTool_Info(t *testing.T) {
	tool := &SubAgentTool{}
	info := tool.Info()

	if info.Name != "subagent" {
		t.Errorf("expected name 'subagent', got %q", info.Name)
	}

	if !slices.Contains(info.Required, "agent_type") {
		t.Errorf("expected 'agent_type' in Required, got %v", info.Required)
	}
}

func TestSubAgentTool_InfoListsAgents(t *testing.T) {
	agentsDir := setupAgentsDir(t)
	tool := &SubAgentTool{agentsDir: agentsDir}
	info := tool.Info()

	if !strings.Contains(info.Description, "scout") {
		t.Errorf("expected description to contain 'explorer', got: %s", info.Description)
	}
	if !strings.Contains(info.Description, "checker") {
		t.Errorf("expected description to contain 'reviewer', got: %s", info.Description)
	}
}

func TestSubAgentTool_Foreground(t *testing.T) {
	agentsDir := setupAgentsDir(t)
	runner, callCount := fakeRunner("analysis complete")

	baseReg := agent.NewToolRegistry()
	tool := &SubAgentTool{
		agentsDir: agentsDir,
		runner:    runner,
		baseReg:   baseReg,
	}

	result, err := tool.Run(context.Background(), `{"prompt":"analyze this","description":"code analysis","agent_type":"scout"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected IsError=true: %s", result.Content)
	}
	if *callCount != 1 {
		t.Errorf("expected runner called once, got %d", *callCount)
	}
	if !strings.Contains(result.Content, "analysis complete") {
		t.Errorf("expected output in result, got: %s", result.Content)
	}
}

func TestSubAgentTool_MissingAgentType(t *testing.T) {
	tool := &SubAgentTool{}

	result, err := tool.Run(context.Background(), `{"prompt":"do something","description":"test task","agent_type":""}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Errorf("expected IsError=true for missing agent_type")
	}
}

func TestSubAgentTool_UnknownAgentType(t *testing.T) {
	agentsDir := setupAgentsDir(t)
	runner, _ := fakeRunner("output")
	baseReg := agent.NewToolRegistry()
	tool := &SubAgentTool{
		agentsDir: agentsDir,
		runner:    runner,
		baseReg:   baseReg,
	}

	result, err := tool.Run(context.Background(), `{"prompt":"do something","description":"test task","agent_type":"nonexistent"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Errorf("expected IsError=true for unknown agent_type")
	}
}

func TestSubAgentTool_RecursionGuard(t *testing.T) {
	agentsDir := setupAgentsDir(t)

	var capturedReg *agent.ToolRegistry
	runner := func(ctx context.Context, ag *agents.Agent, prompt string, reg *agent.ToolRegistry, handler agent.EventHandler) SubAgentResult {
		capturedReg = reg
		return SubAgentResult{Output: "done"}
	}

	// Register a fake subagent tool so we can verify it gets removed
	baseReg := agent.NewToolRegistry()
	baseReg.Register(&SubAgentTool{agentsDir: agentsDir})

	tool := &SubAgentTool{
		agentsDir: agentsDir,
		runner:    runner,
		baseReg:   baseReg,
	}

	_, err := tool.Run(context.Background(), `{"prompt":"do something","description":"test task","agent_type":"scout"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedReg == nil {
		t.Fatal("runner was not called")
	}

	for _, name := range capturedReg.Names() {
		if name == "subagent" {
			t.Errorf("child registry must not contain 'subagent' (recursion guard failed)")
		}
	}
}

func TestSubAgentTool_IsReadOnly(t *testing.T) {
	tool := &SubAgentTool{}
	if !tool.IsReadOnlyCall(`{"prompt":"x","agent_type":"worker"}`) {
		t.Error("subagent should be read-only for parallel batching")
	}
}

func TestSubAgentTool_NoRunner(t *testing.T) {
	agentsDir := setupAgentsDir(t)
	tool := &SubAgentTool{
		agentsDir: agentsDir,
		runner:    nil,
		baseReg:   agent.NewToolRegistry(),
	}

	result, err := tool.Run(context.Background(), `{"prompt":"do something","description":"test task","agent_type":"scout"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Errorf("expected IsError=true when runner is nil")
	}
}

func TestSubAgentTool_NilBaseReg(t *testing.T) {
	agentsDir := setupAgentsDir(t)
	runner, _ := fakeRunner("output")
	tool := &SubAgentTool{
		agentsDir: agentsDir,
		runner:    runner,
		baseReg:   nil,
	}

	result, err := tool.Run(context.Background(), `{"prompt":"test","description":"test","agent_type":"scout"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected IsError=true when baseReg is nil")
	}
}

func TestSubAgentArgs_TaskID(t *testing.T) {
	var args subagentArgs
	err := json.Unmarshal([]byte(`{"prompt":"do stuff","description":"test","agent_type":"scout","task_id":"3"}`), &args)
	if err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if args.TaskID != "3" {
		t.Errorf("expected task_id %q, got %q", "3", args.TaskID)
	}
}

func TestSubAgentArgs_TaskID_Optional(t *testing.T) {
	var args subagentArgs
	err := json.Unmarshal([]byte(`{"prompt":"do stuff","description":"test","agent_type":"scout"}`), &args)
	if err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if args.TaskID != "" {
		t.Errorf("expected empty task_id, got %q", args.TaskID)
	}
}
