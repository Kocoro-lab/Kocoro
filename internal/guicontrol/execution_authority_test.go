package guicontrol

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

const (
	testRiskIntentID     = "cri_AAECAwQFBgcICQoLDA0ODw"
	testRiskTargetDigest = "tdv1_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func TestExecutionAuthorityIsOpaqueExactAndRevokedOnFinish(t *testing.T) {
	coordinator := NewCoordinator(CoordinatorOptions{})
	lease, err := coordinator.BeginWorkflow(WorkflowRequest{
		SessionID: "session-authority", TurnID: "turn-authority",
		RequestedAppBundleID: "com.example.Editor",
		AllowedAppBundleIDs:  []string{"com.example.Editor"},
		PolicySnapshotID:     "policy-authority",
	})
	if err != nil {
		t.Fatalf("BeginWorkflow: %v", err)
	}
	handle, err := coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID, ToolName: "computer_use", ToolUseID: "toolu-authority",
		ActionKind: "click", Effect: ComputerUseActionMutation,
		TargetBundleID: "com.example.Editor",
	})
	if err != nil {
		t.Fatalf("BeginAction: %v", err)
	}

	scope := ExecutionScope{
		ToolName: "computer_use", ToolUseID: "toolu-authority", ActionKind: "click",
		Effect: string(ComputerUseActionMutation), TargetBundleID: "com.example.Editor",
	}
	if ExecutionAuthorized(handle.Context, scope) {
		t.Fatal("raw action context must not carry execution authority")
	}
	authorized := handle.AuthorizeExecution(scope)
	if !ExecutionAuthorized(authorized, scope) {
		t.Fatal("exact action authority was not admitted")
	}
	otherTool := scope
	otherTool.ToolName = "accessibility"
	if ExecutionAuthorized(authorized, otherTool) {
		t.Fatal("authority was reusable for another tool")
	}
	mismatchedClaim := handle.AuthorizeExecution(otherTool)
	if !ExecutionAuthorityPresent(mismatchedClaim) || ExecutionAuthorized(mismatchedClaim, otherTool) {
		t.Fatal("mismatched daemon scope must remain distinguishable but unauthorized")
	}
	otherInvocation := scope
	otherInvocation.ToolUseID = "toolu-other"
	if ExecutionAuthorized(authorized, otherInvocation) {
		t.Fatal("authority was reusable for another tool_use_id")
	}
	if ExecutionAuthorized((ActionHandle{}).AuthorizeExecution(scope), scope) {
		t.Fatal("zero-value handle forged execution authority")
	}

	result := ComputerUseResultVerified
	if err := coordinator.FinishAction(ActionFinish{
		LeaseID: lease.LeaseID, ActionID: handle.ActionID, Result: &result,
	}); err != nil {
		t.Fatalf("FinishAction: %v", err)
	}
	if ExecutionAuthorized(authorized, scope) {
		t.Fatal("finished action retained executable authority")
	}
}

func TestExecutionAuthorityBindsExecutionLaneAndForegroundFallback(t *testing.T) {
	coordinator := NewCoordinator(CoordinatorOptions{})
	lease, err := coordinator.BeginWorkflow(WorkflowRequest{
		SessionID: "session-lane-authority", TurnID: "turn-lane-authority",
		RequestedAppBundleID: "com.example.Editor",
		AllowedAppBundleIDs:  []string{"com.example.Editor"},
		PolicySnapshotID:     "policy-lane-authority",
	})
	if err != nil {
		t.Fatalf("BeginWorkflow: %v", err)
	}
	background := ComputerUseExecutionBackgroundSemantic
	handle, err := coordinator.BeginAction(context.Background(), ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID,
		ToolName: "computer_use", ToolUseID: "toolu-lane-authority",
		ActionKind: "press", Effect: ComputerUseActionMutation,
		TargetBundleID: "com.example.Editor",
		ExecutionLane:  &background,
	})
	if err != nil {
		t.Fatalf("BeginAction: %v", err)
	}

	scope := ExecutionScope{
		ToolName: "computer_use", ToolUseID: "toolu-lane-authority",
		ActionKind: "press", Effect: string(ComputerUseActionMutation),
		TargetBundleID: "com.example.Editor",
		ExecutionLane:  string(background),
	}
	authorized := handle.AuthorizeExecution(scope)
	if !ExecutionAuthorized(authorized, scope) {
		t.Fatal("exact background semantic authority was not admitted")
	}
	foreground := scope
	foreground.ExecutionLane = string(ComputerUseExecutionForeground)
	if ExecutionAuthorized(authorized, foreground) {
		t.Fatal("background authority was reusable on foreground lane")
	}
	fallback := foreground
	fallback.ForegroundFallback = true
	if ExecutionAuthorized(authorized, fallback) {
		t.Fatal("background authority was reusable as foreground fallback")
	}
	if ExecutionAuthorized(handle.AuthorizeExecution(fallback), fallback) {
		t.Fatal("mismatched fallback claim manufactured execution authority")
	}
}

// Stop / Take Over / lease expiry must kill the capability ITSELF, not merely
// cancel its context. The final execution gate is a pure capability check, and a
// tool can still be sitting in DescribeGUIAction or a consequential-risk
// preflight when Stop lands — at which point a live authority would admit the
// action into Tool.Run.
func TestExecutionAuthorityIsRevokedByStopNotOnlyByContextCancellation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action ComputerUseControlAction
	}{
		{name: "stop", action: ComputerUseControlStop},
		{name: "take over", action: ComputerUseControlTakeOver},
	} {
		t.Run(tc.name, func(t *testing.T) {
			coordinator := NewCoordinator(CoordinatorOptions{})
			lease, err := coordinator.BeginWorkflow(WorkflowRequest{
				SessionID: "session-revoke", TurnID: "turn-revoke",
				RequestedAppBundleID: "com.example.Editor",
				AllowedAppBundleIDs:  []string{"com.example.Editor"},
				PolicySnapshotID:     "policy-revoke",
			})
			if err != nil {
				t.Fatalf("BeginWorkflow: %v", err)
			}
			handle, err := coordinator.BeginAction(context.Background(), ActionRequest{
				LeaseID: lease.LeaseID, TurnID: lease.TurnID,
				ToolName: "computer_use", ToolUseID: "toolu-revoke",
				ActionKind: "click", Effect: ComputerUseActionMutation,
				TargetBundleID: "com.example.Editor",
			})
			if err != nil {
				t.Fatalf("BeginAction: %v", err)
			}
			scope := ExecutionScope{
				ToolName: "computer_use", ToolUseID: "toolu-revoke", ActionKind: "click",
				Effect: string(ComputerUseActionMutation), TargetBundleID: "com.example.Editor",
			}
			authorized := handle.AuthorizeExecution(scope)
			if !ExecutionAuthorized(authorized, scope) {
				t.Fatal("exact action authority was not admitted")
			}

			if _, err := coordinator.Control(ComputerUseControlRequest{
				LeaseID: lease.LeaseID, Action: tc.action, IdempotencyKey: "revoke-1",
			}); err != nil {
				t.Fatalf("Control(%s): %v", tc.action, err)
			}

			if ExecutionAuthorized(authorized, scope) {
				t.Fatalf("%s retained executable authority", tc.action)
			}
			// Stripping cancellation proves the capability was revoked, rather
			// than the gate merely observing a cancelled context.
			if ExecutionAuthorized(context.WithoutCancel(authorized), scope) {
				t.Fatalf("%s cancelled the context but left the capability live", tc.action)
			}
		})
	}
}

func TestConsequentialRiskExecutionAuthorityIsExactPrivateAndRevoked(t *testing.T) {
	coordinator := NewCoordinator(CoordinatorOptions{})
	lease, err := coordinator.BeginWorkflow(WorkflowRequest{
		SessionID: "session-risk-authority", TurnID: "turn-risk-authority",
		RequestedAppBundleID: "com.example.Editor",
		AllowedAppBundleIDs:  []string{"com.example.Editor"},
		PolicySnapshotID:     "policy-risk-authority",
	})
	if err != nil {
		t.Fatalf("BeginWorkflow: %v", err)
	}
	accessibility := ComputerUseExecutionAccessibility
	request := ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID,
		ToolName: "computer_use", ToolUseID: "toolu-risk-authority",
		ActionKind: "press", Effect: ComputerUseActionMutation,
		TargetBundleID: "com.example.Editor", ExecutionPath: &accessibility,
		RiskIntentID: testRiskIntentID, RiskTargetDigest: testRiskTargetDigest,
	}
	if _, err := coordinator.StageConsequentialRisk(context.Background(), request, ConsequentialRiskMarkerV1{
		SchemaVersion: 1, Required: true, Kind: "send", IntentID: testRiskIntentID,
		ExpiresAt: "2099-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("StageConsequentialRisk: %v", err)
	}
	handle, err := coordinator.BeginAction(context.Background(), request)
	if err != nil {
		t.Fatalf("BeginAction: %v", err)
	}

	scope := ExecutionScope{
		ToolName: "computer_use", ToolUseID: "toolu-risk-authority",
		ActionKind: "press", Effect: string(ComputerUseActionMutation),
		TargetBundleID: "com.example.Editor", ExecutionPath: string(accessibility),
		RiskIntentID: testRiskIntentID, RiskTargetDigest: testRiskTargetDigest,
	}
	authorized := handle.AuthorizeExecution(scope)
	if !ExecutionAuthorized(authorized, scope) {
		t.Fatal("exact consequential-risk authority was not admitted")
	}

	for name, mutate := range map[string]func(*ExecutionScope){
		"missing intent": func(candidate *ExecutionScope) { candidate.RiskIntentID = "" },
		"missing digest": func(candidate *ExecutionScope) { candidate.RiskTargetDigest = "" },
		"other intent":   func(candidate *ExecutionScope) { candidate.RiskIntentID = "cri_EBESExQVFhcYGRobHB0eHw" },
		"other digest": func(candidate *ExecutionScope) {
			candidate.RiskTargetDigest = "tdv1_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := scope
			mutate(&candidate)
			claim := handle.AuthorizeExecution(candidate)
			if ExecutionAuthorized(claim, candidate) {
				t.Fatal("mismatched risk scope was authorized")
			}
		})
	}

	snapshot := coordinator.Snapshot()
	payload, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), testRiskIntentID) ||
		strings.Contains(string(payload), testRiskTargetDigest) {
		t.Fatalf("private risk authority leaked into activity: %s", payload)
	}

	result := ComputerUseResultCompletedUnverified
	if err := coordinator.FinishAction(ActionFinish{
		LeaseID: lease.LeaseID, ActionID: handle.ActionID, Result: &result,
	}); err != nil {
		t.Fatalf("FinishAction: %v", err)
	}
	if ExecutionAuthorized(authorized, scope) {
		t.Fatal("finished risk-bearing action retained executable authority")
	}
}

func TestCoordinateConsequentialRiskExecutionAuthorityIsClickSyntheticOnly(t *testing.T) {
	coordinator := NewCoordinator(CoordinatorOptions{})
	lease, err := coordinator.BeginWorkflow(WorkflowRequest{
		SessionID: "session-coordinate-risk-authority", TurnID: "turn-coordinate-risk-authority",
		RequestedAppBundleID: "com.example.Editor",
		AllowedAppBundleIDs:  []string{"com.example.Editor"},
		PolicySnapshotID:     "policy-coordinate-risk-authority",
	})
	if err != nil {
		t.Fatal(err)
	}
	synthetic := ComputerUseExecutionSyntheticCoordinate
	request := ActionRequest{
		LeaseID: lease.LeaseID, TurnID: lease.TurnID,
		ToolName: "computer_use", ToolUseID: "toolu-coordinate-risk-authority",
		ActionKind: "click", Effect: ComputerUseActionMutation,
		TargetBundleID: "com.example.Editor", ExecutionPath: &synthetic,
		RiskIntentID: testRiskIntentID, RiskTargetDigest: testRiskTargetDigest,
	}
	if _, err := coordinator.StageConsequentialRisk(context.Background(), request, ConsequentialRiskMarkerV1{
		SchemaVersion: 1, Required: true, Kind: "send", IntentID: testRiskIntentID,
		ExpiresAt: "2099-01-01T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	handle, err := coordinator.BeginAction(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	scope := ExecutionScope{
		ToolName: "computer_use", ToolUseID: "toolu-coordinate-risk-authority",
		ActionKind: "click", Effect: string(ComputerUseActionMutation),
		TargetBundleID: "com.example.Editor", ExecutionPath: string(synthetic),
		RiskIntentID: testRiskIntentID, RiskTargetDigest: testRiskTargetDigest,
	}
	if !ExecutionAuthorized(handle.AuthorizeExecution(scope), scope) {
		t.Fatal("exact coordinate consequential authority was not authorized")
	}
	accessibility := scope
	accessibility.ExecutionPath = string(ComputerUseExecutionAccessibility)
	if ExecutionAuthorized(handle.AuthorizeExecution(accessibility), accessibility) {
		t.Fatal("coordinate grant was reusable on accessibility path")
	}
	press := scope
	press.ActionKind = "press"
	if ExecutionAuthorized(handle.AuthorizeExecution(press), press) {
		t.Fatal("coordinate click grant was reusable as semantic press")
	}
	drag := scope
	drag.ActionKind = "drag"
	if ExecutionAuthorized(handle.AuthorizeExecution(drag), drag) {
		t.Fatal("coordinate click grant was reusable for drag")
	}
	for _, actionKind := range []string{"type", "hotkey"} {
		keyboard := scope
		keyboard.ActionKind = actionKind
		if ExecutionAuthorized(handle.AuthorizeExecution(keyboard), keyboard) {
			t.Fatalf("coordinate click grant was reusable for %s", actionKind)
		}
	}
}

func TestBeginActionRejectsInvalidConsequentialRiskAuthorityBeforeAdmission(t *testing.T) {
	accessibility := ComputerUseExecutionAccessibility
	synthetic := ComputerUseExecutionSyntheticCoordinate
	valid := ActionRequest{
		ToolName: "computer_use", ToolUseID: "toolu-risk-validation",
		ActionKind: "press", Effect: ComputerUseActionMutation,
		TargetBundleID: "com.example.Editor", ExecutionPath: &accessibility,
		RiskIntentID: testRiskIntentID, RiskTargetDigest: testRiskTargetDigest,
	}
	tests := map[string]func(*ActionRequest){
		"intent without digest": func(request *ActionRequest) { request.RiskTargetDigest = "" },
		"digest without intent": func(request *ActionRequest) { request.RiskIntentID = "" },
		"invalid intent":        func(request *ActionRequest) { request.RiskIntentID = "cri_not-random" },
		"invalid digest":        func(request *ActionRequest) { request.RiskTargetDigest = "tdv1_NOT_HEX" },
		"observation":           func(request *ActionRequest) { request.Effect = ComputerUseActionObservation },
		"legacy tool":           func(request *ActionRequest) { request.ToolName = "accessibility" },
		"non press":             func(request *ActionRequest) { request.ActionKind = "click" },
		"missing target":        func(request *ActionRequest) { request.TargetBundleID = "" },
		"missing path":          func(request *ActionRequest) { request.ExecutionPath = nil },
		"synthetic path":        func(request *ActionRequest) { request.ExecutionPath = &synthetic },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			coordinator := NewCoordinator(CoordinatorOptions{})
			lease, err := coordinator.BeginWorkflow(WorkflowRequest{
				SessionID: "session-" + name, TurnID: "turn-" + name,
				RequestedAppBundleID: "com.example.Editor",
				AllowedAppBundleIDs:  []string{"com.example.Editor"},
				PolicySnapshotID:     "policy-" + name,
			})
			if err != nil {
				t.Fatalf("BeginWorkflow: %v", err)
			}
			request := valid
			request.LeaseID, request.TurnID = lease.LeaseID, lease.TurnID
			mutate(&request)
			if _, err := coordinator.BeginAction(context.Background(), request); err == nil {
				t.Fatal("invalid consequential-risk authority was admitted")
			}
			if snapshot := coordinator.Snapshot(); snapshot.Active == nil || snapshot.Active.ActionID != "" {
				t.Fatalf("rejected risk action changed activity: %+v", snapshot.Active)
			}
		})
	}
}
