package tools

import (
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
	mcpproto "github.com/mark3labs/mcp-go/mcp"
)

func mcpScopeRegistry() *agent.ToolRegistry {
	reg := agent.NewToolRegistry()
	reg.Register(&ThinkTool{}) // local tool — must never be touched by MCP scope
	reg.Register(NewMCPTool("serverA", mcpproto.Tool{Name: "a_tool"}, nil))
	reg.Register(NewMCPTool("serverB", mcpproto.Tool{Name: "b_tool"}, nil))
	return reg
}

func twoServerCfg() *config.Config {
	return &config.Config{MCPServers: map[string]mcp.MCPServerConfig{
		"serverA": {},
		"serverB": {},
	}}
}

// TestApplyMCPServerScope_NamedAgentScopesMCPKeepsLocal is the load-bearing
// test: a named agent allowed only serverA must keep its local tools and
// serverA's MCP tools while serverB's MCP tools are removed. Guards the two
// ways this goes wrong — failing to scope out a disallowed server, or
// accidentally deleting non-MCP (local) tools.
func TestApplyMCPServerScope_NamedAgentScopesMCPKeepsLocal(t *testing.T) {
	reg := mcpScopeRegistry()
	agentDef := &agents.Agent{Config: &agents.AgentConfig{
		MCPServers: &agents.AgentMCPConfig{
			Inherit: false,
			Servers: map[string]agents.AgentMCPServerRef{"serverA": {}},
		},
	}}

	out := ApplyMCPServerScope(reg, twoServerCfg(), agentDef)

	if !out.Has("think") {
		t.Error("local tool 'think' wrongly removed by MCP scope")
	}
	if !out.Has("a_tool") {
		t.Error("allowed serverA tool wrongly removed")
	}
	if out.Has("b_tool") {
		t.Error("disallowed serverB tool not scoped out")
	}
}

// TestApplyMCPServerScope_DefaultAgentNoOp asserts the default agent (no
// agentDef) resolves to the full global server set, so nothing is removed.
func TestApplyMCPServerScope_DefaultAgentNoOp(t *testing.T) {
	reg := mcpScopeRegistry()
	out := ApplyMCPServerScope(reg, twoServerCfg())
	for _, n := range []string{"think", "a_tool", "b_tool"} {
		if !out.Has(n) {
			t.Errorf("default agent: tool %q wrongly removed (must be no-op)", n)
		}
	}
}

// TestApplyMCPServerScope_DisabledServerScopedOut asserts a per-agent server
// marked disabled (inherit + override Disabled:true) is scoped out even though
// it resolves into the server map.
func TestApplyMCPServerScope_DisabledServerScopedOut(t *testing.T) {
	reg := mcpScopeRegistry()
	agentDef := &agents.Agent{Config: &agents.AgentConfig{
		MCPServers: &agents.AgentMCPConfig{
			Inherit: true,
			Servers: map[string]agents.AgentMCPServerRef{"serverB": {Disabled: true}},
		},
	}}
	out := ApplyMCPServerScope(reg, twoServerCfg(), agentDef)
	if !out.Has("a_tool") {
		t.Error("serverA tool wrongly removed")
	}
	if out.Has("b_tool") {
		t.Error("per-agent disabled serverB tool not scoped out")
	}
}

// TestApplyMCPServerScope_ComposesWithToolFilter asserts MCP scoping and the
// tools.allow/deny filter both apply when chained (runner runs both after
// clone). Order should be irrelevant since both are name-based FilterByDeny.
func TestApplyMCPServerScope_ComposesWithToolFilter(t *testing.T) {
	reg := mcpScopeRegistry()
	agentDef := &agents.Agent{Config: &agents.AgentConfig{
		Tools: &agents.AgentToolsFilter{Deny: []string{"think"}},
		MCPServers: &agents.AgentMCPConfig{
			Inherit: false,
			Servers: map[string]agents.AgentMCPServerRef{"serverA": {}},
		},
	}}

	out := ApplyMCPServerScope(ApplyToolFilter(reg, agentDef), twoServerCfg(), agentDef)

	if out.Has("think") {
		t.Error("tools.deny[think] not applied after composing with MCP scope")
	}
	if !out.Has("a_tool") {
		t.Error("allowed serverA tool wrongly removed")
	}
	if out.Has("b_tool") {
		t.Error("disallowed serverB tool not scoped out")
	}
}

// TestApplyMCPServerScope_DefaultAgentDenylist asserts the DEFAULT agent (no
// agentDef) honors config.mcp.default_agent_disabled: listed servers are scoped
// out while unlisted ones and local tools survive.
func TestApplyMCPServerScope_DefaultAgentDenylist(t *testing.T) {
	reg := mcpScopeRegistry()
	cfg := twoServerCfg()
	cfg.MCP.DefaultAgentDisabled = []string{"serverB"}

	out := ApplyMCPServerScope(reg, cfg) // default agent

	if !out.Has("think") {
		t.Error("local tool wrongly removed")
	}
	if !out.Has("a_tool") {
		t.Error("serverA tool wrongly removed (not in default denylist)")
	}
	if out.Has("b_tool") {
		t.Error("default-agent-disabled serverB tool not scoped out")
	}
}

// TestApplyMCPServerScope_DefaultDenylistDoesNotAffectNamed asserts the default
// denylist is default-agent-only: a named agent that inherits serverB still
// gets it even when default_agent_disabled lists it.
func TestApplyMCPServerScope_DefaultDenylistDoesNotAffectNamed(t *testing.T) {
	reg := mcpScopeRegistry()
	cfg := twoServerCfg()
	cfg.MCP.DefaultAgentDisabled = []string{"serverB"}
	agentDef := &agents.Agent{Config: &agents.AgentConfig{
		MCPServers: &agents.AgentMCPConfig{Inherit: true},
	}}

	out := ApplyMCPServerScope(reg, cfg, agentDef)

	if !out.Has("b_tool") {
		t.Error("named agent wrongly lost serverB to the default-agent denylist")
	}
}
