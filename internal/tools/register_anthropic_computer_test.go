package tools

import (
	"context"
	"reflect"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

type wrongRunLocalComputerTool struct {
	name string
}

func (t *wrongRunLocalComputerTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:       t.name,
		Parameters: map[string]any{"type": "object"},
	}
}

func (*wrongRunLocalComputerTool) Run(context.Context, string) (agent.ToolResult, error) {
	return agent.ToolResult{}, nil
}

func (*wrongRunLocalComputerTool) RequiresApproval() bool { return false }

func TestCloneWithAnthropicComputerForRunIsExplicitAndRunLocal(t *testing.T) {
	baseline, _, cleanup := RegisterLocalTools(&config.Config{}, nil)
	defer cleanup()

	capability := newAnthropicComputerRunCapabilityAfterVerification(1280, 800)
	first, err := CloneWithAnthropicComputerForRun(baseline, &config.Config{}, capability)
	if err != nil {
		t.Fatalf("first run clone: %v", err)
	}
	second, err := CloneWithAnthropicComputerForRun(baseline, &config.Config{}, capability)
	if err != nil {
		t.Fatalf("second run clone: %v", err)
	}

	baselineComputer, _ := baseline.Get("computer")
	if _, ok := unwrapGUIExecutionGate(baselineComputer).(*ComputerTool); !ok {
		t.Fatalf("baseline computer changed type: %T", unwrapGUIExecutionGate(baselineComputer))
	}
	defaultClone := CloneWithRuntimeConfig(baseline, &config.Config{})
	defaultComputer, _ := defaultClone.Get("computer")
	if _, ok := unwrapGUIExecutionGate(defaultComputer).(*ComputerTool); !ok {
		t.Fatalf("ordinary clone unexpectedly enabled provider adapter: %T", unwrapGUIExecutionGate(defaultComputer))
	}

	firstPublic, _ := first.Get("computer")
	secondPublic, _ := second.Get("computer")
	firstAdapter, ok := unwrapGUIExecutionGate(firstPublic).(*AnthropicComputerAdapter)
	if !ok {
		t.Fatalf("first public computer type = %T", unwrapGUIExecutionGate(firstPublic))
	}
	secondAdapter, ok := unwrapGUIExecutionGate(secondPublic).(*AnthropicComputerAdapter)
	if !ok {
		t.Fatalf("second public computer type = %T", unwrapGUIExecutionGate(secondPublic))
	}
	if firstAdapter == secondAdapter {
		t.Fatal("two run clones share AnthropicComputerAdapter")
	}

	firstRawPublic, _ := first.Get("computer_use")
	secondRawPublic, _ := second.Get("computer_use")
	firstRaw, ok := unwrapGUIExecutionGate(firstRawPublic).(*ComputerUseTool)
	if !ok {
		t.Fatalf("first raw computer_use type = %T", unwrapGUIExecutionGate(firstRawPublic))
	}
	secondRaw, ok := unwrapGUIExecutionGate(secondRawPublic).(*ComputerUseTool)
	if !ok {
		t.Fatalf("second raw computer_use type = %T", unwrapGUIExecutionGate(secondRawPublic))
	}
	if firstRaw == secondRaw {
		t.Fatal("two run clones share raw ComputerUseTool")
	}
	if firstAdapter.raw != firstRaw || secondAdapter.raw != secondRaw {
		t.Fatal("provider adapter does not reference its clone-local raw ComputerUseTool")
	}

	firstRaw.snapshot = &computerUseSnapshot{id: "first-only"}
	firstRaw.refs = map[string]refEntry{"first": {path: "window[0]", pid: 7}}
	firstRaw.coordinateArtifact = &CoordinateWindowArtifactV1{}
	if secondRaw.snapshot != nil || secondRaw.refs != nil || secondRaw.coordinateArtifact != nil {
		t.Fatalf("second run inherited first state: snapshot=%+v refs=%+v artifact=%+v",
			secondRaw.snapshot, secondRaw.refs, secondRaw.coordinateArtifact)
	}

	if _, ok := firstPublic.(agent.NativeToolProvider); !ok {
		t.Fatalf("guarded adapter lost native provider trait: %T", firstPublic)
	}
	if _, ok := firstPublic.(agent.NativeToolRequestPreparer); ok {
		t.Fatalf("Anthropic adapter still performs request-time GUI preparation: %T", firstPublic)
	}
	if _, ok := firstPublic.(agent.SafeChecker); !ok {
		t.Fatalf("guarded adapter lost safe trait: %T", firstPublic)
	}
	if _, ok := firstPublic.(agent.ReadOnlyChecker); !ok {
		t.Fatalf("guarded adapter lost read-only trait: %T", firstPublic)
	}
	if _, ok := firstPublic.(agent.ConcurrencySafeChecker); !ok {
		t.Fatalf("guarded adapter lost concurrency trait: %T", firstPublic)
	}
}

func TestCloneWithAnthropicComputerForRunSurvivesPublicToolFiltering(t *testing.T) {
	baseline, _, cleanup := RegisterLocalTools(&config.Config{}, nil)
	defer cleanup()

	runRegistry, err := CloneWithAnthropicComputerForRun(
		baseline,
		&config.Config{},
		newAnthropicComputerRunCapabilityAfterVerification(1280, 800),
	)
	if err != nil {
		t.Fatalf("run clone: %v", err)
	}
	filtered := runRegistry.FilterByAllow([]string{"computer"})
	if filtered.Has("computer_use") {
		t.Fatal("raw computer_use should not be provider-visible after filtering")
	}
	public, ok := filtered.Get("computer")
	if !ok {
		t.Fatal("filtered registry lost public computer adapter")
	}
	adapter, ok := unwrapGUIExecutionGate(public).(*AnthropicComputerAdapter)
	if !ok {
		t.Fatalf("filtered public tool type = %T", unwrapGUIExecutionGate(public))
	}
	if adapter.raw == nil {
		t.Fatal("filtered public tool lost internal raw reference")
	}
	if _, stillRegistered := filtered.Get("computer_use"); stillRegistered {
		t.Fatal("filtered registry unexpectedly exposes internal raw computer_use")
	}
}

func TestCloneWithAnthropicComputerForRunFailsClosedWithoutPartialReplacement(t *testing.T) {
	validCapability := newAnthropicComputerRunCapabilityAfterVerification(1280, 800)
	tests := []struct {
		name       string
		registry   func() *agent.ToolRegistry
		capability AnthropicComputerRunCapability
	}{
		{
			name:       "untrusted zero capability",
			registry:   validRunLocalComputerRegistry,
			capability: AnthropicComputerRunCapability{},
		},
		{
			name:     "invalid attested dimensions",
			registry: validRunLocalComputerRegistry,
			capability: AnthropicComputerRunCapability{
				seal: trustedAnthropicComputerRunCapabilitySeal,
			},
		},
		{
			name: "missing raw computer use",
			registry: func() *agent.ToolRegistry {
				reg := agent.NewToolRegistry()
				reg.Register(&ComputerTool{})
				return reg
			},
			capability: validCapability,
		},
		{
			name: "wrong raw computer use type",
			registry: func() *agent.ToolRegistry {
				reg := agent.NewToolRegistry()
				reg.Register(&wrongRunLocalComputerTool{name: "computer_use"})
				reg.Register(&ComputerTool{})
				return reg
			},
			capability: validCapability,
		},
		{
			name: "missing legacy computer",
			registry: func() *agent.ToolRegistry {
				reg := agent.NewToolRegistry()
				reg.Register(&ComputerUseTool{})
				return reg
			},
			capability: validCapability,
		},
		{
			name: "wrong legacy computer type",
			registry: func() *agent.ToolRegistry {
				reg := agent.NewToolRegistry()
				reg.Register(&ComputerUseTool{})
				reg.Register(&wrongRunLocalComputerTool{name: "computer"})
				return reg
			},
			capability: validCapability,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			registry := tc.registry()
			beforeNames := registry.Names()
			beforeComputer, hadComputer := registry.Get("computer")
			beforeRaw, hadRaw := registry.Get("computer_use")

			cloned, err := CloneWithAnthropicComputerForRun(registry, nil, tc.capability)
			if err == nil || cloned != nil {
				t.Fatalf("result=(%p, %v), want nil registry and error", cloned, err)
			}
			if !reflect.DeepEqual(registry.Names(), beforeNames) {
				t.Fatalf("input registry names changed: got %v want %v", registry.Names(), beforeNames)
			}
			afterComputer, hasComputer := registry.Get("computer")
			afterRaw, hasRaw := registry.Get("computer_use")
			if hasComputer != hadComputer || afterComputer != beforeComputer {
				t.Fatal("input public computer was partially replaced")
			}
			if hasRaw != hadRaw || afterRaw != beforeRaw {
				t.Fatal("input raw computer_use was partially replaced")
			}
		})
	}

	if cloned, err := CloneWithAnthropicComputerForRun(nil, nil, validCapability); err == nil || cloned != nil {
		t.Fatalf("nil registry result=(%p, %v), want nil registry and error", cloned, err)
	}
}

func validRunLocalComputerRegistry() *agent.ToolRegistry {
	reg := agent.NewToolRegistry()
	reg.Register(&ComputerUseTool{})
	reg.Register(&ComputerTool{})
	return reg
}
