package tools

import (
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// Registration is first-wins on tool-name collisions, and the rebuild used to
// iterate the healthStates map directly — with Go's randomized map order the
// owning server of a shared tool name flipped between rebuilds, so the same
// tool name could start dispatching to a different backend mid-session. The
// owner must be pinned (alphabetically-first server) across every rebuild.
func TestRebuildRegistryForHealth_SharedToolNameOwnerIsDeterministic(t *testing.T) {
	mgr := mcp.NewClientManager()
	shared := mcpgo.Tool{Name: "shared_tool"}
	// Seed in an order unrelated to the expected winner so the test cannot
	// pass by insertion-order accident.
	mgr.SeedToolCache("zeta", []mcp.RemoteTool{{ServerName: "zeta", Tool: shared}})
	mgr.SeedToolCache("alpha", []mcp.RemoteTool{{ServerName: "alpha", Tool: shared}})
	healthStates := map[string]mcp.ServerHealth{
		"zeta":  {State: mcp.StateHealthy},
		"alpha": {State: mcp.StateHealthy},
	}

	baseline := agent.NewToolRegistry()
	for i := 0; i < 64; i++ {
		reg := RebuildRegistryForHealth(baseline, nil, nil, healthStates, mgr, nil)
		raw, ok := reg.Get("shared_tool")
		if !ok {
			t.Fatalf("rebuild %d: shared_tool missing from registry", i)
		}
		owner, ok := raw.(interface{ ServerName() string })
		if !ok {
			t.Fatalf("rebuild %d: registered tool %T does not expose ServerName", i, raw)
		}
		if got := owner.ServerName(); got != "alpha" {
			t.Fatalf("rebuild %d: owner = %q, want stable alphabetically-first %q", i, got, "alpha")
		}
	}
}
