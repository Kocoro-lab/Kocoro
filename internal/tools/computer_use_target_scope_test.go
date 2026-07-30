package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

func TestComputerUseExplicitTargetScopeRejectsUnscopedObservationWithoutAX(t *testing.T) {
	fake := newFakeAXCaller()
	tool := &ComputerUseTool{
		client:      fake,
		targetScope: computerUseTargetScopeExplicitV1,
	}
	args := `{"action":"get_app_state","description":"Inspect the app"}`

	descriptor, err := tool.DescribeGUIAction(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Participates {
		t.Fatalf("unscoped observation descriptor = %+v, want no GUI participation", descriptor)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("descriptor reached AX before explicit target validation: %+v", fake.calls)
	}

	result, err := tool.Run(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Content, "requires an explicit app") {
		t.Fatalf("result = %+v, want actionable explicit-app error", result)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("tool reached AX without an explicit target: %+v", fake.calls)
	}
}

func TestComputerUseExplicitTargetScopeAllowsNamedApp(t *testing.T) {
	fake := &guiTargetFixtureClient{
		bundleID: "com.apple.Notes",
		appName:  "Notes",
	}
	tool := &ComputerUseTool{
		client:      fake,
		targetScope: computerUseTargetScopeExplicitV1,
	}

	descriptor, err := tool.DescribeGUIAction(
		context.Background(),
		`{"action":"get_app_state","app":"Notes","description":"Inspect Notes"}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !descriptor.Participates || descriptor.TargetBundleID != "com.apple.Notes" {
		t.Fatalf("descriptor = %+v", descriptor)
	}
}

func TestRequireExplicitComputerUseTargetForRunDoesNotMutateBaseline(t *testing.T) {
	baseline := agent.NewToolRegistry()
	baselineRaw := &ComputerUseTool{client: newFakeAXCaller()}
	baseline.Register(wrapGUIExecutionGate(baselineRaw))

	cloned, err := CloneWithGenericComputerUseForRun(baseline, &config.Config{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := RequireExplicitComputerUseTargetForRun(cloned); err != nil {
		t.Fatal(err)
	}

	clonedTool, ok := cloned.Get("computer_use")
	if !ok {
		t.Fatal("cloned registry lost computer_use")
	}
	clonedRaw := rawComputerUseToolForRun(clonedTool)
	if clonedRaw == nil || clonedRaw.targetScope != computerUseTargetScopeExplicitV1 {
		t.Fatalf("cloned target scope = %+v", clonedRaw)
	}
	if baselineRaw.targetScope != computerUseTargetScopeForegroundV1 {
		t.Fatalf("baseline target scope mutated to %v", baselineRaw.targetScope)
	}
}

func TestComputerUsePureDelayDoesNotResolveForegroundApp(t *testing.T) {
	fake := newFakeAXCaller()
	tool := &ComputerUseTool{
		client:      fake,
		targetScope: computerUseTargetScopeExplicitV1,
	}
	args := `{"action":"wait","timeout":0.001,"description":"Pause briefly"}`

	descriptor, err := tool.DescribeGUIAction(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Participates {
		t.Fatalf("pure delay descriptor = %+v, want no GUI participation", descriptor)
	}
	result, err := tool.Run(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("pure delay failed: %+v", result)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("pure delay reached AX: %+v", fake.calls)
	}
}
