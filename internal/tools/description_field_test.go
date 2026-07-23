package tools

import (
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

// containsString is a test helper for asserting membership in a string slice
// without depending on order or exact length. Used across *_test.go files
// after PR 7 added a required `description` entry to most tools' Required
// lists — the previous "len(Required)==1" assertions are no longer valid.
func containsString(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// TestApprovalToolsRequireDescription is the cross-tool guard for PR 7: every
// tool whose RequiresApproval() returns true MUST include a "description"
// field in its schema (so non-technical users see human-readable text on the
// approval card instead of raw JSON args). Bash has its own bespoke
// description schema (PR 4) that pre-dates this helper; publish_to_web uses
// `purpose` for the same purpose (Desktop UI falls back to it).
//
// If this test fails after a new approval-required tool lands, either:
//   - add "description" to the tool's schema (via agent.DescriptionFieldSpec +
//     agent.DescriptionGuidance), or
//   - add the tool name to the exemptions map below with a justification.
func TestApprovalToolsRequireDescription(t *testing.T) {
	// Exemptions — tools that satisfy the spirit of "human-readable summary"
	// via a different field. UI clients are expected to fall back to these
	// when args.description is absent.
	exemptions := map[string]string{
		"bash": "PR 4 wrote a bespoke schema before the helper existed; changing it would invalidate prompt cache.",
		"computer": "Registered as an Anthropic native tool (NativeToolDef); agent.buildToolSchema drops Parameters/Description from the wire. " +
			"A `description` field in Info().Parameters would never reach the model. UI clients must synthesize a label from action/x/y.",
	}

	reg, _, cleanup := RegisterLocalTools(&config.Config{}, nil)
	defer cleanup()

	// Conditional tools that may not be in the base registry.
	// (cloud_delegate, generate_image, edit_image, publish_to_web register
	// only when cloud is enabled; we exercise them via direct instantiation
	// below so the matrix is complete.)
	type toolCase struct {
		name string
		info agent.ToolInfo
	}
	cases := make([]toolCase, 0)
	for _, t := range reg.All() {
		cases = append(cases, toolCase{name: t.Info().Name, info: t.Info()})
	}
	// Probe cloud / paid tools by direct struct (RequiresApproval=true on all).
	for _, t := range []agent.Tool{
		&CloudDelegateTool{},
		&PublishToWebTool{},
		&RetractPublishedFileTool{},
		&GenerateImageTool{},
		&EditImageTool{},
	} {
		cases = append(cases, toolCase{name: t.Info().Name, info: t.Info()})
	}

	checked := 0
	for _, c := range cases {
		// Skip tools whose RequiresApproval is false — they never show an
		// approval card so they don't need description.
		// (Need to find the tool back in the registry to call RequiresApproval.)
		var requires bool
		if tool, ok := reg.Get(c.name); ok {
			requires = tool.RequiresApproval()
		} else {
			// Conditional tools we constructed directly above — they all
			// have RequiresApproval=true; safe to assume.
			requires = true
		}
		if !requires {
			continue
		}
		if _, ok := exemptions[c.name]; ok {
			continue
		}
		checked++

		props, ok := c.info.Parameters["properties"].(map[string]any)
		if !ok {
			t.Errorf("%s: Parameters.properties missing or wrong shape", c.name)
			continue
		}
		descProp, ok := props["description"].(map[string]any)
		if !ok {
			t.Errorf("%s: properties.description missing — non-technical users will see raw JSON args on approval cards", c.name)
			continue
		}
		if descProp["type"] != "string" {
			t.Errorf("%s: properties.description.type = %v; want string", c.name, descProp["type"])
		}

		// Required must include "description" so the model is forced to
		// produce it. Without this the schema is suggestion-only.
		foundInRequired := false
		for _, r := range c.info.Required {
			if r == "description" {
				foundInRequired = true
				break
			}
		}
		if !foundInRequired {
			t.Errorf("%s: Required missing 'description' — model may skip the field and UI gets empty card. Required=%v", c.name, c.info.Required)
		}

		// Tool-level Description should include the standard guidance so
		// the model knows to write a user-facing summary.
		if !strings.Contains(c.info.Description, "IMPORTANT: ALWAYS include a \"description\" field") {
			t.Errorf("%s: top-level Description missing standard guidance; append agent.DescriptionGuidance", c.name)
		}
	}

	if checked == 0 {
		t.Fatal("no approval-required tools were exercised by this test; check the registry init path or update the test")
	}
	t.Logf("verified %d approval-required tools have a required 'description' field", checked)
}
