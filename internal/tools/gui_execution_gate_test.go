package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/guicontrol"
)

type guiExecutionGateProbe struct {
	descriptor agent.GUIActionDescriptor
	calls      int
}

func (p *guiExecutionGateProbe) Info() agent.ToolInfo {
	return agent.ToolInfo{Name: "computer_use"}
}
func (p *guiExecutionGateProbe) RequiresApproval() bool { return true }
func (p *guiExecutionGateProbe) DescribeGUIAction(context.Context, string) (agent.GUIActionDescriptor, error) {
	return p.descriptor, nil
}
func (p *guiExecutionGateProbe) Run(context.Context, string) (agent.ToolResult, error) {
	p.calls++
	return agent.ToolResult{Content: "inner reached"}, nil
}

type guiExecutionNativeAllTraitsProbe struct {
	guiExecutionGateProbe
}

func (p *guiExecutionNativeAllTraitsProbe) NativeToolDef() *client.NativeToolDef {
	return &client.NativeToolDef{
		Type:            client.NativeComputerToolType,
		Name:            client.NativeComputerToolName,
		DisplayWidthPx:  1024,
		DisplayHeightPx: 768,
	}
}

func (p *guiExecutionNativeAllTraitsProbe) IsSafeArgs(args string) bool {
	return args == `{"safe":true}`
}

func (p *guiExecutionNativeAllTraitsProbe) IsReadOnlyCall(args string) bool {
	return args == `{"read_only":true}`
}

func (p *guiExecutionNativeAllTraitsProbe) IsConcurrencySafeCall(args string) bool {
	return args == `{"concurrent":true}`
}

func TestGUIExecutionGatePreservesEveryNativeToolTrait(t *testing.T) {
	probe := &guiExecutionNativeAllTraitsProbe{guiExecutionGateProbe: guiExecutionGateProbe{
		descriptor: agent.GUIActionDescriptor{
			Participates: true,
			ActionKind:   "screenshot",
			Effect:       agent.GUIActionObservation,
		},
	}}
	guarded := wrapGUIExecutionGate(probe)

	native, ok := guarded.(agent.NativeToolProvider)
	if !ok {
		t.Fatalf("guarded native tool lost NativeToolProvider: %T", guarded)
	}
	if def := native.NativeToolDef(); def == nil || def.DisplayWidthPx != 1024 || def.DisplayHeightPx != 768 {
		t.Fatalf("guarded native definition = %+v, want 1024x768", def)
	}
	safe, ok := guarded.(agent.SafeChecker)
	if !ok || !safe.IsSafeArgs(`{"safe":true}`) || safe.IsSafeArgs(`{"safe":false}`) {
		t.Fatalf("guarded native tool lost exact SafeChecker delegation: %T", guarded)
	}
	readOnly, ok := guarded.(agent.ReadOnlyChecker)
	if !ok || !readOnly.IsReadOnlyCall(`{"read_only":true}`) || readOnly.IsReadOnlyCall(`{"read_only":false}`) {
		t.Fatalf("guarded native tool lost exact ReadOnlyChecker delegation: %T", guarded)
	}
	concurrency, ok := guarded.(agent.ConcurrencySafeChecker)
	if !ok || !concurrency.IsConcurrencySafeCall(`{"concurrent":true}`) || concurrency.IsConcurrencySafeCall(`{"concurrent":false}`) {
		t.Fatalf("guarded native tool lost exact ConcurrencySafeChecker delegation: %T", guarded)
	}
}

func TestGUIExecutionGatePreservesToolProfileRequirement(t *testing.T) {
	for _, tool := range []agent.Tool{
		&ComputerUseTool{},
		&ComputerTool{},
	} {
		guarded := wrapGUIExecutionGate(tool)
		if got := agent.EffectiveToolProfileRequirement(guarded); got != agent.ToolProfileComputer {
			t.Fatalf(
				"guarded %s profile requirement = %q, want %q",
				tool.Info().Name,
				got,
				agent.ToolProfileComputer,
			)
		}
	}
}

func registeredGUIExecutionProbe(t *testing.T, probe *guiExecutionGateProbe) agent.Tool {
	t.Helper()
	registry := agent.NewToolRegistry()
	registry.Register(probe)
	guardRegisteredGUIExecution(registry)
	guarded, ok := registry.Get(probe.Info().Name)
	if !ok {
		t.Fatalf("guarded probe was not registered")
	}
	return guarded
}

func TestCLITUIRegistryDeniesMutationBeforeInnerRunWithoutAuthority(t *testing.T) {
	probe := &guiExecutionGateProbe{descriptor: agent.GUIActionDescriptor{
		Participates: true, ActionKind: "click", Effect: agent.GUIActionMutation,
		TargetBundleID: "com.example.Editor",
	}}
	guarded := registeredGUIExecutionProbe(t, probe)
	ctx := agent.ContextWithToolInvocation(context.Background(), agent.ToolInvocation{
		ToolName: "computer_use", ToolUseID: "toolu-cli",
	})
	result, err := guarded.Run(ctx, `{}`)
	if err != nil || !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness ||
		!strings.Contains(result.Content, "execution authority") {
		t.Fatalf("result=%+v err=%v; want fail-closed authority error", result, err)
	}
	if probe.calls != 0 {
		t.Fatalf("mutation reached inner tool: calls=%d", probe.calls)
	}
}

func TestCLITUIRegistryAllowsObservationWithoutAuthority(t *testing.T) {
	probe := &guiExecutionGateProbe{descriptor: agent.GUIActionDescriptor{
		Participates: true, ActionKind: "get_app_state", Effect: agent.GUIActionObservation,
	}}
	result, err := registeredGUIExecutionProbe(t, probe).Run(context.Background(), `{}`)
	if err != nil || result.IsError || result.Content != "inner reached" || probe.calls != 1 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, probe.calls)
	}
}

func TestGUIExecutionGateAllowsOnlyExactLiveDaemonAuthority(t *testing.T) {
	probe := &guiExecutionGateProbe{descriptor: agent.GUIActionDescriptor{
		Participates: true, ActionKind: "click", Effect: agent.GUIActionMutation,
		TargetBundleID: "com.example.Editor",
	}}
	guarded := wrapGUIExecutionGate(probe)
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	lease, err := coordinator.BeginWorkflow(guicontrol.WorkflowRequest{
		SessionID: "session-gate", TurnID: "turn-gate",
		RequestedAppBundleID: "com.example.Editor",
		AllowedAppBundleIDs:  []string{"com.example.Editor"}, PolicySnapshotID: "policy-gate",
	})
	if err != nil {
		t.Fatalf("BeginWorkflow: %v", err)
	}
	handle, err := coordinator.BeginAction(context.Background(), guicontrol.ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolName: "computer_use", ToolUseID: "toolu-daemon",
		ActionKind: "click", Effect: guicontrol.ComputerUseActionMutation,
		TargetBundleID: "com.example.Editor",
	})
	if err != nil {
		t.Fatalf("BeginAction: %v", err)
	}
	scope := guicontrol.ExecutionScope{
		ToolName: "computer_use", ToolUseID: "toolu-daemon", ActionKind: "click",
		Effect: string(guicontrol.ComputerUseActionMutation), TargetBundleID: "com.example.Editor",
	}
	ctx := handle.AuthorizeExecution(scope)
	ctx = agent.ContextWithToolInvocation(ctx, agent.ToolInvocation{
		ToolName: "computer_use", ToolUseID: "toolu-daemon",
	})
	result, err := guarded.Run(ctx, `{}`)
	if err != nil || result.IsError || probe.calls != 1 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, probe.calls)
	}
}

func TestGUIExecutionGateRejectsRevokedAuthorityAtFinalRunSeam(t *testing.T) {
	probe := &guiExecutionGateProbe{descriptor: agent.GUIActionDescriptor{
		Participates: true, ActionKind: "click", Effect: agent.GUIActionMutation,
		TargetBundleID: "com.example.Editor",
	}}
	guarded := wrapGUIExecutionGate(probe)
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	lease, err := coordinator.BeginWorkflow(guicontrol.WorkflowRequest{
		SessionID: "session-revoked", TurnID: "turn-revoked",
		RequestedAppBundleID: "com.example.Editor",
		AllowedAppBundleIDs:  []string{"com.example.Editor"}, PolicySnapshotID: "policy-revoked",
	})
	if err != nil {
		t.Fatalf("BeginWorkflow: %v", err)
	}
	handle, err := coordinator.BeginAction(context.Background(), guicontrol.ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolName: "computer_use", ToolUseID: "toolu-revoked",
		ActionKind: "click", Effect: guicontrol.ComputerUseActionMutation,
		TargetBundleID: "com.example.Editor",
	})
	if err != nil {
		t.Fatalf("BeginAction: %v", err)
	}
	scope := guicontrol.ExecutionScope{
		ToolName: "computer_use", ToolUseID: "toolu-revoked", ActionKind: "click",
		Effect: string(guicontrol.ComputerUseActionMutation), TargetBundleID: "com.example.Editor",
	}
	ctx := handle.AuthorizeExecution(scope)
	ctx = agent.ContextWithToolInvocation(ctx, agent.ToolInvocation{
		ToolName: scope.ToolName, ToolUseID: scope.ToolUseID,
	})
	resultCode := guicontrol.ComputerUseResultCancelled
	if err := coordinator.FinishAction(guicontrol.ActionFinish{
		LeaseID: lease.LeaseID, ActionID: handle.ActionID, Result: &resultCode,
	}); err != nil {
		t.Fatalf("FinishAction: %v", err)
	}
	result, err := guarded.Run(ctx, `{}`)
	if err != nil || !result.IsError || probe.calls != 0 {
		t.Fatalf("result=%+v err=%v calls=%d; revoked authority reached inner", result, err, probe.calls)
	}
}

func TestGUIExecutionGateRejectsDescriptorDriftAfterAdmission(t *testing.T) {
	probe := &guiExecutionGateProbe{descriptor: agent.GUIActionDescriptor{
		Participates: true, ActionKind: "click", Effect: agent.GUIActionMutation,
		TargetBundleID: "com.example.Other",
	}}
	guarded := wrapGUIExecutionGate(probe)
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	lease, err := coordinator.BeginWorkflow(guicontrol.WorkflowRequest{
		SessionID: "session-drift", TurnID: "turn-drift",
		RequestedAppBundleID: "com.example.Editor",
		AllowedAppBundleIDs:  []string{"com.example.Editor"}, PolicySnapshotID: "policy-drift",
	})
	if err != nil {
		t.Fatalf("BeginWorkflow: %v", err)
	}
	handle, err := coordinator.BeginAction(context.Background(), guicontrol.ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolName: "computer_use", ToolUseID: "toolu-drift",
		ActionKind: "click", Effect: guicontrol.ComputerUseActionMutation,
		TargetBundleID: "com.example.Editor",
	})
	if err != nil {
		t.Fatalf("BeginAction: %v", err)
	}
	admitted := guicontrol.ExecutionScope{
		ToolName: "computer_use", ToolUseID: "toolu-drift", ActionKind: "click",
		Effect: string(guicontrol.ComputerUseActionMutation), TargetBundleID: "com.example.Editor",
	}
	ctx := handle.AuthorizeExecution(admitted)
	ctx = agent.ContextWithToolInvocation(ctx, agent.ToolInvocation{
		ToolName: admitted.ToolName, ToolUseID: admitted.ToolUseID,
	})
	result, err := guarded.Run(ctx, `{}`)
	if err != nil || !result.IsError || probe.calls != 0 {
		t.Fatalf("result=%+v err=%v calls=%d; drifted target reached inner", result, err, probe.calls)
	}
}

func TestGUIExecutionGateRejectsDaemonObservationTargetDriftButKeepsDirectObservation(t *testing.T) {
	probe := &guiExecutionGateProbe{descriptor: agent.GUIActionDescriptor{
		Participates: true, ActionKind: "get_app_state", Effect: agent.GUIActionObservation,
		TargetBundleID: "com.example.Other",
	}}
	guarded := wrapGUIExecutionGate(probe)
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	lease, err := coordinator.BeginWorkflow(guicontrol.WorkflowRequest{
		SessionID: "session-observe-drift", TurnID: "turn-observe-drift",
		RequestedAppBundleID: "com.example.Editor",
		AllowedAppBundleIDs:  []string{"com.example.Editor"}, PolicySnapshotID: "policy-observe-drift",
	})
	if err != nil {
		t.Fatalf("BeginWorkflow: %v", err)
	}
	handle, err := coordinator.BeginAction(context.Background(), guicontrol.ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolName: "computer_use", ToolUseID: "toolu-observe-drift",
		ActionKind: "get_app_state", Effect: guicontrol.ComputerUseActionObservation,
		TargetBundleID: "com.example.Editor",
	})
	if err != nil {
		t.Fatalf("BeginAction: %v", err)
	}
	admitted := guicontrol.ExecutionScope{
		ToolName: "computer_use", ToolUseID: "toolu-observe-drift", ActionKind: "get_app_state",
		Effect: string(guicontrol.ComputerUseActionObservation), TargetBundleID: "com.example.Editor",
	}
	ctx := handle.AuthorizeExecution(admitted)
	ctx = agent.ContextWithToolInvocation(ctx, agent.ToolInvocation{
		ToolName: admitted.ToolName, ToolUseID: admitted.ToolUseID,
	})
	result, err := guarded.Run(ctx, `{}`)
	if err != nil || !result.IsError || probe.calls != 0 {
		t.Fatalf("result=%+v err=%v calls=%d; drifted daemon observation reached inner", result, err, probe.calls)
	}

	// The exact same observation remains available to CLI/TUI because those
	// direct contexts carry no daemon claim at all.
	result, err = guarded.Run(context.Background(), `{}`)
	if err != nil || result.IsError || probe.calls != 1 {
		t.Fatalf("direct observation result=%+v err=%v calls=%d", result, err, probe.calls)
	}
}

func TestRegisteredLegacyGUIMutationsAreAllGuarded(t *testing.T) {
	reg, _, cleanup := RegisterLocalTools(nil, nil)
	defer cleanup()
	for _, name := range []string{"computer_use", "computer", "accessibility", "applescript", "ghostty"} {
		tool, ok := reg.Get(name)
		if !ok {
			t.Fatalf("missing registered GUI tool %q", name)
		}
		if _, ok := tool.(guiExecutionGuarded); !ok {
			t.Fatalf("registered GUI tool %q bypasses execution gate: %T", name, tool)
		}
	}
}

func TestCloneWithRuntimeConfigKeepsEveryRegisteredGUIExecutionGate(t *testing.T) {
	reg, _, cleanup := RegisterLocalTools(nil, nil)
	defer cleanup()
	clone := CloneWithRuntimeConfig(reg, nil)
	for _, name := range []string{"computer_use", "computer", "accessibility", "applescript", "ghostty"} {
		tool, ok := clone.Get(name)
		if !ok {
			t.Fatalf("missing cloned GUI tool %q", name)
		}
		if _, ok := tool.(guiExecutionGuarded); !ok {
			t.Fatalf("cloned GUI tool %q bypasses execution gate: %T", name, tool)
		}
	}
}

func TestCompleteRegistrationGuardsRawGUIBaseline(t *testing.T) {
	registry := agent.NewToolRegistry()
	registry.Register(&guiExecutionGateProbe{descriptor: agent.GUIActionDescriptor{
		Participates: true, ActionKind: "click", Effect: agent.GUIActionMutation,
	}})
	completed, _, cleanup, err := CompleteRegistration(context.Background(), nil, &config.Config{}, registry)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("CompleteRegistration: %v", err)
	}
	tool, ok := completed.Get("computer_use")
	if !ok {
		t.Fatal("completed registry lost GUI baseline")
	}
	if _, ok := tool.(guiExecutionGuarded); !ok {
		t.Fatalf("completed raw GUI baseline bypasses gate: %T", tool)
	}
}
