package guicontrol

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type coordinatorFixture struct {
	mu  sync.Mutex
	now time.Time
	seq int
}

func newCoordinatorFixture(t *testing.T, sink ActivitySink) (*Coordinator, *coordinatorFixture) {
	t.Helper()
	fixture := &coordinatorFixture{now: time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)}
	coordinator := NewCoordinator(CoordinatorOptions{
		InstanceID: "cui_test",
		Now: func() time.Time {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			return fixture.now
		},
		NewID: func(prefix string) string {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			fixture.seq++
			return fmt.Sprintf("%s_test_%d", prefix, fixture.seq)
		},
		LeaseTTL: 30 * time.Second,
		Sink:     sink,
	})
	return coordinator, fixture
}

func (f *coordinatorFixture) advance(duration time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(duration)
	f.mu.Unlock()
}

func workflowRequest(turnID string) WorkflowRequest {
	return WorkflowRequest{
		SessionID:            "session-" + turnID,
		TurnID:               turnID,
		SourceKind:           "desktop",
		SourceLabel:          "Kocoro Desktop",
		RequestedAppBundleID: "com.apple.Notes",
		AllowedAppBundleIDs:  []string{"com.apple.Notes"},
		PolicySnapshotID:     "policy-1",
	}
}

func finishVerifiedObservation(t *testing.T, coordinator *Coordinator, lease WorkflowLease, toolUseID string) {
	t.Helper()
	action, err := coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: toolUseID,
		ToolName: "computer_use", ActionKind: "get_app_state", Effect: ComputerUseActionObservation,
	})
	if err != nil {
		t.Fatalf("BeginAction verified observation: %v", err)
	}
	verified := ComputerUseResultVerified
	if err := coordinator.FinishAction(ActionFinish{
		LeaseID: lease.LeaseID, ActionID: action.ActionID,
		Phase: ComputerUsePhaseObserving, Result: &verified,
	}); err != nil {
		t.Fatalf("FinishAction verified observation: %v", err)
	}
}

func TestBeginWorkflowReusesSameTurnAndRejectsAnotherTurnAsBusy(t *testing.T) {
	coordinator, _ := newCoordinatorFixture(t, nil)

	first, err := coordinator.BeginWorkflow(workflowRequest("turn-1"))
	if err != nil {
		t.Fatalf("BeginWorkflow first: %v", err)
	}
	reused, err := coordinator.BeginWorkflow(workflowRequest("turn-1"))
	if err != nil {
		t.Fatalf("BeginWorkflow reuse: %v", err)
	}
	if reused.LeaseID != first.LeaseID || reused.CreatedAt != first.CreatedAt {
		t.Fatalf("same turn received a new lease: first=%+v reused=%+v", first, reused)
	}

	_, err = coordinator.BeginWorkflow(workflowRequest("turn-2"))
	var busy *BusyError
	if !errors.As(err, &busy) {
		t.Fatalf("other turn error = %T %v; want *BusyError", err, err)
	}
	if busy.ActiveLeaseID != first.LeaseID || busy.SourceLabel != "Kocoro Desktop" || busy.TargetBundleID != "com.apple.Notes" {
		t.Fatalf("busy metadata = %+v", busy)
	}
	if busy.ActiveSessionID != "" {
		t.Fatalf("busy error leaked session id: %+v", busy)
	}
	if got := coordinator.Snapshot(); got.Revision != 1 || got.Active == nil || got.Active.LeaseID != first.LeaseID {
		t.Fatalf("snapshot after begin = %+v", got)
	}
}

func TestStopRevokesAdmissionCancelsActionAndIsIdempotent(t *testing.T) {
	var eventsMu sync.Mutex
	var events []ComputerUseActivityEvent
	coordinator, _ := newCoordinatorFixture(t, func(event ComputerUseActivityEvent) {
		eventsMu.Lock()
		events = append(events, event)
		eventsMu.Unlock()
	})
	lease, err := coordinator.BeginWorkflow(workflowRequest("turn-stop"))
	if err != nil {
		t.Fatal(err)
	}
	action, err := coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID:        lease.LeaseID,
		TurnID:         lease.TurnID,
		ToolUseID:      "toolu-stop",
		ActionKind:     "click",
		ActionPhase:    ComputerUsePhaseActing,
		TargetBundleID: "com.apple.Notes",
		Effect:         ComputerUseActionMutation,
	})
	if err != nil {
		t.Fatal(err)
	}

	request := ComputerUseControlRequest{
		LeaseID:        lease.LeaseID,
		Action:         ComputerUseControlStop,
		IdempotencyKey: "stop-1",
	}
	first, err := coordinator.Control(request)
	if err != nil {
		t.Fatalf("Control stop: %v", err)
	}
	if !first.Accepted || first.LeaseState != ComputerUseLeaseStopping {
		t.Fatalf("stop response = %+v", first)
	}
	select {
	case <-action.Context.Done():
	case <-time.After(time.Second):
		t.Fatal("stop did not synchronously cancel active action")
	}

	repeated, err := coordinator.Control(request)
	if err != nil {
		t.Fatalf("repeated Control stop: %v", err)
	}
	if repeated != first {
		t.Fatalf("repeated response = %+v; want %+v", repeated, first)
	}
	if snapshot := coordinator.Snapshot(); snapshot.Revision != first.Revision {
		t.Fatalf("idempotent retry advanced revision: snapshot=%+v response=%+v", snapshot, first)
	}

	_, err = coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID:    lease.LeaseID,
		TurnID:     lease.TurnID,
		ToolUseID:  "toolu-after-stop",
		ActionKind: "click", Effect: ComputerUseActionMutation, TargetBundleID: "com.apple.Notes",
	})
	var stopped *StoppedTurnError
	if !errors.As(err, &stopped) {
		t.Fatalf("action after stop error = %T %v; want *StoppedTurnError", err, err)
	}

	eventsMu.Lock()
	defer eventsMu.Unlock()
	if len(events) != 3 {
		t.Fatalf("event count = %d; want begin, action, stop only", len(events))
	}
}

func TestControlIdempotencyKeyConflictFailsClosed(t *testing.T) {
	coordinator, _ := newCoordinatorFixture(t, nil)
	lease, err := coordinator.BeginWorkflow(workflowRequest("turn-idem"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = coordinator.Control(ComputerUseControlRequest{
		LeaseID: lease.LeaseID, Action: ComputerUseControlPause, IdempotencyKey: "same-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = coordinator.Control(ComputerUseControlRequest{
		LeaseID: lease.LeaseID, Action: ComputerUseControlResume, IdempotencyKey: "same-key",
	})
	var conflict *IdempotencyConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("conflicting reuse error = %T %v; want *IdempotencyConflictError", err, err)
	}
}

func TestPauseResumeTakeOverTransitionsAndCancellation(t *testing.T) {
	coordinator, _ := newCoordinatorFixture(t, nil)
	lease, err := coordinator.BeginWorkflow(workflowRequest("turn-control"))
	if err != nil {
		t.Fatal(err)
	}
	action, err := coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-1", ActionKind: "drag",
		Effect: ComputerUseActionMutation, TargetBundleID: "com.apple.Notes",
	})
	if err != nil {
		t.Fatal(err)
	}

	paused, err := coordinator.Control(ComputerUseControlRequest{
		LeaseID: lease.LeaseID, Action: ComputerUseControlPause, IdempotencyKey: "pause-1",
	})
	if err != nil || paused.LeaseState != ComputerUseLeasePaused {
		t.Fatalf("pause = %+v, %v", paused, err)
	}
	select {
	case <-action.Context.Done():
	case <-time.After(time.Second):
		t.Fatal("pause did not cancel current action")
	}
	_, err = coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-paused", ActionKind: "click",
		Effect: ComputerUseActionMutation, TargetBundleID: "com.apple.Notes",
	})
	var invalid *InvalidTransitionError
	if !errors.As(err, &invalid) {
		t.Fatalf("paused BeginAction error = %T %v; want *InvalidTransitionError", err, err)
	}
	cancelled := ComputerUseResultCancelled
	if err := coordinator.FinishAction(ActionFinish{
		LeaseID: lease.LeaseID, ActionID: action.ActionID, Result: &cancelled,
	}); err != nil {
		t.Fatalf("FinishAction after pause: %v", err)
	}

	resumed, err := coordinator.Control(ComputerUseControlRequest{
		LeaseID: lease.LeaseID, Action: ComputerUseControlResume, IdempotencyKey: "resume-1",
	})
	if err != nil || resumed.LeaseState != ComputerUseLeaseActive {
		t.Fatalf("resume = %+v, %v", resumed, err)
	}
	finishVerifiedObservation(t, coordinator, lease, "toolu-observe-after-resume")
	second, err := coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-2", ActionKind: "click",
		Effect: ComputerUseActionMutation, TargetBundleID: "com.apple.Notes",
	})
	if err != nil {
		t.Fatalf("BeginAction after resume: %v", err)
	}
	taken, err := coordinator.Control(ComputerUseControlRequest{
		LeaseID: lease.LeaseID, Action: ComputerUseControlTakeOver, IdempotencyKey: "take-over-1",
	})
	if err != nil || taken.LeaseState != ComputerUseLeasePaused {
		t.Fatalf("take over = %+v, %v", taken, err)
	}
	select {
	case <-second.Context.Done():
	case <-time.After(time.Second):
		t.Fatal("take over did not cancel current action")
	}
}

func TestPhysicalUserInterferenceKeepsLeaseActiveButRequiresVerifiedObservation(t *testing.T) {
	coordinator, _ := newCoordinatorFixture(t, nil)
	lease, err := coordinator.BeginWorkflow(workflowRequest("turn-user-interference"))
	if err != nil {
		t.Fatal(err)
	}
	action, err := coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-interfered", ActionKind: "drag",
		Effect: ComputerUseActionMutation, TargetBundleID: "com.apple.Notes",
	})
	if err != nil {
		t.Fatal(err)
	}
	interference := ComputerUseResultUserInterference
	code := "physical_input_interference"
	if err := coordinator.FinishAction(ActionFinish{
		LeaseID: lease.LeaseID, ActionID: action.ActionID,
		Phase: ComputerUsePhaseWaitingForUser, Result: &interference, FailureCode: &code,
	}); err != nil {
		t.Fatalf("FinishAction user interference: %v", err)
	}
	snapshot := coordinator.Snapshot()
	if snapshot.Active == nil || snapshot.Active.LeaseState != ComputerUseLeaseActive ||
		snapshot.Active.ActionPhase != ComputerUsePhaseObserving ||
		snapshot.Active.ActionResult == nil || *snapshot.Active.ActionResult != ComputerUseResultUserInterference {
		t.Fatalf("physical interference did not enter active re-observation: %+v", snapshot)
	}
	beginMutation := func(toolUseID string) error {
		_, err := coordinator.BeginAction(context.Background(), ActionRequest{
			LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: toolUseID, ActionKind: "click",
			Effect: ComputerUseActionMutation, TargetBundleID: "com.apple.Notes",
		})
		return err
	}
	if err := beginMutation("toolu-stale-mutation"); err == nil {
		t.Fatal("mutation was admitted before re-observation")
	} else {
		var required *ReobservationRequiredError
		if !errors.As(err, &required) {
			t.Fatalf("mutation before re-observation error = %T %v; want *ReobservationRequiredError", err, err)
		}
	}

	verifiedWait, err := coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-wait-verified",
		ToolName: "computer_use", ActionKind: "wait", Effect: ComputerUseActionObservation,
	})
	if err != nil {
		t.Fatalf("verified wait admission: %v", err)
	}
	verified := ComputerUseResultVerified
	if err := coordinator.FinishAction(ActionFinish{
		LeaseID: lease.LeaseID, ActionID: verifiedWait.ActionID,
		Phase: ComputerUsePhaseObserving, Result: &verified,
	}); err != nil {
		t.Fatalf("finish verified wait: %v", err)
	}
	if err := beginMutation("toolu-after-verified-wait"); err == nil {
		t.Fatal("verified wait incorrectly cleared the re-observation barrier")
	}

	failedObservation, err := coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-observe-failed", ActionKind: "observe",
		ToolName: "computer_use", Effect: ComputerUseActionObservation,
	})
	if err != nil {
		t.Fatalf("failed observation admission: %v", err)
	}
	failed := ComputerUseResultFailed
	if err := coordinator.FinishAction(ActionFinish{
		LeaseID: lease.LeaseID, ActionID: failedObservation.ActionID,
		Phase: ComputerUsePhaseObserving, Result: &failed,
	}); err != nil {
		t.Fatalf("finish failed observation: %v", err)
	}
	if err := beginMutation("toolu-after-failed-observe"); err == nil {
		t.Fatal("failed observation cleared the re-observation barrier")
	}

	verifiedObservation, err := coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-observe-verified",
		ToolName: "computer_use", ActionKind: "get_app_state", Effect: ComputerUseActionObservation,
	})
	if err != nil {
		t.Fatalf("verified observation admission: %v", err)
	}
	if err := coordinator.FinishAction(ActionFinish{
		LeaseID: lease.LeaseID, ActionID: verifiedObservation.ActionID,
		Phase: ComputerUsePhaseObserving, Result: &verified,
	}); err != nil {
		t.Fatalf("finish verified observation: %v", err)
	}
	if err := beginMutation("toolu-after-verified-observe"); err != nil {
		t.Fatalf("verified observation did not clear mutation barrier: %v", err)
	}
}

func TestCommittedOrUncertainMutationOutcomeRequiresVerifiedObservationEvenAfterPauseResume(t *testing.T) {
	for _, test := range []struct {
		name        string
		result      ComputerUseActionResult
		failureCode string
	}{
		{name: "verified", result: ComputerUseResultVerified},
		{name: "completed_unverified", result: ComputerUseResultCompletedUnverified},
		{name: "commit_unknown_cancellation", result: ComputerUseResultCancelled, failureCode: "commit_status_unknown_after_cancel"},
	} {
		t.Run(test.name, func(t *testing.T) {
			coordinator, _ := newCoordinatorFixture(t, nil)
			lease, err := coordinator.BeginWorkflow(workflowRequest("turn-uncertain-" + test.name))
			if err != nil {
				t.Fatal(err)
			}
			action, err := coordinator.BeginAction(context.Background(), ActionRequest{
				LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolName: "computer_use",
				ToolUseID: "toolu-uncertain", ActionKind: "click",
				Effect: ComputerUseActionMutation, TargetBundleID: "com.apple.Notes",
			})
			if err != nil {
				t.Fatal(err)
			}
			var failureCode *string
			if test.failureCode != "" {
				failureCode = &test.failureCode
			}
			if err := coordinator.FinishAction(ActionFinish{
				LeaseID: lease.LeaseID, ActionID: action.ActionID,
				Result: &test.result, FailureCode: failureCode,
			}); err != nil {
				t.Fatalf("FinishAction uncertain mutation: %v", err)
			}

			beginMutation := func(toolUseID string) error {
				_, err := coordinator.BeginAction(context.Background(), ActionRequest{
					LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolName: "computer_use",
					ToolUseID: toolUseID, ActionKind: "click",
					Effect: ComputerUseActionMutation, TargetBundleID: "com.apple.Notes",
				})
				return err
			}
			assertRequiresObservation := func(label string) {
				t.Helper()
				err := beginMutation("toolu-" + label)
				var required *ReobservationRequiredError
				if !errors.As(err, &required) {
					t.Fatalf("%s mutation error = %T %v; want *ReobservationRequiredError", label, err, err)
				}
			}
			assertRequiresObservation("immediate")

			if _, err := coordinator.Control(ComputerUseControlRequest{
				LeaseID: lease.LeaseID, Action: ComputerUseControlPause,
				IdempotencyKey: "pause-uncertain-" + test.name,
			}); err != nil {
				t.Fatalf("pause: %v", err)
			}
			if _, err := coordinator.Control(ComputerUseControlRequest{
				LeaseID: lease.LeaseID, Action: ComputerUseControlResume,
				IdempotencyKey: "resume-uncertain-" + test.name,
			}); err != nil {
				t.Fatalf("resume: %v", err)
			}
			assertRequiresObservation("after-resume")

			finishVerifiedObservation(t, coordinator, lease, "toolu-verified-reobserve")
			if err := beginMutation("toolu-after-reobserve"); err != nil {
				t.Fatalf("verified get_app_state did not clear barrier: %v", err)
			}
		})
	}
}

func TestFinishActionAndEndTurnPublishTerminalThenClearSnapshot(t *testing.T) {
	coordinator, _ := newCoordinatorFixture(t, nil)
	lease, err := coordinator.BeginWorkflow(workflowRequest("turn-finish"))
	if err != nil {
		t.Fatal(err)
	}
	action, err := coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-finish", ActionKind: "press",
		Effect: ComputerUseActionMutation, TargetBundleID: "com.apple.Notes",
	})
	if err != nil {
		t.Fatal(err)
	}
	verified := ComputerUseResultVerified
	if err := coordinator.FinishAction(ActionFinish{
		LeaseID: lease.LeaseID, ActionID: action.ActionID, Result: &verified,
	}); err != nil {
		t.Fatalf("FinishAction: %v", err)
	}
	if snapshot := coordinator.Snapshot(); snapshot.Active == nil || snapshot.Active.ActionResult == nil || *snapshot.Active.ActionResult != verified || snapshot.Active.ActionPhase != ComputerUsePhaseIdle {
		t.Fatalf("snapshot after finish = %+v", snapshot)
	}

	if err := coordinator.EndTurn(lease.TurnID, ComputerUseResultVerified); err != nil {
		t.Fatalf("EndTurn: %v", err)
	}
	if snapshot := coordinator.Snapshot(); snapshot.Active != nil {
		t.Fatalf("snapshot after EndTurn retained active lease: %+v", snapshot)
	}
	if _, err := coordinator.BeginWorkflow(workflowRequest("turn-next")); err != nil {
		t.Fatalf("next turn could not acquire released lease: %v", err)
	}
}

func TestHeartbeatExtendsLeaseAndExpiryCancelsAction(t *testing.T) {
	coordinator, fixture := newCoordinatorFixture(t, nil)
	lease, err := coordinator.BeginWorkflow(workflowRequest("turn-heartbeat"))
	if err != nil {
		t.Fatal(err)
	}
	action, err := coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-heartbeat", ActionKind: "wait_for_condition",
		Effect: ComputerUseActionObservation,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.advance(20 * time.Second)
	refreshed, err := coordinator.Heartbeat(lease.LeaseID)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	refreshedExpiresAt, err := time.Parse(time.RFC3339Nano, refreshed.ExpiresAt)
	if err != nil {
		t.Fatalf("heartbeat expires_at: %v", err)
	}
	if !refreshedExpiresAt.After(lease.ExpiresAt) {
		t.Fatalf("heartbeat did not extend expiry: before=%s after=%s", lease.ExpiresAt, refreshed.ExpiresAt)
	}
	fixture.advance(31 * time.Second)
	if expired := coordinator.ExpireStale(); !expired {
		t.Fatal("ExpireStale did not expire overdue lease")
	}
	select {
	case <-action.Context.Done():
	case <-time.After(time.Second):
		t.Fatal("expiry did not cancel current action")
	}
	if snapshot := coordinator.Snapshot(); snapshot.Active == nil || snapshot.Active.LeaseState != ComputerUseLeaseStopping {
		t.Fatalf("expired lease did not retain action quiescence barrier: %+v", snapshot)
	}
	_, err = coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-late", ActionKind: "click",
		Effect: ComputerUseActionMutation, TargetBundleID: "com.apple.Notes",
	})
	var expired *LeaseExpiredError
	if !errors.As(err, &expired) {
		t.Fatalf("late action error = %T %v; want *LeaseExpiredError", err, err)
	}
	if err := coordinator.FinishAction(ActionFinish{
		LeaseID: lease.LeaseID, ActionID: action.ActionID,
	}); err != nil {
		t.Fatalf("expired action quiescence acknowledgement: %v", err)
	}
	if snapshot := coordinator.Snapshot(); snapshot.Active != nil {
		t.Fatalf("expired lease remained active after quiescence: %+v", snapshot)
	}
}

func TestRequiredControllerHeartbeatGatesGUIActions(t *testing.T) {
	fixture := &coordinatorFixture{now: time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)}
	coordinator := NewCoordinator(CoordinatorOptions{
		InstanceID:                 "cui_required_heartbeat",
		RequireControllerHeartbeat: true,
		Now: func() time.Time {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			return fixture.now
		},
		NewID: func(prefix string) string {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			fixture.seq++
			return fmt.Sprintf("%s_required_%d", prefix, fixture.seq)
		},
		LeaseTTL: 30 * time.Second,
	})
	lease, err := coordinator.BeginWorkflow(workflowRequest("turn-controller-gate"))
	if err != nil {
		t.Fatal(err)
	}
	if !lease.HeartbeatAt.IsZero() {
		t.Fatalf("BeginWorkflow masqueraded as controller heartbeat: %s", lease.HeartbeatAt)
	}
	_, err = coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-before-ack", ActionKind: "click",
		Effect: ComputerUseActionMutation, TargetBundleID: "com.apple.Notes",
	})
	var unavailable *ControllerUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("action before controller ack error = %T %v; want *ControllerUnavailableError", err, err)
	}

	heartbeat, err := coordinator.Heartbeat(lease.LeaseID)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if heartbeat.CoordinatorInstanceID != "cui_required_heartbeat" || heartbeat.Revision != 1 || heartbeat.LeaseState != ComputerUseLeaseActive {
		t.Fatalf("heartbeat response = %+v", heartbeat)
	}
	if _, err := coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-after-ack", ActionKind: "click",
		Effect: ComputerUseActionMutation, TargetBundleID: "com.apple.Notes",
	}); err != nil {
		t.Fatalf("action after controller ack: %v", err)
	}
}

func TestProcessCoordinatorRequiresControllerHeartbeat(t *testing.T) {
	if !ProcessCoordinator().requireControllerHeartbeat {
		t.Fatal("process coordinator permits GUI actions without a controller heartbeat")
	}
}

func TestSinkRunsOutsideCoordinatorMutex(t *testing.T) {
	var coordinator *Coordinator
	done := make(chan struct{}, 1)
	coordinator, _ = newCoordinatorFixture(t, func(ComputerUseActivityEvent) {
		_ = coordinator.Snapshot()
		done <- struct{}{}
	})
	if _, err := coordinator.BeginWorkflow(workflowRequest("turn-reentrant")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sink deadlocked while re-entering Snapshot")
	}
}

func TestConcurrentWorkflowAdmissionHasExactlyOneOwner(t *testing.T) {
	coordinator, _ := newCoordinatorFixture(t, nil)
	const contenders = 32
	start := make(chan struct{})
	var winners atomic.Int32
	var busyCount atomic.Int32
	var unexpected atomic.Value
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := coordinator.BeginWorkflow(workflowRequest(fmt.Sprintf("turn-%d", i)))
			if err == nil {
				winners.Add(1)
				return
			}
			var busy *BusyError
			if errors.As(err, &busy) {
				busyCount.Add(1)
				return
			}
			unexpected.Store(err)
		}(i)
	}
	close(start)
	wg.Wait()
	if err, _ := unexpected.Load().(error); err != nil {
		t.Fatalf("unexpected admission error: %v", err)
	}
	if winners.Load() != 1 || busyCount.Load() != contenders-1 {
		t.Fatalf("winners=%d busy=%d; want 1/%d", winners.Load(), busyCount.Load(), contenders-1)
	}
}

func TestTypedErrorsForStaleLeaseAndInvalidTransitions(t *testing.T) {
	coordinator, _ := newCoordinatorFixture(t, nil)
	lease, err := coordinator.BeginWorkflow(workflowRequest("turn-errors"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = coordinator.Control(ComputerUseControlRequest{
		LeaseID: "stale", Action: ComputerUseControlStop, IdempotencyKey: "stale-stop",
	})
	var stale *StaleLeaseError
	if !errors.As(err, &stale) {
		t.Fatalf("stale error = %T %v; want *StaleLeaseError", err, err)
	}
	_, err = coordinator.Control(ComputerUseControlRequest{
		LeaseID: lease.LeaseID, Action: ComputerUseControlResume, IdempotencyKey: "resume-active",
	})
	var invalid *InvalidTransitionError
	if !errors.As(err, &invalid) {
		t.Fatalf("invalid transition error = %T %v; want *InvalidTransitionError", err, err)
	}
}

func TestExpiredLeaseIsNeverReusedOrControlled(t *testing.T) {
	coordinator, fixture := newCoordinatorFixture(t, nil)
	lease, err := coordinator.BeginWorkflow(workflowRequest("turn-expired"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.advance(31 * time.Second)

	_, err = coordinator.Control(ComputerUseControlRequest{
		LeaseID: lease.LeaseID, Action: ComputerUseControlPause, IdempotencyKey: "pause-expired",
	})
	var expired *LeaseExpiredError
	if !errors.As(err, &expired) {
		t.Fatalf("expired control error = %T %v; want *LeaseExpiredError", err, err)
	}
	if snapshot := coordinator.Snapshot(); snapshot.Active != nil {
		t.Fatalf("expired control retained active lease: %+v", snapshot)
	}

	next, err := coordinator.BeginWorkflow(workflowRequest("turn-after-expiry"))
	if err != nil {
		t.Fatalf("new turn could not acquire after lazy expiry: %v", err)
	}
	if next.LeaseID == lease.LeaseID {
		t.Fatal("new turn reused expired lease")
	}
}

func TestExpiryKeepsActionQuiescenceBarrierUntilExecutorAcknowledges(t *testing.T) {
	var eventsMu sync.Mutex
	var events []ComputerUseActivityEvent
	coordinator, fixture := newCoordinatorFixture(t, func(event ComputerUseActivityEvent) {
		eventsMu.Lock()
		events = append(events, event)
		eventsMu.Unlock()
	})
	lease, err := coordinator.BeginWorkflow(workflowRequest("turn-expired-action"))
	if err != nil {
		t.Fatal(err)
	}
	action, err := coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID,
		ToolUseID: "toolu-expired-action", ActionKind: "drag",
		Effect: ComputerUseActionMutation, TargetBundleID: "com.apple.Notes",
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.advance(31 * time.Second)
	if !coordinator.ExpireStale() {
		t.Fatal("expired action lease was not swept")
	}
	select {
	case <-action.Context.Done():
	case <-time.After(time.Second):
		t.Fatal("expiry did not cancel active action")
	}

	snapshot := coordinator.Snapshot()
	if snapshot.Active == nil || snapshot.Active.LeaseState != ComputerUseLeaseStopping ||
		snapshot.Active.FailureCode == nil || *snapshot.Active.FailureCode != "lease_heartbeat_expired" {
		t.Fatalf("expiry removed quiescence barrier or lost cause: %+v", snapshot)
	}
	if _, err := coordinator.BeginWorkflow(workflowRequest("turn-after-expired-action")); err == nil {
		t.Fatal("new workflow entered while expired executor had not acknowledged cancellation")
	} else {
		var busy *BusyError
		if !errors.As(err, &busy) {
			t.Fatalf("new workflow during expiry barrier = %T %v; want *BusyError", err, err)
		}
	}

	verified := ComputerUseResultVerified
	if err := coordinator.FinishAction(ActionFinish{
		LeaseID: lease.LeaseID, ActionID: action.ActionID,
		Result: &verified, Phase: ComputerUsePhaseIdle,
	}); err != nil {
		t.Fatalf("expired executor could not acknowledge quiescence: %v", err)
	}
	if snapshot := coordinator.Snapshot(); snapshot.Active != nil {
		t.Fatalf("expiry barrier remained after executor acknowledgement: %+v", snapshot)
	}
	if _, err := coordinator.BeginWorkflow(workflowRequest("turn-after-expired-action")); err != nil {
		t.Fatalf("new workflow could not enter after expiry quiescence: %v", err)
	}

	eventsMu.Lock()
	defer eventsMu.Unlock()
	terminal := events[len(events)-2]
	if terminal.LeaseState != ComputerUseLeaseTerminal || terminal.ActionResult == nil ||
		*terminal.ActionResult != ComputerUseResultCancelled || terminal.FailureCode == nil ||
		*terminal.FailureCode != "lease_heartbeat_expired" {
		t.Fatalf("expiry terminal event lost cancellation authority: %+v", terminal)
	}
}

func TestBeginActionRejectsInvalidPointerWithoutWedgingLease(t *testing.T) {
	coordinator, _ := newCoordinatorFixture(t, nil)
	lease, err := coordinator.BeginWorkflow(workflowRequest("turn-invalid-pointer"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-bad", ActionKind: "move",
		Pointer: &ComputerUsePointer{CoordinateSpace: ComputerUseCoordinateQuartzGlobalPoints},
		Effect:  ComputerUseActionMutation, TargetBundleID: "com.apple.Notes",
	})
	if err == nil {
		t.Fatal("invalid pointer was accepted")
	}
	if _, err := coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-good", ActionKind: "click",
		Effect: ComputerUseActionMutation, TargetBundleID: "com.apple.Notes",
	}); err != nil {
		t.Fatalf("invalid request left an action wedged: %v", err)
	}
}

func TestStopFinishCannotReportVerified(t *testing.T) {
	var eventsMu sync.Mutex
	var events []ComputerUseActivityEvent
	coordinator, _ := newCoordinatorFixture(t, func(event ComputerUseActivityEvent) {
		eventsMu.Lock()
		events = append(events, event)
		eventsMu.Unlock()
	})
	lease, err := coordinator.BeginWorkflow(workflowRequest("turn-stop-result"))
	if err != nil {
		t.Fatal(err)
	}
	action, err := coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-stop-result", ActionKind: "click",
		Effect: ComputerUseActionMutation, TargetBundleID: "com.apple.Notes",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Control(ComputerUseControlRequest{
		LeaseID: lease.LeaseID, Action: ComputerUseControlStop, IdempotencyKey: "stop-result",
	}); err != nil {
		t.Fatal(err)
	}
	verified := ComputerUseResultVerified
	if err := coordinator.FinishAction(ActionFinish{
		LeaseID: lease.LeaseID, ActionID: action.ActionID, Result: &verified,
	}); err != nil {
		t.Fatal(err)
	}
	if snapshot := coordinator.Snapshot(); snapshot.Active != nil {
		t.Fatalf("stopped action remained active: %+v", snapshot)
	}
	eventsMu.Lock()
	defer eventsMu.Unlock()
	terminal := events[len(events)-1]
	if terminal.LeaseState != ComputerUseLeaseTerminal || terminal.ActionResult == nil || *terminal.ActionResult != ComputerUseResultCancelled {
		t.Fatalf("terminal event after stop = %+v; want terminal/cancelled", terminal)
	}
}

func TestStopTombstoneSurvivesTurnCleanup(t *testing.T) {
	coordinator, _ := newCoordinatorFixture(t, nil)
	lease, err := coordinator.BeginWorkflow(workflowRequest("turn-stop-tombstone"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Control(ComputerUseControlRequest{
		LeaseID: lease.LeaseID, Action: ComputerUseControlStop, IdempotencyKey: "stop-tombstone",
	}); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.EndTurn(lease.TurnID, ComputerUseResultVerified); err != nil {
		t.Fatal(err)
	}
	_, err = coordinator.BeginWorkflow(workflowRequest(lease.TurnID))
	var stopped *StoppedTurnError
	if !errors.As(err, &stopped) {
		t.Fatalf("stopped turn was readmitted after cleanup: %T %v", err, err)
	}
}

func TestPauseAndTakeOverRetainQuiescenceBarrierUntilActionAcknowledgesCancellation(t *testing.T) {
	for _, test := range []struct {
		name       string
		control    ComputerUseControlAction
		wantResult ComputerUseActionResult
	}{
		{name: "pause", control: ComputerUseControlPause, wantResult: ComputerUseResultCancelled},
		{name: "take_over", control: ComputerUseControlTakeOver, wantResult: ComputerUseResultUserInterference},
	} {
		t.Run(test.name, func(t *testing.T) {
			coordinator, _ := newCoordinatorFixture(t, nil)
			lease, err := coordinator.BeginWorkflow(workflowRequest("turn-quiescence-" + test.name))
			if err != nil {
				t.Fatal(err)
			}
			action, err := coordinator.BeginAction(context.Background(), ActionRequest{
				LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-old", ActionKind: "click",
				Effect: ComputerUseActionMutation, TargetBundleID: "com.apple.Notes",
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := coordinator.Control(ComputerUseControlRequest{
				LeaseID: lease.LeaseID, Action: test.control, IdempotencyKey: "control-" + test.name,
			}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-action.Context.Done():
			case <-time.After(time.Second):
				t.Fatal("control did not signal cancellation")
			}

			_, err = coordinator.Control(ComputerUseControlRequest{
				LeaseID: lease.LeaseID, Action: ComputerUseControlResume, IdempotencyKey: "resume-too-early-" + test.name,
			})
			var inProgress *ActionInProgressError
			if !errors.As(err, &inProgress) {
				t.Fatalf("resume before cancellation ack error = %T %v; want *ActionInProgressError", err, err)
			}

			verified := ComputerUseResultVerified
			if err := coordinator.FinishAction(ActionFinish{
				LeaseID: lease.LeaseID, ActionID: action.ActionID,
				Phase: ComputerUsePhaseInputCommitted, Result: &verified,
			}); err != nil {
				t.Fatalf("FinishAction cancellation acknowledgement: %v", err)
			}
			snapshot := coordinator.Snapshot()
			if snapshot.Active == nil || snapshot.Active.LeaseState != ComputerUseLeasePaused ||
				snapshot.Active.ActionPhase != ComputerUsePhaseWaitingForUser ||
				snapshot.Active.ActionResult == nil || *snapshot.Active.ActionResult != test.wantResult {
				t.Fatalf("paused cancellation acknowledgement = %+v", snapshot)
			}

			if _, err := coordinator.Control(ComputerUseControlRequest{
				LeaseID: lease.LeaseID, Action: ComputerUseControlResume, IdempotencyKey: "resume-after-ack-" + test.name,
			}); err != nil {
				t.Fatalf("resume after cancellation ack: %v", err)
			}
			finishVerifiedObservation(t, coordinator, lease, "toolu-observe-after-"+test.name)
			if _, err := coordinator.BeginAction(context.Background(), ActionRequest{
				LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-new", ActionKind: "click",
				Effect: ComputerUseActionMutation, TargetBundleID: "com.apple.Notes",
			}); err != nil {
				t.Fatalf("new action after quiescence: %v", err)
			}
		})
	}
}

// An idempotent take-over retry — the exact case the idempotency key exists
// for, after the first response was lost — must not be answered 409. By then the
// executor has finished and the lease is gone, and a lease owning no action is
// trivially quiesced. A correct client reacts to 409 by NOT handing control back
// to the user, so reporting a conflict for a take-over that already succeeded
// leaves the user believing they do not have the pointer when they do.
func TestAwaitActionQuiescenceTreatsEndedLeaseAsQuiesced(t *testing.T) {
	coordinator, _ := newCoordinatorFixture(t, nil)
	lease, err := coordinator.BeginWorkflow(workflowRequest("turn-quiesced-replay"))
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.EndTurn(lease.TurnID, ComputerUseResultVerified); err != nil {
		t.Fatalf("EndTurn: %v", err)
	}
	if err := coordinator.AwaitActionQuiescence(
		context.Background(), lease.LeaseID,
	); err != nil {
		t.Fatalf("AwaitActionQuiescence on an ended lease = %v, want nil", err)
	}
}

func TestAwaitActionQuiescenceReturnsOnlyAfterFinishAction(t *testing.T) {
	coordinator, _ := newCoordinatorFixture(t, nil)
	lease, err := coordinator.BeginWorkflow(workflowRequest("turn-await-quiescence"))
	if err != nil {
		t.Fatal(err)
	}
	action, err := coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-await",
		ActionKind: "drag", Effect: ComputerUseActionMutation,
		TargetBundleID: "com.apple.Notes",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Control(ComputerUseControlRequest{
		LeaseID: lease.LeaseID, Action: ComputerUseControlTakeOver,
		IdempotencyKey: "take-over-await",
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- coordinator.AwaitActionQuiescence(context.Background(), lease.LeaseID) }()
	select {
	case err := <-done:
		t.Fatalf("quiescence returned before cleanup acknowledgement: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	verified := ComputerUseResultVerified
	if err := coordinator.FinishAction(ActionFinish{
		LeaseID: lease.LeaseID, ActionID: action.ActionID,
		Phase: ComputerUsePhaseIdle, Result: &verified,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("quiescence did not return after FinishAction")
	}
}

func TestStopWithoutActionPublishesTerminalAndClearsImmediately(t *testing.T) {
	coordinator, _ := newCoordinatorFixture(t, nil)
	lease, err := coordinator.BeginWorkflow(workflowRequest("turn-stop-idle"))
	if err != nil {
		t.Fatal(err)
	}
	request := ComputerUseControlRequest{
		LeaseID: lease.LeaseID, Action: ComputerUseControlStop, IdempotencyKey: "stop-idle",
	}
	response, err := coordinator.Control(request)
	if err != nil {
		t.Fatal(err)
	}
	if !response.Accepted || response.LeaseState != ComputerUseLeaseTerminal {
		t.Fatalf("idle stop response = %+v; want accepted terminal", response)
	}
	if snapshot := coordinator.Snapshot(); snapshot.Active != nil || snapshot.Revision != response.Revision {
		t.Fatalf("idle stop snapshot = %+v", snapshot)
	}
	repeated, err := coordinator.Control(request)
	if err != nil || repeated != response {
		t.Fatalf("idempotent terminal stop = %+v, %v; want %+v", repeated, err, response)
	}
	if _, err := coordinator.BeginWorkflow(workflowRequest("turn-after-idle-stop")); err != nil {
		t.Fatalf("next turn after idle stop: %v", err)
	}
}

func TestEndTurnCannotBypassStoppingActionBarrier(t *testing.T) {
	coordinator, _ := newCoordinatorFixture(t, nil)
	lease, err := coordinator.BeginWorkflow(workflowRequest("turn-stop-barrier"))
	if err != nil {
		t.Fatal(err)
	}
	action, err := coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-stop-barrier", ActionKind: "click",
		Effect: ComputerUseActionMutation, TargetBundleID: "com.apple.Notes",
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := coordinator.Control(ComputerUseControlRequest{
		LeaseID: lease.LeaseID, Action: ComputerUseControlStop, IdempotencyKey: "stop-barrier",
	})
	if err != nil || response.LeaseState != ComputerUseLeaseStopping {
		t.Fatalf("stop = %+v, %v", response, err)
	}
	err = coordinator.EndTurn(lease.TurnID, ComputerUseResultCancelled)
	var inProgress *ActionInProgressError
	if !errors.As(err, &inProgress) {
		t.Fatalf("EndTurn while action cancellation pending = %T %v; want *ActionInProgressError", err, err)
	}
	if snapshot := coordinator.Snapshot(); snapshot.Active == nil || snapshot.Active.LeaseState != ComputerUseLeaseStopping {
		t.Fatalf("EndTurn bypassed stop barrier: %+v", snapshot)
	}
	verified := ComputerUseResultVerified
	if err := coordinator.FinishAction(ActionFinish{
		LeaseID: lease.LeaseID, ActionID: action.ActionID, Result: &verified,
	}); err != nil {
		t.Fatal(err)
	}
	if snapshot := coordinator.Snapshot(); snapshot.Active != nil {
		t.Fatalf("FinishAction did not complete stopping lease: %+v", snapshot)
	}
}

func TestLeaseDeadlinesPreserveMonotonicClockAuthority(t *testing.T) {
	now := time.Now()
	coordinator := NewCoordinator(CoordinatorOptions{
		InstanceID: "cui_monotonic",
		Now:        func() time.Time { return now },
		NewID:      func(prefix string) string { return prefix + "_monotonic" },
		LeaseTTL:   30 * time.Second,
	})
	lease, err := coordinator.BeginWorkflow(workflowRequest("turn-monotonic"))
	if err != nil {
		t.Fatal(err)
	}
	if lease.CreatedAt != now || lease.ExpiresAt != now.Add(30*time.Second) {
		t.Fatalf("lease stripped monotonic clock: created=%v now=%v expires=%v", lease.CreatedAt, now, lease.ExpiresAt)
	}
	if !strings.Contains(lease.ExpiresAt.String(), " m=") {
		t.Fatalf("test precondition failed: deadline has no monotonic reading: %v", lease.ExpiresAt)
	}
}

func TestMutationAdmissionRequiresFrozenAllowedTarget(t *testing.T) {
	coordinator, _ := newCoordinatorFixture(t, nil)
	request := workflowRequest("turn-policy")
	lease, err := coordinator.BeginWorkflow(request)
	if err != nil {
		t.Fatal(err)
	}
	request.AllowedAppBundleIDs[0] = "com.attacker.ChangedAfterAdmission"

	for _, test := range []struct {
		name   string
		effect ComputerUseActionEffect
		target string
		wantOK bool
	}{
		{name: "missing classification", target: "com.apple.Notes"},
		{name: "mutation missing target", effect: ComputerUseActionMutation},
		{name: "mutation denied target", effect: ComputerUseActionMutation, target: "com.apple.Terminal"},
		{name: "mutation frozen allowed target", effect: ComputerUseActionMutation, target: "com.apple.Notes", wantOK: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			handle, err := coordinator.BeginAction(context.Background(), ActionRequest{
				LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-" + test.name,
				ActionKind: "click", Effect: test.effect, TargetBundleID: test.target,
			})
			if !test.wantOK {
				var denied *PolicyDeniedError
				if !errors.As(err, &denied) {
					t.Fatalf("BeginAction error = %T %v; want *PolicyDeniedError", err, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("allowed mutation: %v", err)
			}
			failed := ComputerUseResultFailed
			if err := coordinator.FinishAction(ActionFinish{LeaseID: lease.LeaseID, ActionID: handle.ActionID, Result: &failed}); err != nil {
				t.Fatal(err)
			}
		})
	}

	observation, err := coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-observe",
		ActionKind: "observe", Effect: ComputerUseActionObservation,
	})
	if err != nil {
		t.Fatalf("targetless observation: %v", err)
	}
	failed := ComputerUseResultFailed
	if err := coordinator.FinishAction(ActionFinish{LeaseID: lease.LeaseID, ActionID: observation.ActionID, Result: &failed}); err != nil {
		t.Fatal(err)
	}
}

func TestOrderedNativeBatchMaySwitchToAnotherApp(t *testing.T) {
	coordinator, _ := newCoordinatorFixture(t, nil)
	lease, err := coordinator.BeginWorkflow(workflowRequest("turn-native-switch"))
	if err != nil {
		t.Fatal(err)
	}
	action, err := coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID:            lease.LeaseID,
		TurnID:             lease.TurnID,
		ToolName:           "computer_use",
		ToolUseID:          "toolu-native-calculator",
		ActionKind:         "click",
		Effect:             ComputerUseActionMutation,
		TargetBundleID:     "com.apple.calculator",
		TargetAppName:      "Calculator",
		OrderedBatchAction: true,
	})
	if err != nil {
		t.Fatalf("native cross-app mutation: %v", err)
	}
	verified := ComputerUseResultVerified
	if err := coordinator.FinishAction(ActionFinish{
		LeaseID:  lease.LeaseID,
		ActionID: action.ActionID,
		Result:   &verified,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowLeaseAdmitsEachResolvedObservationForLaterMutation(t *testing.T) {
	coordinator, _ := newCoordinatorFixture(t, nil)
	request := workflowRequest("turn-late-bind")
	request.RequestedAppBundleID = ""
	request.RequestedAppName = ""
	request.AllowedAppBundleIDs = nil
	lease, err := coordinator.BeginWorkflow(request)
	if err != nil {
		t.Fatal(err)
	}

	_, err = coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-early-mutation",
		ActionKind: "click", Effect: ComputerUseActionMutation,
		TargetBundleID: "com.apple.Notes", TargetAppName: "Notes",
	})
	var denied *PolicyDeniedError
	if !errors.As(err, &denied) || !strings.Contains(err.Error(), "observe the running app") {
		t.Fatalf("early mutation error = %T %v; want actionable policy denial", err, err)
	}

	observation, err := coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-observe-notes",
		ActionKind: "get_app_state", Effect: ComputerUseActionObservation,
		TargetBundleID: "com.apple.Notes", TargetAppName: "Notes",
	})
	if err != nil {
		t.Fatalf("resolved observation: %v", err)
	}
	verified := ComputerUseResultVerified
	if err := coordinator.FinishAction(ActionFinish{
		LeaseID: lease.LeaseID, ActionID: observation.ActionID, Result: &verified,
	}); err != nil {
		t.Fatal(err)
	}

	bound, err := coordinator.BeginWorkflow(request)
	if err != nil {
		t.Fatal(err)
	}
	if bound.RequestedAppBundleID != "com.apple.Notes" ||
		bound.RequestedAppName != "Notes" ||
		!reflect.DeepEqual(bound.AllowedAppBundleIDs, []string{"com.apple.Notes"}) {
		t.Fatalf("late-bound lease = %+v", bound)
	}

	allowed, err := coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-click-notes",
		ActionKind: "click", Effect: ComputerUseActionMutation,
		TargetBundleID: "com.apple.Notes", TargetAppName: "Notes",
	})
	if err != nil {
		t.Fatalf("bound mutation: %v", err)
	}
	failed := ComputerUseResultFailed
	if err := coordinator.FinishAction(ActionFinish{
		LeaseID: lease.LeaseID, ActionID: allowed.ActionID, Result: &failed,
	}); err != nil {
		t.Fatal(err)
	}

	otherObservation, err := coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-observe-slack",
		ActionKind: "get_app_state", Effect: ComputerUseActionObservation,
		TargetBundleID: "com.tinyspeck.slackmacgap", TargetAppName: "Slack",
	})
	if err != nil {
		t.Fatalf("cross-app observation: %v", err)
	}
	if err := coordinator.FinishAction(ActionFinish{
		LeaseID: lease.LeaseID, ActionID: otherObservation.ActionID, Result: &verified,
	}); err != nil {
		t.Fatal(err)
	}

	bound, err = coordinator.BeginWorkflow(request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(bound.AllowedAppBundleIDs, []string{
		"com.apple.Notes", "com.tinyspeck.slackmacgap",
	}) {
		t.Fatalf("observed app targets were not admitted: %+v", bound.AllowedAppBundleIDs)
	}

	otherMutation, err := coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-click-slack",
		ActionKind: "click", Effect: ComputerUseActionMutation,
		TargetBundleID: "com.tinyspeck.slackmacgap", TargetAppName: "Slack",
	})
	if err != nil {
		t.Fatalf("observed cross-app mutation: %v", err)
	}
	if err := coordinator.FinishAction(ActionFinish{
		LeaseID: lease.LeaseID, ActionID: otherMutation.ActionID, Result: &failed,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-click-mail",
		ActionKind: "click", Effect: ComputerUseActionMutation,
		TargetBundleID: "com.apple.mail", TargetAppName: "Mail",
	})
	if !errors.As(err, &denied) || !strings.Contains(err.Error(), "not been observed") {
		t.Fatalf("unobserved cross-app mutation error = %T %v; want observation policy denial", err, err)
	}
}

func TestWorkflowLeaseDoesNotAdmitFailedObservation(t *testing.T) {
	coordinator, _ := newCoordinatorFixture(t, nil)
	request := workflowRequest("turn-failed-observation")
	request.RequestedAppBundleID = ""
	request.RequestedAppName = ""
	request.AllowedAppBundleIDs = nil
	lease, err := coordinator.BeginWorkflow(request)
	if err != nil {
		t.Fatal(err)
	}
	observation, err := coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-observe-failed",
		ActionKind: "get_app_state", Effect: ComputerUseActionObservation,
		TargetBundleID: "com.example.Editor", TargetAppName: "Editor",
	})
	if err != nil {
		t.Fatal(err)
	}
	failed := ComputerUseResultFailed
	if err := coordinator.FinishAction(ActionFinish{
		LeaseID: lease.LeaseID, ActionID: observation.ActionID, Result: &failed,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolUseID: "toolu-mutate-failed",
		ActionKind: "click", Effect: ComputerUseActionMutation,
		TargetBundleID: "com.example.Editor", TargetAppName: "Editor",
	})
	var denied *PolicyDeniedError
	if !errors.As(err, &denied) || !strings.Contains(err.Error(), "observe the running app") {
		t.Fatalf("failed observation admitted mutation: %T %v", err, err)
	}
}

func TestTombstonesAreBounded(t *testing.T) {
	fixture := &coordinatorFixture{now: time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)}
	coordinator := NewCoordinator(CoordinatorOptions{
		InstanceID: "cui_bounded_tombstones",
		Now: func() time.Time {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			return fixture.now
		},
		NewID: func(prefix string) string {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			fixture.seq++
			return fmt.Sprintf("%s_bounded_%d", prefix, fixture.seq)
		},
		LeaseTTL:       30 * time.Second,
		TombstoneLimit: 2,
	})
	for i := 1; i <= 3; i++ {
		turnID := fmt.Sprintf("turn-bounded-%d", i)
		lease, err := coordinator.BeginWorkflow(workflowRequest(turnID))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := coordinator.Control(ComputerUseControlRequest{
			LeaseID: lease.LeaseID, Action: ComputerUseControlStop,
			IdempotencyKey: fmt.Sprintf("stop-bounded-%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(coordinator.tombstones); got != 2 {
		t.Fatalf("tombstone count = %d; want 2", got)
	}
	if _, found := coordinator.tombstones["turn-bounded-1"]; found {
		t.Fatal("oldest tombstone was not evicted")
	}
	if _, err := coordinator.BeginWorkflow(workflowRequest("turn-bounded-1")); err != nil {
		t.Fatalf("evicted turn could not be admitted: %v", err)
	}
}

func TestActivitySinkReceivesCoordinatorRevisionsInOrder(t *testing.T) {
	var seenMu sync.Mutex
	var seen []uint64
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	coordinator, _ := newCoordinatorFixture(t, func(event ComputerUseActivityEvent) {
		if event.Revision == 1 {
			close(firstEntered)
			<-releaseFirst
		}
		seenMu.Lock()
		seen = append(seen, event.Revision)
		seenMu.Unlock()
	})

	beginDone := make(chan error, 1)
	go func() {
		_, err := coordinator.BeginWorkflow(workflowRequest("turn-ordered-events"))
		beginDone <- err
	}()
	<-firstEntered
	snapshot := coordinator.Snapshot()
	if snapshot.Active == nil {
		t.Fatal("active lease missing while first event was blocked")
	}
	actionDone := make(chan error, 1)
	go func() {
		_, err := coordinator.BeginAction(context.Background(), ActionRequest{
			LeaseID: snapshot.Active.LeaseID, TurnID: "turn-ordered-events", ToolUseID: "toolu-ordered",
			ActionKind: "click", Effect: ComputerUseActionMutation, TargetBundleID: "com.apple.Notes",
		})
		actionDone <- err
	}()
	time.Sleep(25 * time.Millisecond)
	close(releaseFirst)
	if err := <-beginDone; err != nil {
		t.Fatal(err)
	}
	if err := <-actionDone; err != nil {
		t.Fatal(err)
	}
	seenMu.Lock()
	defer seenMu.Unlock()
	if !reflect.DeepEqual(seen, []uint64{1, 2}) {
		t.Fatalf("sink revision order = %v; want [1 2]", seen)
	}
}

func TestAwaitControllerRequiresRealHeartbeatAndWakesWithoutPolling(t *testing.T) {
	coordinator, _ := newCoordinatorFixture(t, nil)
	lease, err := coordinator.BeginWorkflow(workflowRequest("turn-await-controller"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- coordinator.AwaitController(context.Background(), lease.LeaseID, lease.TurnID)
	}()
	select {
	case err := <-done:
		t.Fatalf("BeginWorkflow masqueraded as controller acknowledgement: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	if _, err := coordinator.Heartbeat(lease.LeaseID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("AwaitController after heartbeat: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("heartbeat did not wake AwaitController")
	}
	if err := coordinator.AwaitController(context.Background(), lease.LeaseID, lease.TurnID); err != nil {
		t.Fatalf("already acknowledged controller did not return immediately: %v", err)
	}
}

func TestAwaitControllerFailsClosedOnStateChangesAndCancellation(t *testing.T) {
	startWaiter := func(t *testing.T, coordinator *Coordinator, lease WorkflowLease) <-chan error {
		t.Helper()
		done := make(chan error, 1)
		go func() {
			done <- coordinator.AwaitController(context.Background(), lease.LeaseID, lease.TurnID)
		}()
		return done
	}
	waitFor := func(t *testing.T, done <-chan error) error {
		t.Helper()
		select {
		case err := <-done:
			return err
		case <-time.After(time.Second):
			t.Fatal("AwaitController did not wake")
			return nil
		}
	}

	t.Run("pause", func(t *testing.T) {
		coordinator, _ := newCoordinatorFixture(t, nil)
		lease, err := coordinator.BeginWorkflow(workflowRequest("turn-await-pause"))
		if err != nil {
			t.Fatal(err)
		}
		done := startWaiter(t, coordinator, lease)
		if _, err := coordinator.Control(ComputerUseControlRequest{
			LeaseID: lease.LeaseID, Action: ComputerUseControlPause, IdempotencyKey: "await-pause",
		}); err != nil {
			t.Fatal(err)
		}
		var invalid *InvalidTransitionError
		if err := waitFor(t, done); !errors.As(err, &invalid) || invalid.State != ComputerUseLeasePaused {
			t.Fatalf("pause wait error = %T %v", err, err)
		}
	})

	t.Run("stop", func(t *testing.T) {
		coordinator, _ := newCoordinatorFixture(t, nil)
		lease, err := coordinator.BeginWorkflow(workflowRequest("turn-await-stop"))
		if err != nil {
			t.Fatal(err)
		}
		done := startWaiter(t, coordinator, lease)
		if _, err := coordinator.Control(ComputerUseControlRequest{
			LeaseID: lease.LeaseID, Action: ComputerUseControlStop, IdempotencyKey: "await-stop",
		}); err != nil {
			t.Fatal(err)
		}
		var stopped *StoppedTurnError
		if err := waitFor(t, done); !errors.As(err, &stopped) {
			t.Fatalf("stop wait error = %T %v", err, err)
		}
	})

	t.Run("expiry", func(t *testing.T) {
		coordinator, fixture := newCoordinatorFixture(t, nil)
		lease, err := coordinator.BeginWorkflow(workflowRequest("turn-await-expiry"))
		if err != nil {
			t.Fatal(err)
		}
		done := startWaiter(t, coordinator, lease)
		fixture.advance(31 * time.Second)
		if !coordinator.ExpireStale() {
			t.Fatal("ExpireStale did not expire waiting lease")
		}
		var expired *LeaseExpiredError
		if err := waitFor(t, done); !errors.As(err, &expired) {
			t.Fatalf("expiry wait error = %T %v", err, err)
		}
	})

	t.Run("stale", func(t *testing.T) {
		coordinator, _ := newCoordinatorFixture(t, nil)
		lease, err := coordinator.BeginWorkflow(workflowRequest("turn-await-stale"))
		if err != nil {
			t.Fatal(err)
		}
		err = coordinator.AwaitController(context.Background(), "cul_wrong", lease.TurnID)
		var stale *StaleLeaseError
		if !errors.As(err, &stale) {
			t.Fatalf("stale wait error = %T %v", err, err)
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		coordinator, _ := newCoordinatorFixture(t, nil)
		lease, err := coordinator.BeginWorkflow(workflowRequest("turn-await-cancel"))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- coordinator.AwaitController(ctx, lease.LeaseID, lease.TurnID) }()
		cancel()
		if err := waitFor(t, done); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel wait error = %T %v", err, err)
		}
	})
}
