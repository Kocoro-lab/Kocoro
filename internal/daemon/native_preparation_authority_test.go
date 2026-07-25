package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/guicontrol"
)

type daemonNativePreparationProbe struct {
	descriptor agent.GUIActionDescriptor
	started    chan struct{}
	wait       bool
	prepareErr error

	mu           sync.Mutex
	prepareCalls int
	authorized   bool
	toolUseID    string
}

func (p *daemonNativePreparationProbe) Info() agent.ToolInfo {
	return agent.ToolInfo{Name: client.NativeComputerToolName}
}

func (p *daemonNativePreparationProbe) RequiresApproval() bool { return true }

func (p *daemonNativePreparationProbe) NativeToolDef() *client.NativeToolDef {
	return &client.NativeToolDef{
		Type: client.NativeComputerToolType, Name: client.NativeComputerToolName,
		DisplayWidthPx: 1024, DisplayHeightPx: 768,
	}
}

func (p *daemonNativePreparationProbe) DescribeGUIAction(
	context.Context,
	string,
) (agent.GUIActionDescriptor, error) {
	return p.descriptor, nil
}

func (p *daemonNativePreparationProbe) DescribeNativeToolRequestPreparation(
	context.Context,
) (agent.GUIActionDescriptor, error) {
	return p.descriptor, nil
}

func (p *daemonNativePreparationProbe) IsReadOnlyCall(string) bool { return true }

func (p *daemonNativePreparationProbe) PrepareNativeToolRequest(ctx context.Context) error {
	invocation, _ := agent.ToolInvocationFromContext(ctx)
	path := p.descriptor.ExecutionPath
	scope := guicontrol.ExecutionScope{
		ToolName: p.Info().Name, ToolUseID: invocation.ToolUseID,
		ActionKind:     p.descriptor.ActionKind,
		Effect:         string(guicontrol.ComputerUseActionObservation),
		TargetBundleID: p.descriptor.TargetBundleID,
		ExecutionPath:  path,
	}
	p.mu.Lock()
	p.prepareCalls++
	p.toolUseID = invocation.ToolUseID
	p.authorized = guicontrol.ExecutionAuthorized(ctx, scope)
	p.mu.Unlock()
	if p.started != nil {
		close(p.started)
	}
	if p.wait {
		<-ctx.Done()
		return ctx.Err()
	}
	return p.prepareErr
}

func (p *daemonNativePreparationProbe) Run(
	context.Context,
	string,
) (agent.ToolResult, error) {
	return agent.ToolResult{Content: "unused"}, nil
}

func (p *daemonNativePreparationProbe) snapshot() (calls int, authorized bool, toolUseID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.prepareCalls, p.authorized, p.toolUseID
}

func wrappedDaemonNativePreparer(
	t *testing.T,
	workflow *daemonGUIWorkflow,
	probe *daemonNativePreparationProbe,
) agent.NativeToolRequestPreparer {
	t.Helper()
	registry := agent.NewToolRegistry()
	registry.Register(probe)
	wrapDaemonGUITools(registry, workflow)
	wrapped, ok := registry.Get(client.NativeComputerToolName)
	if !ok {
		t.Fatal("wrapped native computer tool is missing")
	}
	preparer, ok := wrapped.(agent.NativeToolRequestPreparer)
	if !ok {
		t.Fatalf("wrapped native computer lost request preparer: %T", wrapped)
	}
	return preparer
}

func nativePreparationDescriptor() agent.GUIActionDescriptor {
	return agent.GUIActionDescriptor{
		Participates: true, ActionKind: "get_app_state",
		Effect:         agent.GUIActionObservation,
		TargetBundleID: "com.example.Editor", TargetAppName: "Editor",
		ExecutionPath: "accessibility",
	}
}

func TestDaemonNativePreparationStopCancelsAndAcknowledgesQuiescence(t *testing.T) {
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
	workflow := testGUIWorkflow(coordinator, "sess-native-stop", "turn-native-stop")
	probe := &daemonNativePreparationProbe{
		descriptor: nativePreparationDescriptor(),
		started:    make(chan struct{}),
		wait:       true,
	}
	preparer := wrappedDaemonNativePreparer(t, workflow, probe)

	done := make(chan error, 1)
	go func() { done <- preparer.PrepareNativeToolRequest(context.Background()) }()
	lease := acknowledgeController(t, coordinator)
	select {
	case <-probe.started:
	case <-time.After(2 * time.Second):
		t.Fatal("native preparation did not start")
	}

	response, err := coordinator.Control(guicontrol.ComputerUseControlRequest{
		LeaseID: lease.LeaseID, Action: guicontrol.ComputerUseControlStop,
		IdempotencyKey: "stop-native-preparation",
	})
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if response.Quiesced {
		t.Fatal("Stop claimed quiescence before native preparer acknowledged cancellation")
	}
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("native preparation cancellation error = %T %v", err, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("native preparation did not return after Stop")
	}
	if active := coordinator.Snapshot().Active; active != nil {
		t.Fatalf("native preparation did not acknowledge quiescence: %+v", active)
	}
	calls, authorized, toolUseID := probe.snapshot()
	if calls != 1 || !authorized || !strings.HasPrefix(toolUseID, "native_prepare/turn-native-stop/") {
		t.Fatalf("preparation calls=%d authorized=%t tool_use_id=%q", calls, authorized, toolUseID)
	}

	eventsMu.Lock()
	defer eventsMu.Unlock()
	var begins, finishes int
	for _, event := range events {
		if event.ActionID == "" || event.ActionKind != "get_app_state" {
			continue
		}
		if event.ActionResult == nil {
			begins++
		} else if event.LeaseState == guicontrol.ComputerUseLeaseTerminal {
			finishes++
		}
	}
	if begins != 1 || finishes != 1 {
		t.Fatalf("native preparation lifecycle begins=%d finishes=%d events=%+v", begins, finishes, events)
	}
}

func TestDaemonNativePreparationBlockedAppFailsBeforeLeaseAndCapture(t *testing.T) {
	store := NewComputerUseAppPolicyStore(t.TempDir())
	if _, err := store.Update("com.example.editor", ComputerUseAppPolicyBlocked); err != nil {
		t.Fatal(err)
	}
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{
		RequireControllerHeartbeat: true,
	})
	workflow := testGUIWorkflow(coordinator, "sess-native-blocked", "turn-native-blocked")
	workflow.appPolicy = store
	probe := &daemonNativePreparationProbe{descriptor: nativePreparationDescriptor()}

	err := wrappedDaemonNativePreparer(t, workflow, probe).
		PrepareNativeToolRequest(context.Background())
	if err == nil || !strings.Contains(err.Error(), "app policy blocked") {
		t.Fatalf("blocked native preparation error = %v", err)
	}
	if calls, _, _ := probe.snapshot(); calls != 0 {
		t.Fatalf("blocked native preparation reached capture %d times", calls)
	}
	if active := coordinator.Snapshot().Active; active != nil {
		t.Fatalf("blocked native preparation acquired lease: %+v", active)
	}
}

func TestDaemonNativePreparationRequiresRealControllerHeartbeat(t *testing.T) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{
		RequireControllerHeartbeat: true,
		LeaseTTL:                   5 * time.Second,
	})
	workflow := testGUIWorkflow(coordinator, "sess-native-no-controller", "turn-native-no-controller")
	probe := &daemonNativePreparationProbe{descriptor: nativePreparationDescriptor()}
	preparer := wrappedDaemonNativePreparer(t, workflow, probe)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- preparer.PrepareNativeToolRequest(ctx) }()
	waitForActiveLease(t, coordinator)
	cancel()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("missing-controller error = %T %v", err, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("native preparation did not leave controller wait")
	}
	if calls, _, _ := probe.snapshot(); calls != 0 {
		t.Fatalf("native capture started without controller heartbeat: %d", calls)
	}
	if active := coordinator.Snapshot().Active; active != nil {
		t.Fatalf("cancelled controller wait stranded native lease: %+v", active)
	}
}

func TestDaemonNativePreparationHeartbeatExpiryCancelsActiveCapture(t *testing.T) {
	var clockMu sync.Mutex
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{
		InstanceID:                 "cui_native_expiry",
		RequireControllerHeartbeat: true,
		LeaseTTL:                   30 * time.Second,
		Now: func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return now
		},
		NewID: func(prefix string) string { return fmt.Sprintf("%s_native_expiry", prefix) },
	})
	workflow := testGUIWorkflow(coordinator, "sess-native-expiry", "turn-native-expiry")
	probe := &daemonNativePreparationProbe{
		descriptor: nativePreparationDescriptor(),
		started:    make(chan struct{}),
		wait:       true,
	}
	preparer := wrappedDaemonNativePreparer(t, workflow, probe)

	done := make(chan error, 1)
	go func() { done <- preparer.PrepareNativeToolRequest(context.Background()) }()
	acknowledgeController(t, coordinator)
	select {
	case <-probe.started:
	case <-time.After(2 * time.Second):
		t.Fatal("native preparation did not start")
	}
	clockMu.Lock()
	now = now.Add(31 * time.Second)
	clockMu.Unlock()
	if !coordinator.ExpireStale() {
		t.Fatal("native preparation lease did not expire")
	}
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("expired native preparation error = %T %v", err, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expired native preparation did not quiesce")
	}
	if active := coordinator.Snapshot().Active; active != nil {
		t.Fatalf("expired native preparation retained action: %+v", active)
	}
}

func TestDaemonNativePreparationErrorFinishesExactlyOnce(t *testing.T) {
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
	workflow := testGUIWorkflow(coordinator, "sess-native-error", "turn-native-error")
	probe := &daemonNativePreparationProbe{
		descriptor: nativePreparationDescriptor(),
		prepareErr: errors.New("strict screenshot unavailable"),
	}
	preparer := wrappedDaemonNativePreparer(t, workflow, probe)

	done := make(chan error, 1)
	go func() { done <- preparer.PrepareNativeToolRequest(context.Background()) }()
	acknowledgeController(t, coordinator)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "strict screenshot unavailable") {
		t.Fatalf("native preparation error = %v", err)
	}
	active := coordinator.Snapshot().Active
	if active == nil || active.ActionResult == nil ||
		*active.ActionResult != guicontrol.ComputerUseResultFailed ||
		active.ActionPhase != guicontrol.ComputerUsePhaseIdle {
		t.Fatalf("failed native preparation activity = %+v", active)
	}
	if calls, authorized, _ := probe.snapshot(); calls != 1 || !authorized {
		t.Fatalf("failed preparation calls=%d authorized=%t", calls, authorized)
	}

	eventsMu.Lock()
	defer eventsMu.Unlock()
	var begins, finishes int
	for _, event := range events {
		if event.ActionID == "" {
			continue
		}
		if event.ActionResult == nil {
			begins++
		} else {
			finishes++
		}
	}
	if begins != 1 || finishes != 1 {
		t.Fatalf("failed preparation lifecycle begins=%d finishes=%d events=%+v", begins, finishes, events)
	}
}

func TestDaemonNativePreparationRejectsFocusOrLaunchBeforeApprovalBypass(t *testing.T) {
	for _, action := range []string{"focus_app", "launch_app"} {
		t.Run(action, func(t *testing.T) {
			coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{
				RequireControllerHeartbeat: true,
			})
			workflow := testGUIWorkflow(
				coordinator,
				"sess-native-"+action,
				"turn-native-"+action,
			)
			descriptor := nativePreparationDescriptor()
			descriptor.ActionKind = action
			descriptor.Effect = agent.GUIActionMutation
			probe := &daemonNativePreparationProbe{descriptor: descriptor}

			err := wrappedDaemonNativePreparer(t, workflow, probe).
				PrepareNativeToolRequest(context.Background())
			if err == nil || !strings.Contains(err.Error(), "observation-only") {
				t.Fatalf("%s native preparation error = %v", action, err)
			}
			if calls, _, _ := probe.snapshot(); calls != 0 {
				t.Fatalf("%s preparation bypassed approval policy: calls=%d", action, calls)
			}
			if active := coordinator.Snapshot().Active; active != nil {
				t.Fatalf("%s preparation acquired lease: %+v", action, active)
			}
		})
	}
}
