package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/guicontrol"
)

type initialComputerTargetAXFixture struct {
	readPIDs    []int
	focusParams []map[string]any
}

func (f *initialComputerTargetAXFixture) Call(_ context.Context, method string, params any) (json.RawMessage, error) {
	values, _ := params.(map[string]any)
	switch method {
	case "read_tree":
		pid, _ := values["pid"].(int)
		f.readPIDs = append(f.readPIDs, pid)
		app, bundle := "Kocoro Desktop", "run.shannon.shanclaw.dev"
		windowID := 91
		if pid == 4242 {
			app, bundle, windowID = "Calculator", "com.apple.calculator", 17
		}
		payload, _ := json.Marshal(map[string]any{
			"schema_version": 1, "pid": pid, "app": app, "app_name": app,
			"bundle_id": bundle, "window": app, "window_title": app,
			"window_id": windowID, "elements": []any{}, "ref_paths": map[string]any{},
		})
		return payload, nil
	case "focus":
		copy := make(map[string]any, len(values))
		for key, value := range values {
			copy[key] = value
		}
		f.focusParams = append(f.focusParams, copy)
		return json.RawMessage(`{"result":"focused Calculator (pid 4242)"}`), nil
	default:
		return nil, fmt.Errorf("unexpected AX method %q", method)
	}
}

func TestAnthropicComputerInitialTargetUsesForegroundHintAndRestoresBeforeMutation(t *testing.T) {
	fixture := &initialComputerTargetAXFixture{}
	registry := agent.NewToolRegistry()
	registry.Register(wrapGUIExecutionGate(&ComputerUseTool{client: fixture}))
	registry.Register(wrapGUIExecutionGate(&ComputerTool{}))

	cloned, err := CloneWithAnthropicComputerForRun(
		registry,
		nil,
		newAnthropicComputerRunCapabilityAfterVerification(1280, 800),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := BindComputerUseInitialTargetForRun(cloned, ComputerUseInitialTargetV1{
		PID: 4242, AppName: "Calculator", BundleID: "com.apple.calculator",
	}); err != nil {
		t.Fatal(err)
	}

	public, _ := cloned.Get("computer")
	adapter, ok := unwrapGUIExecutionGate(public).(*AnthropicComputerAdapter)
	if !ok {
		t.Fatalf("native computer = %T", unwrapGUIExecutionGate(public))
	}
	descriptor, err := adapter.DescribeNativeToolRequestPreparation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.TargetBundleID != "com.apple.calculator" || descriptor.TargetAppName != "Calculator" {
		t.Fatalf("preparation target = %+v", descriptor)
	}
	if _, err := adapter.raw.Run(context.Background(), `{"action":"get_app_state","description":"observe"}`); err != nil {
		t.Fatal(err)
	}
	if adapter.raw.snapshot == nil || adapter.raw.snapshot.pid != 4242 ||
		adapter.raw.snapshot.bundleID != "com.apple.calculator" {
		t.Fatalf("snapshot = %+v", adapter.raw.snapshot)
	}

	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	lease, err := coordinator.BeginWorkflow(guicontrol.WorkflowRequest{
		SessionID: "session", TurnID: "turn", RequestedAppBundleID: "com.apple.calculator",
		AllowedAppBundleIDs: []string{"com.apple.calculator"},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := guicontrol.ComputerUseExecutionSyntheticCoordinate
	handle, err := coordinator.BeginAction(context.Background(), guicontrol.ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolName: "computer", ToolUseID: "toolu_restore",
		ActionKind: "click", TargetBundleID: "com.apple.calculator", TargetAppName: "Calculator",
		ExecutionPath: &path, Effect: guicontrol.ComputerUseActionMutation,
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := guicontrol.ExecutionScope{
		ToolName: "computer", ToolUseID: "toolu_restore", ActionKind: "click",
		Effect: string(guicontrol.ComputerUseActionMutation), TargetBundleID: "com.apple.calculator",
		ExecutionPath: string(path),
	}
	ctx := agent.ContextWithToolInvocation(handle.AuthorizeExecution(scope), agent.ToolInvocation{
		ToolName: "computer", ToolUseID: "toolu_restore",
	})
	if err := adapter.RestoreGUIActionTargetV1(ctx, agent.GUIActionDescriptor{
		Participates: true, ActionKind: "click", Effect: agent.GUIActionMutation,
		TargetBundleID: "com.apple.calculator", TargetAppName: "Calculator",
		ExecutionPath: string(path),
	}); err != nil {
		t.Fatal(err)
	}
	if len(fixture.focusParams) != 1 || fixture.focusParams[0]["app_name"] != "Calculator" ||
		fixture.focusParams[0]["verify"] != true {
		t.Fatalf("focus calls = %+v", fixture.focusParams)
	}
}

func TestComputerUseInitialTargetRejectsPIDBundleDrift(t *testing.T) {
	fixture := &initialComputerTargetAXFixture{}
	tool := &ComputerUseTool{client: fixture, initialTarget: &ComputerUseInitialTargetV1{
		PID: 4242, AppName: "Calculator", BundleID: "com.example.not-calculator",
	}}
	if _, err := tool.DescribeGUIAction(context.Background(), `{"action":"get_app_state","description":"observe"}`); err == nil {
		t.Fatal("mismatched foreground hint received an execution descriptor")
	}
}
