package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/guicontrol"
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

func TestAnthropicNativePreparationRejectsMismatchedDaemonAuthority(t *testing.T) {
	adapter := NewAnthropicComputerAdapter(&ComputerUseTool{}, 1024, 768)
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	lease, err := coordinator.BeginWorkflow(guicontrol.WorkflowRequest{
		SessionID: "session-native-authority", TurnID: "turn-native-authority",
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := coordinator.BeginAction(context.Background(), guicontrol.ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID,
		ToolName: client.NativeComputerToolName, ToolUseID: "native_prepare/wrong/1",
		ActionKind: "screenshot", Effect: guicontrol.ComputerUseActionObservation,
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := guicontrol.ExecutionScope{
		ToolName: client.NativeComputerToolName, ToolUseID: "native_prepare/wrong/1",
		ActionKind: "screenshot", Effect: string(guicontrol.ComputerUseActionObservation),
	}
	ctx := handle.AuthorizeExecution(scope)
	ctx = agent.ContextWithToolInvocation(ctx, agent.ToolInvocation{
		ToolName: client.NativeComputerToolName, ToolUseID: scope.ToolUseID,
	})
	err = adapter.PrepareNativeToolRequest(ctx)
	if err == nil || !strings.Contains(err.Error(), "execution authority") {
		t.Fatalf("mismatched native preparation authority error = %v", err)
	}
}
