package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/guicontrol"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

type guiProbeTool struct {
	name       string
	descriptor agent.GUIActionDescriptor
	started    chan struct{}
	finished   chan struct{}
	wait       bool
	calls      int
	outcome    *agent.GUIActionOutcome
}

func (t *guiProbeTool) Info() agent.ToolInfo   { return agent.ToolInfo{Name: t.name} }
func (t *guiProbeTool) RequiresApproval() bool { return true }
func (t *guiProbeTool) IsReadOnlyCall(string) bool {
	return t.descriptor.Effect == agent.GUIActionObservation
}
func (t *guiProbeTool) DescribeGUIAction(context.Context, string) (agent.GUIActionDescriptor, error) {
	return t.descriptor, nil
}
func (t *guiProbeTool) Run(ctx context.Context, _ string) (agent.ToolResult, error) {
	t.calls++
	if t.started != nil {
		close(t.started)
	}
	if t.wait {
		<-ctx.Done()
		if t.finished != nil {
			close(t.finished)
		}
		return agent.BusinessError("probe cancelled"), nil
	}
	if t.finished != nil {
		close(t.finished)
	}
	return agent.ToolResult{Content: "ok", GUIOutcome: t.outcome}, nil
}

type plainProbeTool struct{ calls int }

func (t *plainProbeTool) Info() agent.ToolInfo   { return agent.ToolInfo{Name: "think"} }
func (t *plainProbeTool) RequiresApproval() bool { return false }
func (t *plainProbeTool) Run(context.Context, string) (agent.ToolResult, error) {
	t.calls++
	return agent.ToolResult{Content: "plain"}, nil
}

type restoringGUIProbeTool struct {
	guiProbeTool
	restored          bool
	restoreAuthorized bool
	runAfterRestore   bool
}

func (t *restoringGUIProbeTool) RestoreGUIActionTargetV1(
	ctx context.Context,
	_ agent.GUIActionDescriptor,
) error {
	t.restored = true
	t.restoreAuthorized = guicontrol.ExecutionAuthorityPresent(ctx)
	return nil
}

func (t *restoringGUIProbeTool) Run(context.Context, string) (agent.ToolResult, error) {
	t.calls++
	t.runAfterRestore = t.restored
	return agent.ToolResult{Content: "ok"}, nil
}

type daemonNativeAllTraitsProbe struct {
	guiProbeTool
	lastArgs     string
	prepareCalls int
}

func (t *daemonNativeAllTraitsProbe) NativeToolDef() *client.NativeToolDef {
	return &client.NativeToolDef{
		Type:            client.NativeComputerToolType,
		Name:            client.NativeComputerToolName,
		DisplayWidthPx:  1024,
		DisplayHeightPx: 768,
	}
}

func (t *daemonNativeAllTraitsProbe) PrepareNativeToolRequest(context.Context) error {
	t.prepareCalls++
	return nil
}

func (t *daemonNativeAllTraitsProbe) DescribeNativeToolRequestPreparation(
	context.Context,
) (agent.GUIActionDescriptor, error) {
	return t.descriptor, nil
}

func (t *daemonNativeAllTraitsProbe) IsSafeArgs(args string) bool {
	return args == `{"safe":true}`
}

func (t *daemonNativeAllTraitsProbe) IsReadOnlyCall(args string) bool {
	return args == `{"read_only":true}`
}

func (t *daemonNativeAllTraitsProbe) IsConcurrencySafeCall(args string) bool {
	return args == `{"concurrent":true}`
}

func (t *daemonNativeAllTraitsProbe) Run(_ context.Context, args string) (agent.ToolResult, error) {
	t.calls++
	t.lastArgs = args
	return agent.ToolResult{Content: "native probe reached"}, nil
}

type typedCancellationProbeTool struct {
	started    chan struct{}
	descriptor agent.GUIActionDescriptor
	outcome    *agent.GUIActionOutcome
}

func (t *typedCancellationProbeTool) Info() agent.ToolInfo {
	return agent.ToolInfo{Name: "computer_use"}
}
func (t *typedCancellationProbeTool) RequiresApproval() bool { return true }
func (t *typedCancellationProbeTool) DescribeGUIAction(context.Context, string) (agent.GUIActionDescriptor, error) {
	return t.descriptor, nil
}
func (t *typedCancellationProbeTool) Run(ctx context.Context, _ string) (agent.ToolResult, error) {
	close(t.started)
	<-ctx.Done()
	result := agent.BusinessError("typed helper cancellation acknowledgement")
	result.GUIOutcome = t.outcome
	return result, nil
}

type consequentialRiskProbeTool struct {
	draft tools.ConsequentialRiskDraftV1
	calls int
}

func (t *consequentialRiskProbeTool) Info() agent.ToolInfo {
	return agent.ToolInfo{Name: "computer_use"}
}
func (t *consequentialRiskProbeTool) RequiresApproval() bool { return true }
func (t *consequentialRiskProbeTool) DescribeGUIAction(context.Context, string) (agent.GUIActionDescriptor, error) {
	return agent.GUIActionDescriptor{
		Participates: true, ActionKind: t.draft.Target.ActionKind, Effect: agent.GUIActionMutation,
		TargetBundleID: t.draft.Target.BundleID, TargetAppName: t.draft.Target.AppName,
		ExecutionPath: t.draft.Target.ExecutionPath,
	}, nil
}
func (t *consequentialRiskProbeTool) PreflightConsequentialRiskV1(context.Context, string, string) (tools.ConsequentialRiskPreflightResultV1, error) {
	draft := t.draft
	return tools.ConsequentialRiskPreflightResultV1{Status: tools.ConsequentialRiskPreflightRequiredV1, Draft: &draft}, nil
}
func (t *consequentialRiskProbeTool) Run(context.Context, string) (agent.ToolResult, error) {
	t.calls++
	return agent.ToolResult{Content: "ok"}, nil
}

func testGUIWorkflow(coordinator *guicontrol.Coordinator, sessionID, turnID string) *daemonGUIWorkflow {
	workflow := newDaemonGUIWorkflow(coordinator, daemonGUIWorkflowRequest{
		SessionID:   sessionID,
		TurnID:      turnID,
		SourceKind:  "desktop",
		SourceLabel: "Kocoro Desktop",
	})
	workflow.invocationFromContext = func(context.Context) (agent.ToolInvocation, bool) {
		return agent.ToolInvocation{ToolUseID: "toolu_exact_123"}, true
	}
	return workflow
}

func TestDaemonGUIToolAppPolicyAdmission(t *testing.T) {
	store := NewComputerUseAppPolicyStore(t.TempDir())
	if _, err := store.Update("com.example.blocked", ComputerUseAppPolicyBlocked); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("com.example.ask", ComputerUseAppPolicyAsk); err != nil {
		t.Fatal(err)
	}
	workflow := testGUIWorkflow(guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{}), "sess-policy", "turn-policy")
	workflow.appPolicy = store

	tests := []struct {
		name       string
		descriptor agent.GUIActionDescriptor
		want       agent.ApprovalAdmissionDecision
	}{
		{
			name: "blocked exact bundle",
			descriptor: agent.GUIActionDescriptor{Participates: true, ActionKind: "click", Effect: agent.GUIActionMutation,
				TargetBundleID: "COM.EXAMPLE.BLOCKED"},
			want: agent.ApprovalAdmissionDeny,
		},
		{
			name: "built-in terminal",
			descriptor: agent.GUIActionDescriptor{Participates: true, ActionKind: "press", Effect: agent.GUIActionMutation,
				TargetBundleID: "com.apple.Terminal"},
			want: agent.ApprovalAdmissionDeny,
		},
		{
			name: "ordinary app inherits global tool permission",
			descriptor: agent.GUIActionDescriptor{Participates: true, ActionKind: "click", Effect: agent.GUIActionMutation,
				TargetBundleID: "com.tinyspeck.slackmacgap"},
			want: agent.ApprovalAdmissionInherit,
		},
		{
			name: "legacy explicit ask cannot override global tool permission",
			descriptor: agent.GUIActionDescriptor{Participates: true, ActionKind: "click", Effect: agent.GUIActionMutation,
				TargetBundleID: "com.example.ask"},
			want: agent.ApprovalAdmissionInherit,
		},
		{
			name:       "uncertain target stays ask",
			descriptor: agent.GUIActionDescriptor{Participates: true, ActionKind: "hotkey", Effect: agent.GUIActionMutation},
			want:       agent.ApprovalAdmissionRequireFresh,
		},
		{
			name: "blocked observation",
			descriptor: agent.GUIActionDescriptor{Participates: true, ActionKind: "get_app_state", Effect: agent.GUIActionObservation,
				TargetBundleID: "com.apple.Terminal"},
			want: agent.ApprovalAdmissionDeny,
		},
		{
			name: "ordinary observation unchanged",
			descriptor: agent.GUIActionDescriptor{Participates: true, ActionKind: "get_app_state", Effect: agent.GUIActionObservation,
				TargetBundleID: "com.tinyspeck.slackmacgap"},
			want: agent.ApprovalAdmissionInherit,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tool := &guiProbeTool{name: "computer_use", descriptor: test.descriptor}
			wrapped := &daemonGUIToolBase{inner: tool, workflow: workflow}
			if got := wrapped.ApprovalAdmission(context.Background(), `{}`); got != test.want {
				t.Fatalf("ApprovalAdmission = %q, want %q", got, test.want)
			}
		})
	}
}

type driftingGUIProbeTool struct {
	descriptors []agent.GUIActionDescriptor
	calls       int
	runs        int
}

func (t *driftingGUIProbeTool) Info() agent.ToolInfo   { return agent.ToolInfo{Name: "computer_use"} }
func (t *driftingGUIProbeTool) RequiresApproval() bool { return true }
func (t *driftingGUIProbeTool) DescribeGUIAction(context.Context, string) (agent.GUIActionDescriptor, error) {
	index := t.calls
	if index >= len(t.descriptors) {
		index = len(t.descriptors) - 1
	}
	t.calls++
	return t.descriptors[index], nil
}
func (t *driftingGUIProbeTool) Run(context.Context, string) (agent.ToolResult, error) {
	t.runs++
	return agent.ToolResult{Content: "unexpected"}, nil
}

func TestDaemonGUIToolRechecksPolicyAfterApprovalDescriptorDrift(t *testing.T) {
	store := NewComputerUseAppPolicyStore(t.TempDir())
	workflow := testGUIWorkflow(guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{}), "sess-drift", "turn-drift")
	workflow.appPolicy = store
	tool := &driftingGUIProbeTool{descriptors: []agent.GUIActionDescriptor{
		{Participates: true, ActionKind: "click", Effect: agent.GUIActionMutation, TargetBundleID: "com.example.editor"},
		{Participates: true, ActionKind: "click", Effect: agent.GUIActionMutation, TargetBundleID: "com.apple.terminal"},
	}}
	wrapped := &daemonGUIToolBase{inner: tool, workflow: workflow}
	if got := wrapped.ApprovalAdmission(context.Background(), `{}`); got != agent.ApprovalAdmissionInherit {
		t.Fatalf("initial admission = %q, want global-permission inheritance", got)
	}
	result, err := workflow.runTool(context.Background(), tool, `{}`)
	if err != nil || !result.IsError || !strings.Contains(result.Content, "app policy blocked") {
		t.Fatalf("drift result = %+v, err=%v", result, err)
	}
	if tool.runs != 0 {
		t.Fatalf("blocked drift reached inner tool %d times", tool.runs)
	}
	if active := workflow.coordinator.Snapshot().Active; active != nil {
		t.Fatalf("blocked drift created workflow lease: %+v", active)
	}
}

func waitForActiveLease(t *testing.T, coordinator *guicontrol.Coordinator) guicontrol.ComputerUseActivityState {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if active := coordinator.Snapshot().Active; active != nil {
			return *active
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for GUI workflow lease")
	return guicontrol.ComputerUseActivityState{}
}

func acknowledgeController(t *testing.T, coordinator *guicontrol.Coordinator) guicontrol.ComputerUseActivityState {
	t.Helper()
	active := waitForActiveLease(t, coordinator)
	if _, err := coordinator.Heartbeat(active.LeaseID); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	return active
}

func runGUIWorkflowWithControllerAck(
	t *testing.T,
	coordinator *guicontrol.Coordinator,
	workflow *daemonGUIWorkflow,
	tool agent.Tool,
	args string,
) agent.ToolResult {
	t.Helper()
	done := make(chan agent.ToolResult, 1)
	go func() {
		result, _ := workflow.runTool(context.Background(), tool, args)
		done <- result
	}()
	acknowledgeController(t, coordinator)
	select {
	case result := <-done:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for daemon GUI tool after controller acknowledgement")
		return agent.ToolResult{}
	}
}

func TestDaemonGUIWorkflowRestoresTargetAfterAuthorityBeforeMutation(t *testing.T) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{
		RequireControllerHeartbeat: true,
		LeaseTTL:                   5 * time.Second,
	})
	workflow := testGUIWorkflow(coordinator, "sess-restore", "turn-restore")
	tool := &restoringGUIProbeTool{guiProbeTool: guiProbeTool{
		name: "computer_use",
		descriptor: agent.GUIActionDescriptor{
			Participates: true, ActionKind: "click", Effect: agent.GUIActionMutation,
			TargetBundleID: "com.apple.calculator", TargetAppName: "Calculator",
			ExecutionPath: "synthetic_coordinate",
		},
	}}

	result := runGUIWorkflowWithControllerAck(t, coordinator, workflow, tool, `{}`)
	if result.IsError || tool.calls != 1 || !tool.restored ||
		!tool.restoreAuthorized || !tool.runAfterRestore {
		t.Fatalf("result=%+v tool=%+v", result, tool)
	}
	workflow.EndTurn()
}

func TestDaemonGUIWorkflowBusyRejectsConcurrentRun(t *testing.T) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{
		RequireControllerHeartbeat: true,
		LeaseTTL:                   5 * time.Second,
	})
	first := testGUIWorkflow(coordinator, "sess-1", "turn-1")
	firstTool := &guiProbeTool{
		name: "computer_use", wait: true, started: make(chan struct{}),
		descriptor: agent.GUIActionDescriptor{
			Participates: true, ActionKind: "click", Effect: agent.GUIActionMutation,
			TargetBundleID: "com.example.Notes", TargetAppName: "Notes",
		},
	}
	firstDone := make(chan agent.ToolResult, 1)
	go func() {
		result, _ := first.runTool(context.Background(), firstTool, `{}`)
		firstDone <- result
	}()
	firstLease := acknowledgeController(t, coordinator)
	select {
	case <-firstTool.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first action never started")
	}

	second := testGUIWorkflow(coordinator, "sess-2", "turn-2")
	secondTool := &guiProbeTool{
		name: "accessibility",
		descriptor: agent.GUIActionDescriptor{
			Participates: true, ActionKind: "press", Effect: agent.GUIActionMutation,
			TargetBundleID: "com.example.Slack", TargetAppName: "Slack",
		},
	}
	result, err := second.runTool(context.Background(), secondTool, `{}`)
	if err != nil || !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness ||
		!strings.Contains(result.Content, "computer_use_busy") {
		t.Fatalf("second run result=%+v err=%v; want stable busy business error", result, err)
	}

	if _, err := coordinator.Control(guicontrol.ComputerUseControlRequest{
		LeaseID: firstLease.LeaseID, Action: guicontrol.ComputerUseControlStop,
		IdempotencyKey: "stop-first",
	}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled first action did not return")
	}
}

func TestDaemonGUIWorkflowStopCancelsToolAndAcknowledgesBeforeTerminal(t *testing.T) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{
		RequireControllerHeartbeat: true,
		LeaseTTL:                   5 * time.Second,
	})
	workflow := testGUIWorkflow(coordinator, "sess-stop", "turn-stop")
	tool := &guiProbeTool{
		name: "computer", wait: true, started: make(chan struct{}), finished: make(chan struct{}),
		descriptor: agent.GUIActionDescriptor{
			Participates: true, ActionKind: "hotkey", Effect: agent.GUIActionMutation,
			TargetBundleID: "com.example.Editor", TargetAppName: "Editor",
		},
	}
	done := make(chan agent.ToolResult, 1)
	go func() {
		result, _ := workflow.runTool(context.Background(), tool, `{}`)
		done <- result
	}()
	lease := acknowledgeController(t, coordinator)
	<-tool.started

	if _, err := coordinator.Control(guicontrol.ComputerUseControlRequest{
		LeaseID: lease.LeaseID, Action: guicontrol.ComputerUseControlStop,
		IdempotencyKey: "stop-action",
	}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if active := coordinator.Snapshot().Active; active == nil || active.LeaseState != guicontrol.ComputerUseLeaseStopping {
		t.Fatalf("Stop cleared lease before executor acknowledgement: %+v", active)
	}
	select {
	case <-tool.finished:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop context did not reach tool")
	}
	result := <-done
	if !result.IsError || !strings.Contains(result.Content, "commit status is unknown") || !strings.Contains(result.Content, "do not retry") {
		t.Fatalf("cancelled result=%+v", result)
	}
	if active := coordinator.Snapshot().Active; active != nil {
		t.Fatalf("FinishAction did not acknowledge cancellation: %+v", active)
	}
	workflow.EndTurn()
}

func TestDaemonGUIWorkflowStopPreservesTypedCancellationAcknowledgement(t *testing.T) {
	var eventsMu sync.Mutex
	var events []guicontrol.ComputerUseActivityEvent
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{
		RequireControllerHeartbeat: true,
		LeaseTTL:                   5 * time.Second,
		Sink: func(event guicontrol.ComputerUseActivityEvent) {
			eventsMu.Lock()
			events = append(events, event)
			eventsMu.Unlock()
		},
	})
	workflow := testGUIWorkflow(coordinator, "sess-scroll-stop", "turn-scroll-stop")
	tool := &typedCancellationProbeTool{
		started: make(chan struct{}),
		descriptor: agent.GUIActionDescriptor{
			Participates: true, ActionKind: "scroll", Effect: agent.GUIActionMutation,
			TargetBundleID: "com.apple.Notes", TargetAppName: "Notes",
			ExecutionPath: "accessibility",
		},
		outcome: &agent.GUIActionOutcome{
			Result: agent.GUIActionResultCancelled,
			Phase:  agent.GUIActionPhaseInputCommitted, FailureCode: "controller_cancelled",
		},
	}
	done := make(chan agent.ToolResult, 1)
	go func() {
		result, _ := workflow.runTool(context.Background(), tool, `{}`)
		done <- result
	}()
	lease := acknowledgeController(t, coordinator)
	<-tool.started
	if _, err := coordinator.Control(guicontrol.ComputerUseControlRequest{
		LeaseID: lease.LeaseID, Action: guicontrol.ComputerUseControlStop,
		IdempotencyKey: "stop-typed-scroll",
	}); err != nil {
		t.Fatal(err)
	}
	result := <-done
	if result.GUIOutcome == nil ||
		result.GUIOutcome.Result != agent.GUIActionResultCancelled ||
		result.GUIOutcome.Phase != agent.GUIActionPhaseInputCommitted ||
		result.GUIOutcome.FailureCode != "controller_cancelled" {
		t.Fatalf("typed cancellation was overwritten: %+v", result)
	}
	eventsMu.Lock()
	terminal := events[len(events)-1]
	eventsMu.Unlock()
	if terminal.LeaseState != guicontrol.ComputerUseLeaseTerminal ||
		terminal.ActionResult == nil || *terminal.ActionResult != guicontrol.ComputerUseResultCancelled ||
		terminal.ActionPhase != guicontrol.ComputerUsePhaseInputCommitted ||
		terminal.FailureCode == nil || *terminal.FailureCode != "controller_cancelled" {
		t.Fatalf("typed cancellation did not reach terminal event: %+v", terminal)
	}
}

func TestDaemonGUIWorkflowPublishesTypedOutcomeAndAuthoritativePointer(t *testing.T) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{
		RequireControllerHeartbeat: true,
		LeaseTTL:                   5 * time.Second,
	})
	workflow := testGUIWorkflow(coordinator, "sess-outcome", "turn-outcome")
	tool := &guiProbeTool{
		name: "computer_use",
		descriptor: agent.GUIActionDescriptor{
			Participates: true, ActionKind: "move", Effect: agent.GUIActionMutation,
			TargetBundleID: "com.example.Editor", TargetAppName: "Editor",
			ExecutionPath: "synthetic_coordinate",
		},
		outcome: &agent.GUIActionOutcome{
			Result:      agent.GUIActionResultVerified,
			Phase:       agent.GUIActionPhaseVerifying,
			FailureCode: "",
			Pointer: &agent.GUIActionPointer{
				DisplayID: 7, TopologyID: "topology-7", TopologyGeneration: 9,
				X: -120.5, Y: 440.25,
			},
		},
	}
	done := make(chan agent.ToolResult, 1)
	go func() {
		result, _ := workflow.runTool(context.Background(), tool, `{}`)
		done <- result
	}()
	acknowledgeController(t, coordinator)
	result := <-done
	if result.IsError {
		t.Fatalf("runTool result=%+v", result)
	}
	active := coordinator.Snapshot().Active
	if active == nil || active.ActionResult == nil || *active.ActionResult != guicontrol.ComputerUseResultVerified {
		t.Fatalf("typed result not published: %+v", active)
	}
	if active.ActionPhase != guicontrol.ComputerUsePhaseVerifying || active.Pointer == nil ||
		active.Pointer.DisplayID != 7 || active.Pointer.TopologyID != "topology-7" ||
		active.Pointer.TopologyGeneration != 9 || active.Pointer.X != -120.5 || active.Pointer.Y != 440.25 {
		t.Fatalf("typed phase/pointer not published: %+v", active)
	}
	workflow.EndTurn()
}

func TestDaemonGUIWorkflowSemanticSelectionUserInterferenceAutoPauses(t *testing.T) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{
		RequireControllerHeartbeat: true,
		LeaseTTL:                   5 * time.Second,
	})
	workflow := testGUIWorkflow(coordinator, "sess-selection-interference", "turn-selection-interference")
	tool := &guiProbeTool{
		name: "computer_use",
		descriptor: agent.GUIActionDescriptor{
			Participates: true, ActionKind: "select_text", Effect: agent.GUIActionMutation,
			TargetBundleID: "com.apple.Notes", TargetAppName: "Notes",
			ExecutionPath: "accessibility",
		},
		outcome: &agent.GUIActionOutcome{
			Result: agent.GUIActionResultUserInterference, Phase: agent.GUIActionPhaseActing,
			FailureCode: "physical_input_interference",
		},
	}
	result := runGUIWorkflowWithControllerAck(t, coordinator, workflow, tool, `{}`)
	if result.GUIOutcome == nil {
		t.Fatalf("selection interference result=%+v", result)
	}
	active := coordinator.Snapshot().Active
	if active == nil || active.LeaseState != guicontrol.ComputerUseLeasePaused ||
		active.ActionPhase != guicontrol.ComputerUsePhaseWaitingForUser ||
		active.ActionResult == nil || *active.ActionResult != guicontrol.ComputerUseResultUserInterference ||
		active.FailureCode == nil || *active.FailureCode != "physical_input_interference" {
		t.Fatalf("selection interference did not auto-pause through daemon FinishAction: %+v", active)
	}
	if _, err := coordinator.Control(guicontrol.ComputerUseControlRequest{
		LeaseID: active.LeaseID, Action: guicontrol.ComputerUseControlResume,
		IdempotencyKey: "resume-selection-interference",
	}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	nextMutation := &guiProbeTool{
		name: "computer_use",
		descriptor: agent.GUIActionDescriptor{
			Participates: true, ActionKind: "click", Effect: agent.GUIActionMutation,
			TargetBundleID: "com.apple.Notes", TargetAppName: "Notes",
			ExecutionPath: "accessibility",
		},
	}
	blocked, err := workflow.runTool(context.Background(), nextMutation, `{}`)
	if err != nil || !blocked.IsError || !strings.Contains(blocked.Content, "requires a verified observation") {
		t.Fatalf("post-interference mutation=%+v err=%v", blocked, err)
	}
	if nextMutation.calls != 0 {
		t.Fatalf("post-interference mutation reached executor %d times", nextMutation.calls)
	}
	workflow.EndTurn()
}

func TestDaemonGUIWorkflowSemanticSelectionCommitRequiresReobservation(t *testing.T) {
	for _, test := range []struct {
		name        string
		result      agent.GUIActionResult
		failureCode string
		want        guicontrol.ComputerUseActionResult
	}{
		{name: "verified", result: agent.GUIActionResultVerified, want: guicontrol.ComputerUseResultVerified},
		{
			name: "completed_unverified", result: agent.GUIActionResultCompletedUnverified,
			failureCode: "interference_detection_unavailable",
			want:        guicontrol.ComputerUseResultCompletedUnverified,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{
				RequireControllerHeartbeat: true,
				LeaseTTL:                   5 * time.Second,
			})
			workflow := testGUIWorkflow(coordinator, "sess-selection-"+test.name, "turn-selection-"+test.name)
			selection := &guiProbeTool{
				name: "computer_use",
				descriptor: agent.GUIActionDescriptor{
					Participates: true, ActionKind: "select_text", Effect: agent.GUIActionMutation,
					TargetBundleID: "com.apple.Notes", TargetAppName: "Notes",
					ExecutionPath: "accessibility",
				},
				outcome: &agent.GUIActionOutcome{
					Result: test.result, Phase: agent.GUIActionPhaseVerifying,
					FailureCode: test.failureCode,
				},
			}
			result := runGUIWorkflowWithControllerAck(t, coordinator, workflow, selection, `{}`)
			if result.GUIOutcome == nil {
				t.Fatalf("selection result=%+v", result)
			}
			active := coordinator.Snapshot().Active
			if active == nil || active.ActionResult == nil || *active.ActionResult != test.want {
				t.Fatalf("selection typed result did not reach daemon FinishAction: %+v", active)
			}
			nextMutation := &guiProbeTool{
				name: "computer_use",
				descriptor: agent.GUIActionDescriptor{
					Participates: true, ActionKind: "click", Effect: agent.GUIActionMutation,
					TargetBundleID: "com.apple.Notes", TargetAppName: "Notes",
					ExecutionPath: "accessibility",
				},
			}
			blocked, err := workflow.runTool(context.Background(), nextMutation, `{}`)
			if err != nil || !blocked.IsError || !strings.Contains(blocked.Content, "requires a verified observation") {
				t.Fatalf("post-selection mutation=%+v err=%v", blocked, err)
			}
			if nextMutation.calls != 0 {
				t.Fatalf("post-selection mutation reached executor %d times", nextMutation.calls)
			}
			workflow.EndTurn()
		})
	}
}

func TestDaemonGUIWorkflowEndTurnReleasesLease(t *testing.T) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{
		RequireControllerHeartbeat: true,
		LeaseTTL:                   5 * time.Second,
	})
	workflow := testGUIWorkflow(coordinator, "sess-end", "turn-end")
	tool := &guiProbeTool{
		name: "computer_use",
		descriptor: agent.GUIActionDescriptor{
			Participates: true, ActionKind: "get_app_state", Effect: agent.GUIActionObservation,
			TargetBundleID: "com.example.Notes", TargetAppName: "Notes",
		},
	}
	done := make(chan agent.ToolResult, 1)
	go func() {
		result, _ := workflow.runTool(context.Background(), tool, `{}`)
		done <- result
	}()
	acknowledgeController(t, coordinator)
	if result := <-done; result.IsError {
		t.Fatalf("observation result=%+v", result)
	}
	if active := coordinator.Snapshot().Active; active == nil || active.ToolUseID != "toolu_exact_123" || active.ActionKind != "get_app_state" {
		t.Fatalf("activity lost exact tool invocation identity: %+v", active)
	}
	workflow.EndTurn()
	if active := coordinator.Snapshot().Active; active != nil {
		t.Fatalf("EndTurn retained active lease: %+v", active)
	}
	next := testGUIWorkflow(coordinator, "sess-next", "turn-next")
	nextDone := make(chan agent.ToolResult, 1)
	go func() {
		result, _ := next.runTool(context.Background(), tool, `{}`)
		nextDone <- result
	}()
	acknowledgeController(t, coordinator)
	if result := <-nextDone; result.IsError {
		t.Fatalf("next workflow remained busy: %+v", result)
	}
	next.EndTurn()
}

func TestDaemonGUIWorkflowDeniesCrossAppMutationAfterFrozenFirstTarget(t *testing.T) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{
		RequireControllerHeartbeat: true,
		LeaseTTL:                   5 * time.Second,
	})
	workflow := testGUIWorkflow(coordinator, "sess-cross-app", "turn-cross-app")
	observe := &guiProbeTool{
		name: "computer_use",
		descriptor: agent.GUIActionDescriptor{
			Participates: true, ActionKind: "get_app_state", Effect: agent.GUIActionObservation,
			TargetBundleID: "com.example.Notes", TargetAppName: "Notes",
		},
	}
	observed := make(chan agent.ToolResult, 1)
	go func() {
		result, _ := workflow.runTool(context.Background(), observe, `{}`)
		observed <- result
	}()
	acknowledgeController(t, coordinator)
	if result := <-observed; result.IsError {
		t.Fatalf("initial observation=%+v", result)
	}
	mutate := &guiProbeTool{
		name: "accessibility",
		descriptor: agent.GUIActionDescriptor{
			Participates: true, ActionKind: "press", Effect: agent.GUIActionMutation,
			TargetBundleID: "com.example.Slack", TargetAppName: "Slack",
		},
	}
	result, err := workflow.runTool(context.Background(), mutate, `{}`)
	if err != nil || !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness || !strings.Contains(result.Content, "outside the frozen allowlist") {
		t.Fatalf("cross-app mutation result=%+v err=%v", result, err)
	}
	if mutate.calls != 0 {
		t.Fatalf("cross-app mutation reached legacy tool: calls=%d", mutate.calls)
	}
	workflow.EndTurn()
}

func TestDaemonGUIWorkflowUnresolvedFirstObservationFreezesEmptyAllowlist(t *testing.T) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{
		RequireControllerHeartbeat: true,
		LeaseTTL:                   5 * time.Second,
	})
	workflow := testGUIWorkflow(coordinator, "sess-empty-policy", "turn-empty-policy")
	observe := &guiProbeTool{
		name: "computer_use",
		descriptor: agent.GUIActionDescriptor{
			Participates: true, ActionKind: "screenshot", Effect: agent.GUIActionObservation,
		},
	}
	done := make(chan agent.ToolResult, 1)
	go func() {
		result, _ := workflow.runTool(context.Background(), observe, `{}`)
		done <- result
	}()
	acknowledgeController(t, coordinator)
	if result := <-done; result.IsError {
		t.Fatalf("targetless observation=%+v", result)
	}
	mutate := &guiProbeTool{
		name: "computer",
		descriptor: agent.GUIActionDescriptor{
			Participates: true, ActionKind: "click", Effect: agent.GUIActionMutation,
			TargetBundleID: "com.example.Notes", TargetAppName: "Notes",
		},
	}
	result, _ := workflow.runTool(context.Background(), mutate, `{}`)
	if !result.IsError || !strings.Contains(result.Content, "outside the frozen allowlist") || mutate.calls != 0 {
		t.Fatalf("mutation after unresolved observation=%+v calls=%d", result, mutate.calls)
	}
	workflow.EndTurn()
}

func TestDaemonGUIWorkflowCancelledControllerWaitDoesNotStrandLease(t *testing.T) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{
		RequireControllerHeartbeat: true,
		LeaseTTL:                   5 * time.Second,
	})
	workflow := testGUIWorkflow(coordinator, "sess-cancel-wait", "turn-cancel-wait")
	tool := &guiProbeTool{
		name: "computer_use",
		descriptor: agent.GUIActionDescriptor{
			Participates: true, ActionKind: "get_app_state", Effect: agent.GUIActionObservation,
			TargetBundleID: "com.example.Notes", TargetAppName: "Notes",
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan agent.ToolResult, 1)
	go func() {
		result, _ := workflow.runTool(ctx, tool, `{}`)
		done <- result
	}()
	waitForActiveLease(t, coordinator)
	cancel()
	result := <-done
	if !result.IsError {
		t.Fatalf("cancelled controller wait result=%+v", result)
	}
	if active := coordinator.Snapshot().Active; active != nil {
		t.Fatalf("cancelled controller wait stranded lease: %+v", active)
	}
}

func TestDaemonGUIWorkflowSkipsNonGUIAndGhosttyListTabs(t *testing.T) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{RequireControllerHeartbeat: true})
	workflow := testGUIWorkflow(coordinator, "sess-read", "turn-read")
	plain := &plainProbeTool{}
	result, err := workflow.runTool(context.Background(), plain, `{}`)
	if err != nil || result.Content != "plain" || plain.calls != 1 {
		t.Fatalf("plain result=%+v err=%v calls=%d", result, err, plain.calls)
	}
	ghostty := &tools.GhosttyTool{}
	descriptor, err := ghostty.DescribeGUIAction(context.Background(), `{"action":"list_tabs","description":"list"}`)
	if err != nil || descriptor.Participates || descriptor.Effect != agent.GUIActionObservation {
		t.Fatalf("ghostty list_tabs descriptor=%+v err=%v", descriptor, err)
	}
	if coordinator.Snapshot().Active != nil {
		t.Fatal("non-GUI reads acquired a GUI workflow lease")
	}
	workflow.EndTurn()
}

func TestWrapDaemonGUIToolsPreservesSchemasAndSafetyTraits(t *testing.T) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(coordinator, "sess-traits", "turn-traits")
	reg := agent.NewToolRegistry()
	originalComputer := &tools.ComputerTool{}
	wantInfo := originalComputer.Info()
	reg.Register(originalComputer)
	reg.Register(&tools.ComputerUseTool{})
	reg.Register(&tools.AccessibilityTool{})
	reg.Register(&tools.AppleScriptTool{})
	reg.Register(&tools.GhosttyTool{})
	wrapDaemonGUITools(reg, workflow)

	computer, _ := reg.Get("computer")
	if _, ok := computer.(agent.NativeToolProvider); ok {
		t.Fatalf("legacy computer regained provider-native identity after daemon wrapping: %T", computer)
	}
	schemas := reg.Schemas()
	if len(schemas) == 0 {
		t.Fatal("wrapped legacy function schema is missing")
	}
	var computerSchema *client.Tool
	for index := range schemas {
		if schemas[index].Type == "function" && schemas[index].Function.Name == wantInfo.Name {
			computerSchema = &schemas[index]
			break
		}
	}
	if computerSchema == nil || computerSchema.Function.Description != wantInfo.Description ||
		!reflect.DeepEqual(computerSchema.Function.Parameters["required"], wantInfo.Required) {
		t.Fatalf("wrapped legacy function schema=%+v want info=%+v", computerSchema, wantInfo)
	}

	for _, name := range []string{"computer_use", "accessibility"} {
		wrapped, _ := reg.Get(name)
		if _, ok := wrapped.(agent.SafeChecker); !ok {
			t.Fatalf("%s lost SafeChecker", name)
		}
		if _, ok := wrapped.(agent.ReadOnlyChecker); !ok {
			t.Fatalf("%s lost ReadOnlyChecker", name)
		}
		if _, ok := wrapped.(agent.ConcurrencySafeChecker); !ok {
			t.Fatalf("%s lost ConcurrencySafeChecker", name)
		}
	}
	if wrapped, _ := reg.Get("applescript"); wrapped.RequiresApproval() != true {
		t.Fatal("wrapped applescript changed approval semantics")
	}
}

func TestWrapDaemonGUIToolsPreservesEveryNativeToolTraitAndRunDelegation(t *testing.T) {
	workflow := testGUIWorkflow(
		guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{}),
		"sess-native-traits",
		"turn-native-traits",
	)
	probe := &daemonNativeAllTraitsProbe{guiProbeTool: guiProbeTool{
		name: "computer",
		descriptor: agent.GUIActionDescriptor{
			Participates: true, ActionKind: "get_app_state",
			Effect:         agent.GUIActionObservation,
			TargetBundleID: "com.example.Editor", TargetAppName: "Editor",
			ExecutionPath: "accessibility",
		},
	}}
	reg := agent.NewToolRegistry()
	reg.Register(probe)

	wrapDaemonGUITools(reg, workflow)
	wrapped, ok := reg.Get("computer")
	if !ok {
		t.Fatal("wrapped native probe was not registered")
	}

	native, ok := wrapped.(agent.NativeToolProvider)
	if !ok {
		t.Fatalf("wrapped native tool lost NativeToolProvider: %T", wrapped)
	}
	if def := native.NativeToolDef(); def == nil || def.DisplayWidthPx != 1024 || def.DisplayHeightPx != 768 {
		t.Fatalf("wrapped native definition = %+v, want 1024x768", def)
	}
	preparer, ok := wrapped.(agent.NativeToolRequestPreparer)
	if !ok {
		t.Fatalf("wrapped native tool lost NativeToolRequestPreparer: %T", wrapped)
	}
	prepareDone := make(chan error, 1)
	go func() {
		prepareDone <- preparer.PrepareNativeToolRequest(context.Background())
	}()
	acknowledgeController(t, workflow.coordinator)
	if err := <-prepareDone; err != nil || probe.prepareCalls != 1 {
		t.Fatalf("wrapped native preparation err=%v calls=%d", err, probe.prepareCalls)
	}
	safe, ok := wrapped.(agent.SafeChecker)
	if !ok || !safe.IsSafeArgs(`{"safe":true}`) || safe.IsSafeArgs(`{"safe":false}`) {
		t.Fatalf("wrapped native tool lost exact SafeChecker delegation: %T", wrapped)
	}
	readOnly, ok := wrapped.(agent.ReadOnlyChecker)
	if !ok || !readOnly.IsReadOnlyCall(`{"read_only":true}`) || readOnly.IsReadOnlyCall(`{"read_only":false}`) {
		t.Fatalf("wrapped native tool lost exact ReadOnlyChecker delegation: %T", wrapped)
	}
	concurrency, ok := wrapped.(agent.ConcurrencySafeChecker)
	if !ok || !concurrency.IsConcurrencySafeCall(`{"concurrent":true}`) || concurrency.IsConcurrencySafeCall(`{"concurrent":false}`) {
		t.Fatalf("wrapped native tool lost exact ConcurrencySafeChecker delegation: %T", wrapped)
	}

	const args = `{"delegated":true}`
	result, err := wrapped.Run(context.Background(), args)
	if err != nil || result.Content != "native probe reached" || probe.calls != 1 || probe.lastArgs != args {
		t.Fatalf("wrapped native Run result=%+v err=%v calls=%d args=%q", result, err, probe.calls, probe.lastArgs)
	}
}

func TestDaemonGUIWorkflowAppleScriptFailsClosedWithoutLeakingScript(t *testing.T) {
	var mu sync.Mutex
	var events []guicontrol.ComputerUseActivityEvent
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{
		RequireControllerHeartbeat: true,
		LeaseTTL:                   5 * time.Second,
		Sink: func(event guicontrol.ComputerUseActivityEvent) {
			mu.Lock()
			events = append(events, event)
			mu.Unlock()
		},
	})
	workflow := testGUIWorkflow(coordinator, "sess-script", "turn-script")
	tool := &tools.AppleScriptTool{}
	args := `{"script":"tell application secret_app to keystroke secret_value","description":"unsafe broad script"}`
	done := make(chan agent.ToolResult, 1)
	go func() {
		result, _ := workflow.runTool(context.Background(), tool, args)
		done <- result
	}()
	acknowledgeController(t, coordinator)
	result := <-done
	if !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness || !strings.Contains(result.Content, "policy denied") {
		t.Fatalf("unscoped AppleScript result=%+v", result)
	}
	mu.Lock()
	payload, _ := json.Marshal(events)
	mu.Unlock()
	if strings.Contains(string(payload), "secret_app") || strings.Contains(string(payload), "secret_value") ||
		strings.Contains(string(payload), "unsafe broad script") {
		t.Fatalf("activity leaked AppleScript content: %s", payload)
	}
	workflow.EndTurn()
}

func TestDaemonGUIWorkflowStagesAndAwaitsSeparateConsequentialDecision(t *testing.T) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{
		RequireControllerHeartbeat: true, LeaseTTL: 5 * time.Second,
	})
	broker, err := NewConsequentialRiskBroker(ConsequentialRiskBrokerOptions{
		Random:     bytes.NewReader(sequentialConsequentialRiskRandom(2)),
		PendingTTL: 5 * time.Second, GrantTTL: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	workflow := testGUIWorkflow(coordinator, "sess-risk", "turn-risk")
	workflow.riskBroker = broker
	draft := consequentialRiskBrokerDraft(t, "toolu_exact_123")
	tool := &consequentialRiskProbeTool{draft: draft}
	done := make(chan agent.ToolResult, 1)
	go func() { result, _ := workflow.runTool(context.Background(), tool, `{}`); done <- result }()
	acknowledgeController(t, coordinator)
	var intentID string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		active := coordinator.Snapshot().Active
		if active != nil && active.ConsequentialRisk != nil {
			intentID = active.ConsequentialRisk.IntentID
			if active.ActionPhase != guicontrol.ComputerUsePhaseWaitingForUser ||
				active.ConsequentialRisk.Kind != "send" ||
				active.TargetBundleID != draft.Target.BundleID || active.TargetAppName != draft.Target.AppName {
				t.Fatalf("pending activity=%+v draftTarget=%+v", active, draft.Target)
			}
			break
		}
		time.Sleep(time.Millisecond)
	}
	if intentID == "" {
		t.Fatal("risk marker was not staged")
	}
	if _, err := broker.Decide(ConsequentialRiskDecisionRequestV1{
		SchemaVersion: 1, IntentID: intentID, Decision: ConsequentialRiskDecisionAllow,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.IsError || tool.calls != 1 {
			t.Fatalf("result=%+v calls=%d", result, tool.calls)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("allow did not unblock workflow")
	}
	workflow.EndTurn()
}

func TestDaemonGUIWorkflowUnresolvedLaunchHasExplicitBusinessError(t *testing.T) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{
		RequireControllerHeartbeat: true,
		LeaseTTL:                   5 * time.Second,
	})
	workflow := testGUIWorkflow(coordinator, "sess-launch", "turn-launch")
	tool := &guiProbeTool{
		name: "computer_use",
		descriptor: agent.GUIActionDescriptor{
			Participates: true, ActionKind: "launch_app", Effect: agent.GUIActionMutation,
			TargetAppName: "Notes",
		},
	}
	result, err := workflow.runTool(context.Background(), tool, `{}`)
	if err != nil || !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness ||
		!strings.Contains(result.Content, "cannot safely resolve the target bundle before launch") ||
		!strings.Contains(result.Content, "launch the app manually first") || tool.calls != 0 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, tool.calls)
	}
	if active := coordinator.Snapshot().Active; active != nil {
		t.Fatalf("unresolved launch acquired a lease: %+v", active)
	}
}

func TestDaemonGUIWorkflowUnavailableComputerUseActionsNeverReachBeginAction(t *testing.T) {
	registry, _, cleanup := tools.RegisterLocalTools(nil, nil)
	defer cleanup()
	tool, ok := registry.Get("computer_use")
	if !ok {
		t.Fatal("computer_use is not registered")
	}

	for _, action := range []string{"focus_app", "launch_app", "set_value"} {
		t.Run(action, func(t *testing.T) {
			coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{
				RequireControllerHeartbeat: true,
				LeaseTTL:                   5 * time.Second,
			})
			workflow := testGUIWorkflow(coordinator, "sess-unavailable-"+action, "turn-unavailable-"+action)
			ctx := agent.ContextWithToolInvocation(context.Background(), agent.ToolInvocation{
				ToolName: "computer_use", ToolUseID: "toolu-unavailable-" + action,
			})
			args := `{"action":"` + action + `","app":"Notes","value":"redacted","description":"mutate"}`
			result, err := workflow.runTool(ctx, tool, args)
			if err != nil || !result.IsError || result.ErrorCategory != agent.ErrCategoryValidation ||
				!strings.Contains(result.Content, "could not be safely classified") {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if active := coordinator.Snapshot().Active; active != nil {
				t.Fatalf("unavailable action reached BeginAction and acquired authority: %+v", active)
			}
		})
	}
}

func TestDaemonGUIWorkflowUntargetedGlobalInputFailsBeforeExecution(t *testing.T) {
	for _, action := range []string{"type", "hotkey", "scroll"} {
		t.Run(action, func(t *testing.T) {
			coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{
				RequireControllerHeartbeat: true,
				LeaseTTL:                   5 * time.Second,
			})
			workflow := testGUIWorkflow(coordinator, "sess-"+action, "turn-"+action)
			tool := &guiProbeTool{
				name: "computer_use",
				descriptor: agent.GUIActionDescriptor{
					Participates: true, ActionKind: action, Effect: agent.GUIActionMutation,
					TargetAppName: "Notes",
				},
			}
			result, err := workflow.runTool(context.Background(), tool, `{}`)
			if err != nil || !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness ||
				!strings.Contains(result.Content, "target-bound execution is unavailable") || tool.calls != 0 {
				t.Fatalf("result=%+v err=%v calls=%d", result, err, tool.calls)
			}
			if active := coordinator.Snapshot().Active; active != nil {
				t.Fatalf("untargeted %s acquired a lease: %+v", action, active)
			}
		})
	}
}

func TestDaemonGUIWorkflowAdmitsExactlyTargetedSemanticScroll(t *testing.T) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{
		RequireControllerHeartbeat: true,
		LeaseTTL:                   5 * time.Second,
	})
	workflow := testGUIWorkflow(coordinator, "sess-scroll", "turn-scroll")
	tool := &guiProbeTool{
		name: "computer_use",
		descriptor: agent.GUIActionDescriptor{
			Participates: true, ActionKind: "scroll", Effect: agent.GUIActionMutation,
			TargetBundleID: "com.apple.Notes", TargetAppName: "Notes",
			ExecutionPath: "accessibility",
		},
		outcome: &agent.GUIActionOutcome{
			Result: agent.GUIActionResultVerified, Phase: agent.GUIActionPhaseVerifying,
		},
	}
	result := runGUIWorkflowWithControllerAck(t, coordinator, workflow, tool, `{}`)
	if result.IsError || tool.calls != 1 || result.GUIOutcome == nil ||
		result.GUIOutcome.Result != agent.GUIActionResultVerified {
		t.Fatalf("targeted semantic scroll result=%+v calls=%d", result, tool.calls)
	}
	workflow.EndTurn()
}
