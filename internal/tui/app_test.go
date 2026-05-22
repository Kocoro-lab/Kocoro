package tui

import (
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/permissions"
)

// applyTUIAgentOverlayForTest mirrors the inlined overlay-apply block used at
// both TUI sites (startup ~L449-466 and agent-switch ~L623-640). It exists
// only so the contract can be unit-tested without spinning up a full TUI
// harness. Per spec (PR2 plan Task 4, Section 5.2.2) the production sites
// keep the block inlined for minimal diff; this helper must stay byte-equal
// to those blocks for the test to be meaningful. Only the ModelTier branch
// is exercised here — the other fields (Model/MaxIterations/...) were
// covered before T2 and have not changed.
func applyTUIAgentOverlayForTest(loop *agent.AgentLoop, agentOverride *agents.Agent) {
	if agentOverride != nil && agentOverride.Config != nil && agentOverride.Config.Agent != nil {
		ac := agentOverride.Config.Agent
		if ac.ModelTier != nil && *ac.ModelTier != "" {
			loop.SetModelTier(*ac.ModelTier)
		}
		if ac.Model != nil {
			loop.SetSpecificModel(*ac.Model)
		}
	}
}

func newTUITestLoop(seedTier string) *agent.AgentLoop {
	return agent.NewAgentLoop(nil, nil, seedTier, "", 0, 0, 0, &permissions.PermissionsConfig{}, nil, nil)
}

// TestTUI_AgentOverlayModelTierAppliesToLoop verifies the overlay-apply
// block inlined at both TUI sites picks up AgentModelConfig.ModelTier.
func TestTUI_AgentOverlayModelTierAppliesToLoop(t *testing.T) {
	loop := newTUITestLoop("medium")
	if got := loop.ModelTier(); got != "medium" {
		t.Fatalf("seed ModelTier = %q, want %q", got, "medium")
	}

	tier := "large"
	override := &agents.Agent{
		Config: &agents.AgentConfig{
			Agent: &agents.AgentModelConfig{ModelTier: &tier},
		},
	}
	applyTUIAgentOverlayForTest(loop, override)

	if got := loop.ModelTier(); got != "large" {
		t.Errorf("after overlay: ModelTier = %q, want %q", got, "large")
	}
}

// TestTUI_AgentOverlayEmptyModelTierIsNoop guards against an empty-string
// pointer silently clobbering the seed tier.
func TestTUI_AgentOverlayEmptyModelTierIsNoop(t *testing.T) {
	loop := newTUITestLoop("medium")

	empty := ""
	override := &agents.Agent{
		Config: &agents.AgentConfig{
			Agent: &agents.AgentModelConfig{ModelTier: &empty},
		},
	}
	applyTUIAgentOverlayForTest(loop, override)

	if got := loop.ModelTier(); got != "medium" {
		t.Errorf("empty-string ModelTier overlay clobbered seed: got %q, want %q", got, "medium")
	}
}

// TestTUI_AgentOverlayNilModelTierIsNoop confirms a nil ModelTier pointer
// (the default for agents that don't pin a tier) leaves the seed untouched.
func TestTUI_AgentOverlayNilModelTierIsNoop(t *testing.T) {
	loop := newTUITestLoop("medium")

	override := &agents.Agent{
		Config: &agents.AgentConfig{
			Agent: &agents.AgentModelConfig{}, // ModelTier == nil
		},
	}
	applyTUIAgentOverlayForTest(loop, override)

	if got := loop.ModelTier(); got != "medium" {
		t.Errorf("nil ModelTier overlay clobbered seed: got %q, want %q", got, "medium")
	}
}

// TestTUI_AgentOverlayNilOverrideIsNoop confirms a nil override (no agent
// passed via --agent) skips the block entirely.
func TestTUI_AgentOverlayNilOverrideIsNoop(t *testing.T) {
	loop := newTUITestLoop("medium")
	applyTUIAgentOverlayForTest(loop, nil)
	if got := loop.ModelTier(); got != "medium" {
		t.Errorf("nil override clobbered seed: got %q, want %q", got, "medium")
	}
}
