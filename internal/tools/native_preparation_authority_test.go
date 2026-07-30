package tools

import (
	"context"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

type nativePreparationGuardProbe struct {
	guiExecutionGateProbe
	prepareCalls int
}

func (p *nativePreparationGuardProbe) NativeToolDef() *client.NativeToolDef {
	return &client.NativeToolDef{
		Type: client.NativeComputerToolType, Name: client.NativeComputerToolName,
		DisplayWidthPx: 1024, DisplayHeightPx: 768,
	}
}

func (p *nativePreparationGuardProbe) DescribeNativeToolRequestPreparation(
	context.Context,
) (agent.GUIActionDescriptor, error) {
	return p.descriptor, nil
}

func (p *nativePreparationGuardProbe) PrepareNativeToolRequest(context.Context) error {
	p.prepareCalls++
	return nil
}

func (p *nativePreparationGuardProbe) IsReadOnlyCall(string) bool { return true }

func TestGUIExecutionGatePreservesNativePreparationDescriptor(t *testing.T) {
	probe := &nativePreparationGuardProbe{
		guiExecutionGateProbe: guiExecutionGateProbe{
			descriptor: agent.GUIActionDescriptor{
				Participates: true, ActionKind: "get_app_state",
				Effect:         agent.GUIActionObservation,
				TargetBundleID: "com.example.Editor",
			},
		},
	}
	guarded := wrapGUIExecutionGate(probe)
	describer, ok := guarded.(interface {
		DescribeNativeToolRequestPreparation(context.Context) (agent.GUIActionDescriptor, error)
	})
	if !ok {
		t.Fatalf("guarded native tool lost preparation descriptor: %T", guarded)
	}
	descriptor, err := describer.DescribeNativeToolRequestPreparation(context.Background())
	if err != nil || descriptor != probe.descriptor {
		t.Fatalf("preparation descriptor = %+v, %v; want %+v", descriptor, err, probe.descriptor)
	}
}
