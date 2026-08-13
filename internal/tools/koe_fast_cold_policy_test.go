package tools

import (
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// TestKoeFastColdToolPolicyPinsRegistryDrift pins the Koe Fast cold-tool
// policy (agent.koeFastColdTools) against the real local-tool registration.
//
// Two invariants:
//  1. Every policy entry names a tool that actually registers — a rename or
//     removal must update the policy list in the same change.
//  2. Every Direct local tool that is NOT in the cold list stays under a
//     schema-size ceiling. A new schema-heavy tool registered Direct fails
//     here, forcing an explicit decision in the same change: add it to
//     koeFastColdTools (cold on Fast, discovered via tool_search) or raise
//     the ceiling with a written justification.
//
// Ceiling provenance: the largest legitimate Direct opener today is grep at
// ~797 estimated tokens (2026-08-10 measurement; ask_user_question 729,
// file_read 609 follow). 850 gives it headroom for wording tweaks while
// still catching anything in a different size class. Symptom when it binds:
// this test fails on a newly registered tool. Override path: extend
// koeFastColdTools or adjust the ceiling here — both are deliberate,
// reviewed decisions rather than silent drift.
const koeFastDirectSchemaTokenCeiling = 850

func TestKoeFastColdToolPolicyPinsRegistryDrift(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reg, _, cleanup := RegisterLocalTools(nil, nil)
	t.Cleanup(cleanup)
	// Conditional registrations that participate in the cold policy but are
	// gated at startup (cloud_delegate needs cloud.enabled).
	reg.Register(NewCloudDelegateTool(nil, "", time.Hour, 0, nil, "", ""))

	cold := map[string]bool{}
	for _, name := range agent.KoeFastColdToolNames() {
		cold[name] = true
		if !reg.Has(name) {
			t.Errorf("koeFastColdTools entry %q does not register as a local tool", name)
		}
	}

	for _, tool := range reg.All() {
		if agent.EffectiveToolExposure(tool) != agent.ToolExposureDirect {
			continue
		}
		name := tool.Info().Name
		if cold[name] {
			continue
		}
		if tokens := agent.EstimateToolSchemaTokens(tool); tokens > koeFastDirectSchemaTokenCeiling {
			t.Errorf(
				"Direct tool %q estimates %d schema tokens (> %d ceiling): add it to koeFastColdTools or raise the ceiling with justification",
				name, tokens, koeFastDirectSchemaTokenCeiling,
			)
		}
	}
}
