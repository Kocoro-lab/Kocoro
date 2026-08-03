package agent

import (
	"context"
	"testing"
)

type budgetTestTool struct {
	name        string
	desc        string
	exposure    ToolExposure
	requirement ToolProfileRequirement
}

func (t *budgetTestTool) Info() ToolInfo {
	return ToolInfo{
		Name:        t.name,
		Description: t.desc,
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

func (t *budgetTestTool) Run(context.Context, string) (ToolResult, error) {
	return ToolResult{}, nil
}

func (t *budgetTestTool) RequiresApproval() bool { return false }
func (t *budgetTestTool) ToolExposure() ToolExposure {
	return t.exposure
}
func (t *budgetTestTool) ToolProfileRequirement() ToolProfileRequirement {
	return t.requirement
}

func TestEstimateSchemaTokens_Simple(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&mockTool{name: "bash"})
	reg.Register(&mockTool{name: "grep"})

	tokens := estimateSchemaTokens(reg, reg.Names())
	if tokens <= 0 {
		t.Fatalf("expected positive token count, got %d", tokens)
	}
}

func TestEstimateSchemaTokens_Empty(t *testing.T) {
	reg := NewToolRegistry()
	tokens := estimateSchemaTokens(reg, nil)
	if tokens != 0 {
		t.Fatalf("expected 0 tokens for empty list, got %d", tokens)
	}
}

func TestDirectSchemaBudgetReport_UnderBudget(t *testing.T) {
	reg := NewToolRegistry()
	for _, name := range []string{"a", "b", "c"} {
		reg.Register(&mockTool{name: name})
	}

	report := directSchemaBudgetReport(reg, 50000)
	if report.Exceeded() {
		t.Fatalf("small Direct set unexpectedly exceeded budget: %+v", report)
	}
}

func TestDirectSchemaBudgetReport_ExcludesDeferredAndSortsContributors(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&budgetTestTool{name: "small_direct", desc: "small", exposure: ToolExposureDirect})
	reg.Register(&budgetTestTool{name: "large_direct", desc: string(make([]byte, 8000)), exposure: ToolExposureDirect})
	reg.Register(&budgetTestTool{name: "huge_deferred", desc: string(make([]byte, 16000)), exposure: ToolExposureDeferred})

	report := directSchemaBudgetReport(reg, 1000)
	if !report.Exceeded() {
		t.Fatalf("large Direct set should exceed the diagnostic budget: %+v", report)
	}
	if len(report.Contributors) != 2 {
		t.Fatalf("Direct contributors = %+v, want exactly 2", report.Contributors)
	}
	if report.Contributors[0].Name != "large_direct" || report.Contributors[1].Name != "small_direct" {
		t.Fatalf("contributors are not deterministically size-ranked: %+v", report.Contributors)
	}
	for _, contributor := range report.Contributors {
		if contributor.Name == "huge_deferred" {
			t.Fatalf("Deferred schema leaked into Direct budget report: %+v", report.Contributors)
		}
	}
}

func TestFormatSchemaBudgetContributors_LimitsAndCountsRemainder(t *testing.T) {
	report := schemaBudgetReport{
		Contributors: []schemaBudgetContributor{
			{Name: "largest", Tokens: 300},
			{Name: "middle", Tokens: 200},
			{Name: "smallest", Tokens: 100},
		},
	}

	if got := formatSchemaBudgetContributors(report, 2); got != "largest=300,middle=200,...(+1)" {
		t.Fatalf("limited contributors = %q", got)
	}
	if got := formatSchemaBudgetContributors(report, 0); got != "largest=300,middle=200,smallest=100" {
		t.Fatalf("unlimited contributors = %q", got)
	}
	if got := formatSchemaBudgetContributors(schemaBudgetReport{}, 3); got != "" {
		t.Fatalf("empty contributors = %q, want empty string", got)
	}
}

func TestToolSchemaFingerprint_Deterministic(t *testing.T) {
	reg1 := NewToolRegistry()
	reg1.Register(&mockTool{name: "bash"})
	reg1.Register(&mockMCPTool{name: "browser_click"})

	reg2 := NewToolRegistry()
	reg2.Register(&mockMCPTool{name: "browser_click"})
	reg2.Register(&mockTool{name: "bash"})

	fp1 := toolSchemaFingerprint(reg1)
	fp2 := toolSchemaFingerprint(reg2)
	if fp1 == "" || fp2 == "" {
		t.Fatal("fingerprints should not be empty")
	}
	if fp1 != fp2 {
		t.Fatalf("fingerprints should be deterministic, got %q vs %q", fp1, fp2)
	}
}

func TestToolSchemaFingerprint_ChangesWithProfileRequirement(t *testing.T) {
	unbound := NewToolRegistry()
	unbound.Register(&budgetTestTool{
		name:     "computer",
		exposure: ToolExposureDeferred,
	})
	profileBound := NewToolRegistry()
	profileBound.Register(&budgetTestTool{
		name:        "computer",
		exposure:    ToolExposureDeferred,
		requirement: ToolProfileComputer,
	})

	if toolSchemaFingerprint(unbound) == toolSchemaFingerprint(profileBound) {
		t.Fatal("profile requirement change must alter toolset fingerprint")
	}
}
