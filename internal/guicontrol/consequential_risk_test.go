package guicontrol

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func riskActionRequest(lease WorkflowLease) ActionRequest {
	path := ComputerUseExecutionAccessibility
	return ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolName: "computer_use",
		ToolUseID: "toolu_risk_1", ActionKind: "press", ActionPhase: ComputerUsePhaseActing,
		TargetBundleID: "com.apple.Notes", ExecutionPath: &path, Effect: ComputerUseActionMutation,
		RiskIntentID:     "cri_AAECAwQFBgcICQoLDA0ODw",
		RiskTargetDigest: "tdv1_" + strings.Repeat("a", 64),
	}
}

func coordinateRiskActionRequest(lease WorkflowLease) ActionRequest {
	path := ComputerUseExecutionSyntheticCoordinate
	return ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolName: "computer_use",
		ToolUseID: "toolu_coordinate_risk_1", ActionKind: "click", ActionPhase: ComputerUsePhaseActing,
		TargetBundleID: "com.apple.Notes", ExecutionPath: &path, Effect: ComputerUseActionMutation,
		RiskIntentID:     "cri_AAECAwQFBgcICQoLDA0ODw",
		RiskTargetDigest: "tdv1_" + strings.Repeat("b", 64),
	}
}

func riskMarker() ConsequentialRiskMarkerV1 {
	return ConsequentialRiskMarkerV1{
		SchemaVersion: 1, Required: true, Kind: "send",
		IntentID: "cri_AAECAwQFBgcICQoLDA0ODw", ExpiresAt: "2026-07-22T12:00:20Z",
	}
}

func TestStageConsequentialRiskPublishesOnlyContentFreeMarkerAndBindsBeginAction(t *testing.T) {
	coordinator, _ := newCoordinatorFixture(t, nil)
	lease, err := coordinator.BeginWorkflow(workflowRequest("turn-risk-stage"))
	if err != nil {
		t.Fatal(err)
	}
	request := riskActionRequest(lease)
	handle, err := coordinator.StageConsequentialRisk(context.Background(), request, riskMarker())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := coordinator.Snapshot()
	if snapshot.Active == nil || snapshot.Active.ActionPhase != ComputerUsePhaseWaitingForUser ||
		snapshot.Active.ConsequentialRisk == nil || snapshot.Active.ConsequentialRisk.IntentID != request.RiskIntentID {
		t.Fatalf("staged snapshot=%+v", snapshot)
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"tdv1_", "destination_label", "target_digest", "#private"} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("activity leaked %q: %s", forbidden, payload)
		}
	}
	mismatch := request
	mismatch.ToolUseID = "toolu_other"
	if _, err := coordinator.BeginAction(context.Background(), mismatch); err == nil {
		t.Fatal("mismatched action consumed staged confirmation")
	}
	if _, err := coordinator.BeginAction(context.Background(), request); err != nil {
		t.Fatalf("exact BeginAction: %v", err)
	}
	select {
	case <-handle.Context.Done():
	case <-time.After(time.Second):
		t.Fatal("BeginAction did not retire staged waiter")
	}
	if active := coordinator.Snapshot().Active; active == nil || active.ConsequentialRisk != nil {
		t.Fatalf("marker survived BeginAction: %+v", active)
	}
}

func TestStopCancelsPendingConsequentialRisk(t *testing.T) {
	coordinator, _ := newCoordinatorFixture(t, nil)
	lease, err := coordinator.BeginWorkflow(workflowRequest("turn-risk-stop"))
	if err != nil {
		t.Fatal(err)
	}
	handle, err := coordinator.StageConsequentialRisk(context.Background(), riskActionRequest(lease), riskMarker())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Control(ComputerUseControlRequest{
		LeaseID: lease.LeaseID, Action: ComputerUseControlStop, IdempotencyKey: "stop-risk",
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-handle.Context.Done():
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel pending confirmation")
	}
}

func TestPendingConsequentialRiskIsCancelledByTakeOverExpiryAndEndTurn(t *testing.T) {
	for _, test := range []struct {
		name   string
		cancel func(*testing.T, *Coordinator, *coordinatorFixture, WorkflowLease)
	}{
		{name: "take over", cancel: func(t *testing.T, coordinator *Coordinator, _ *coordinatorFixture, lease WorkflowLease) {
			_, err := coordinator.Control(ComputerUseControlRequest{LeaseID: lease.LeaseID, Action: ComputerUseControlTakeOver, IdempotencyKey: "take-risk"})
			if err != nil {
				t.Fatal(err)
			}
		}},
		{name: "expiry", cancel: func(t *testing.T, coordinator *Coordinator, fixture *coordinatorFixture, _ WorkflowLease) {
			fixture.advance(31 * time.Second)
			if !coordinator.ExpireStale() {
				t.Fatal("lease did not expire")
			}
		}},
		{name: "end turn", cancel: func(t *testing.T, coordinator *Coordinator, _ *coordinatorFixture, lease WorkflowLease) {
			if err := coordinator.EndTurn(lease.TurnID, ComputerUseResultCancelled); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			coordinator, fixture := newCoordinatorFixture(t, nil)
			lease, err := coordinator.BeginWorkflow(workflowRequest("turn-risk-" + strings.ReplaceAll(test.name, " ", "-")))
			if err != nil {
				t.Fatal(err)
			}
			handle, err := coordinator.StageConsequentialRisk(context.Background(), riskActionRequest(lease), riskMarker())
			if err != nil {
				t.Fatal(err)
			}
			test.cancel(t, coordinator, fixture, lease)
			select {
			case <-handle.Context.Done():
			case <-time.After(time.Second):
				t.Fatal("pending confirmation was not cancelled")
			}
		})
	}
}

func TestCoordinateConsequentialRiskIsRevokedByStopAndTakeOver(t *testing.T) {
	for _, action := range []ComputerUseControlAction{
		ComputerUseControlStop,
		ComputerUseControlTakeOver,
	} {
		t.Run(string(action), func(t *testing.T) {
			coordinator, _ := newCoordinatorFixture(t, nil)
			lease, err := coordinator.BeginWorkflow(workflowRequest("turn-coordinate-risk-" + string(action)))
			if err != nil {
				t.Fatal(err)
			}
			request := coordinateRiskActionRequest(lease)
			handle, err := coordinator.StageConsequentialRisk(
				context.Background(), request, ConsequentialRiskMarkerV1{
					SchemaVersion: 1, Required: true, Kind: "send",
					IntentID: request.RiskIntentID, ExpiresAt: "2026-07-22T12:00:20Z",
				})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := coordinator.Control(ComputerUseControlRequest{
				LeaseID: lease.LeaseID, Action: action,
				IdempotencyKey: "coordinate-risk-" + string(action),
			}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-handle.Context.Done():
			case <-time.After(time.Second):
				t.Fatal("control did not revoke coordinate confirmation")
			}
			if _, err := coordinator.BeginAction(context.Background(), request); err == nil {
				t.Fatal("revoked coordinate authority was admitted")
			}
		})
	}
}
