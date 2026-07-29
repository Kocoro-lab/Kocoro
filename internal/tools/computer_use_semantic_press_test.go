package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

func semanticPressTreeFixture(windowID *int) string {
	encodedWindowID := "null"
	if windowID != nil {
		encodedWindowID = fmt.Sprintf("%d", *windowID)
	}
	return `{
		"schema_version":1,"app":"Notes","app_name":"Notes","bundle_id":"com.apple.Notes",
		"pid":42,"window":"Note","window_title":"Note","window_id":` + encodedWindowID + `,
		"window_frame":{"x":0,"y":0,"width":800,"height":600},"focused_ref":null,
		"elements":[
			{"ref":"e1","fingerprint":"axf_target","path":"window[0]/AXButton[0]","role":"AXButton","value_redacted":false,"enabled":true,"focused":false,"selected":false,"actions":["AXPress"],"children":[]},
			{"ref":"e2","fingerprint":"axf_field","path":"window[0]/AXTextField[0]","role":"AXTextField","value_redacted":false,"enabled":true,"focused":false,"selected":false,"actions":[],"children":[]}
		],
		"ref_paths":{
			"e1":{"path":"window[0]/AXButton[0]","role":"AXButton","fingerprint":"axf_target"},
			"e2":{"path":"window[0]/AXTextField[0]","role":"AXTextField","fingerprint":"axf_field"}
		}
	}`
}

func semanticPressTestTool(t *testing.T, windowID *int) (*ComputerUseTool, *fakeAXCaller, string) {
	t.Helper()
	fake := newFakeAXCaller()
	tool := newTestComputerUse(fake)
	fixture := semanticPressTreeFixture(windowID)
	tree, err := decodeComputerUseTree([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	stateID := computerUseStateID(tree)
	tool.snapshot = &computerUseSnapshot{
		id: stateID, pid: 42, bundleID: "com.apple.Notes", app: "Notes", window: "Note",
		windowID: windowID, filter: "interactive", budget: 25, elements: tree.Elements, typed: true,
	}
	tool.refs = map[string]refEntry{
		"e1": {
			path: "window[0]/AXButton[0]", role: "AXButton",
			fingerprint: "axf_target", pid: 42,
		},
	}
	fake.queue("read_tree", fixture)
	return tool, fake, stateID
}

func TestComputerUseRefClickAndPressUseAtomicSemanticPress(t *testing.T) {
	windowID := 7001
	for _, action := range []string{"click", "press"} {
		t.Run(action, func(t *testing.T) {
			tool, fake, stateID := semanticPressTestTool(t, &windowID)
			var executed *SemanticPressRequestV2
			tool.semanticPressExecutor = func(
				_ context.Context, request SemanticPressRequestV2,
			) (SemanticPressResultV2, error) {
				executed = &request
				code := "postcondition_not_declared"
				return SemanticPressResultV2{
					SchemaVersion: 2, Status: "completed_unverified", CommitState: "committed",
					Phase: "post_verification", FailureCode: &code, RetrySafe: false,
				}, nil
			}

			result, err := tool.Run(context.Background(), `{"action":"`+action+`","state_id":"`+stateID+`","ref":"e1","description":"Press Save"}`)
			if err != nil || result.IsError {
				t.Fatalf("Run result=%+v err=%v", result, err)
			}
			if result.GUIOutcome == nil ||
				result.GUIOutcome.Result != agent.GUIActionResultCompletedUnverified ||
				result.GUIOutcome.Phase != agent.GUIActionPhaseVerifying ||
				result.GUIOutcome.FailureCode != "postcondition_not_declared" ||
				!result.GUIOutcome.SameObservationContinuationSafe {
				t.Fatalf("semantic typed outcome=%+v", result.GUIOutcome)
			}
			if len(fake.calls) != 1 || fake.calls[0].method != "read_tree" || executed == nil {
				t.Fatalf("AX calls = %+v executed=%+v, want read_tree then typed semantic_press_v2", fake.calls, executed)
			}
			if executed.SchemaVersion != 2 || executed.PID != 42 || executed.BundleID != "com.apple.Notes" ||
				executed.WindowID != 7001 || executed.Ref != "e1" ||
				executed.Path != "window[0]/AXButton[0]" || executed.ExpectedRole != "AXButton" ||
				executed.ExpectedFingerprint != "axf_target" || executed.FallbackPolicy != "none" {
				t.Fatalf("semantic_press_v2 request = %+v", executed)
			}
			for _, call := range fake.calls {
				if call.method == "click" || call.method == "press" || call.method == "mouse_event" {
					t.Fatalf("ref %s used forbidden legacy/synthetic RPC: %+v", action, call)
				}
			}
			if tool.snapshot != nil || tool.refs != nil {
				t.Fatal("committed semantic press did not invalidate state")
			}
		})
	}
}

func TestComputerUseNativeBatchKeepsSemanticPressObservationUntilBatchEnd(
	t *testing.T,
) {
	windowID := 7001
	tool, fake, stateID := semanticPressTestTool(t, &windowID)
	fake.queue("read_tree", semanticPressTreeFixture(&windowID))
	tool.semanticPressExecutor = func(
		_ context.Context, request SemanticPressRequestV2,
	) (SemanticPressResultV2, error) {
		code := "postcondition_not_declared"
		return SemanticPressResultV2{
			SchemaVersion: 2,
			Status:        "completed_unverified",
			CommitState:   "committed",
			Phase:         "post_verification",
			RetrySafe:     false,
			FailureCode:   &code,
		}, nil
	}
	ctx := ContextWithOpenAINativeComputerActionV1(context.Background())
	for attempt := 0; attempt < 2; attempt++ {
		result, err := tool.Run(
			ctx,
			`{"action":"press","state_id":"`+stateID+
				`","ref":"e1","description":"Press exact target"}`,
		)
		if err != nil || result.IsError {
			t.Fatalf("native semantic press %d result=%+v err=%v",
				attempt+1, result, err)
		}
	}
	if tool.snapshot == nil || tool.refs == nil {
		t.Fatal("native provider batch lost its source observation between actions")
	}
}

func TestComputerUseSemanticPressCompletedUnverifiedIsNotRetryable(t *testing.T) {
	windowID := 7001
	tool, _, stateID := semanticPressTestTool(t, &windowID)
	tool.semanticPressExecutor = func(
		context.Context, SemanticPressRequestV2,
	) (SemanticPressResultV2, error) {
		code := "postcondition_not_declared"
		return SemanticPressResultV2{
			SchemaVersion: 2, Status: "completed_unverified", CommitState: "committed",
			Phase: "post_verification", FailureCode: &code,
		}, nil
	}

	result, err := tool.Run(context.Background(), `{"action":"press","state_id":"`+stateID+`","ref":"e1","description":"Press Save"}`)
	if err != nil || result.IsError {
		t.Fatalf("Run result=%+v err=%v", result, err)
	}
	if !strings.Contains(result.Content, "completed_unverified") || !strings.Contains(result.Content, "do not retry") {
		t.Fatalf("ambiguous completion is not explicit: %s", result.Content)
	}
	if tool.snapshot != nil || tool.refs != nil {
		t.Fatal("completed_unverified did not invalidate state")
	}
}

func TestComputerUseSemanticPressSeparatesLaneTransitionFromUserInput(
	t *testing.T,
) {
	targetForeground := "target_foreground_interference"
	laneOutcome := computerUseSemanticPressGUIOutcomeV2(
		SemanticPressResultV2{
			SchemaVersion: 2,
			Status:        "user_interference",
			CommitState:   "not_committed",
			Phase:         "user_interference",
			FailureCode:   &targetForeground,
		},
	)
	if laneOutcome.Result != agent.GUIActionResultFailed {
		t.Fatalf("lane transition outcome = %+v", laneOutcome)
	}

	physicalInput := "physical_input_interference"
	userOutcome := computerUseSemanticPressGUIOutcomeV2(
		SemanticPressResultV2{
			SchemaVersion: 2,
			Status:        "user_interference",
			CommitState:   "not_committed",
			Phase:         "user_interference",
			FailureCode:   &physicalInput,
		},
	)
	if userOutcome.Result != agent.GUIActionResultUserInterference {
		t.Fatalf("physical input outcome = %+v", userOutcome)
	}
}

func TestComputerUseSemanticPressFailedInvalidatesState(t *testing.T) {
	windowID := 7001
	tool, _, stateID := semanticPressTestTool(t, &windowID)
	tool.semanticPressExecutor = func(
		context.Context, SemanticPressRequestV2,
	) (SemanticPressResultV2, error) {
		code := "fingerprint_ambiguous"
		return SemanticPressResultV2{
			SchemaVersion: 2, Status: "failed", CommitState: "not_committed",
			Phase: "preflight", FailureCode: &code,
		}, nil
	}

	result, err := tool.Run(context.Background(), `{"action":"press","state_id":"`+stateID+`","ref":"e1","description":"Press Save"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Content, "fingerprint_ambiguous") {
		t.Fatalf("failed semantic press result = %+v", result)
	}
	if tool.snapshot != nil || tool.refs != nil {
		t.Fatal("failed semantic press did not invalidate state")
	}
}

func TestComputerUseSemanticPressRejectsStaleStateAndMissingWindow(t *testing.T) {
	windowID := 7001
	t.Run("stale state", func(t *testing.T) {
		tool, fake, _ := semanticPressTestTool(t, &windowID)
		fake.responses["read_tree"] = nil
		result, err := tool.Run(context.Background(), `{"action":"press","state_id":"stale","ref":"e1","description":"Press Save"}`)
		if err != nil || !result.IsError || !strings.Contains(result.Content, "stale state_id") {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if len(fake.calls) != 0 || tool.snapshot != nil || tool.refs != nil {
			t.Fatalf("stale request mutated helper or retained state: calls=%+v snapshot=%+v", fake.calls, tool.snapshot)
		}
	})
	t.Run("missing window identity", func(t *testing.T) {
		tool, fake, stateID := semanticPressTestTool(t, nil)
		result, err := tool.Run(context.Background(), `{"action":"press","state_id":"`+stateID+`","ref":"e1","description":"Press Save"}`)
		if err != nil || !result.IsError || !strings.Contains(result.Content, "window identity") {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if len(fake.calls) != 2 ||
			fake.calls[0].method != "read_tree" ||
			fake.calls[1].method != "read_window_target" ||
			tool.snapshot != nil || tool.refs != nil {
			t.Fatalf("missing-window request skipped freshness check, reached mutation, or retained state: calls=%+v snapshot=%+v", fake.calls, tool.snapshot)
		}
	})
}

func TestComputerUseSemanticPressTransportOrMalformedResultInvalidatesState(t *testing.T) {
	windowID := 7001
	for _, test := range []struct {
		name    string
		payload string
		err     error
	}{
		{name: "transport", err: newSemanticPressCommitUnknownV2(errors.New("socket closed"))},
		{name: "malformed result", payload: `failed-without-code`},
		{name: "unsupported verified result", payload: `verified-without-predicate`},
	} {
		t.Run(test.name, func(t *testing.T) {
			tool, _, stateID := semanticPressTestTool(t, &windowID)
			tool.semanticPressExecutor = func(
				context.Context, SemanticPressRequestV2,
			) (SemanticPressResultV2, error) {
				if test.err != nil {
					return SemanticPressResultV2{}, test.err
				}
				if test.payload == "failed-without-code" {
					return SemanticPressResultV2{
						SchemaVersion: 2, Status: "failed", CommitState: "not_committed", Phase: "preflight",
					}, nil
				}
				postcondition := "target_attribute_changed"
				return SemanticPressResultV2{
					SchemaVersion: 2, Status: "verified", CommitState: "committed",
					Phase: "post_verification", Postcondition: &postcondition,
				}, nil
			}
			result, err := tool.Run(context.Background(), `{"action":"press","state_id":"`+stateID+`","ref":"e1","description":"Press Save"}`)
			if err != nil || !result.IsError {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if tool.snapshot != nil || tool.refs != nil {
				t.Fatal("unknown semantic press outcome retained state")
			}
		})
	}
}

func TestComputerUseSemanticPressV2UserInterferenceMapsCommittedPhase(t *testing.T) {
	windowID := 7001
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed_%t", committed), func(t *testing.T) {
			tool, _, stateID := semanticPressTestTool(t, &windowID)
			tool.semanticPressExecutor = func(
				context.Context, SemanticPressRequestV2,
			) (SemanticPressResultV2, error) {
				code := "physical_input_interference"
				return SemanticPressResultV2{
					SchemaVersion: 2, Status: "user_interference",
					CommitState: map[bool]string{false: "not_committed", true: "committed"}[committed],
					Phase:       "user_interference",
					FailureCode: &code,
				}, nil
			}
			result, err := tool.Run(context.Background(), `{"action":"press","state_id":"`+stateID+`","ref":"e1","description":"Press Save"}`)
			if err != nil || !result.IsError || result.GUIOutcome == nil {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			wantPhase := agent.GUIActionPhaseActing
			if committed {
				wantPhase = agent.GUIActionPhaseInputCommitted
			}
			if result.GUIOutcome.Result != agent.GUIActionResultUserInterference ||
				result.GUIOutcome.Phase != wantPhase ||
				result.GUIOutcome.FailureCode != "physical_input_interference" {
				t.Fatalf("GUI outcome = %+v", result.GUIOutcome)
			}
		})
	}
}

func TestComputerUseSemanticPressV2CommitUnknownIsNotReportedAsFailed(t *testing.T) {
	windowID := 7001
	tool, _, stateID := semanticPressTestTool(t, &windowID)
	tool.semanticPressExecutor = func(
		context.Context, SemanticPressRequestV2,
	) (SemanticPressResultV2, error) {
		code := "ax_press_commit_unknown"
		return SemanticPressResultV2{
			SchemaVersion: 2, Status: "completed_unverified", CommitState: "unknown",
			Phase: "action", FailureCode: &code,
		}, nil
	}
	result, err := tool.Run(context.Background(), `{"action":"press","state_id":"`+stateID+`","ref":"e1","description":"Press Save"}`)
	if err != nil || result.IsError || result.GUIOutcome == nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if !strings.Contains(result.Content, "may have committed") ||
		result.GUIOutcome.Result != agent.GUIActionResultCompletedUnverified ||
		result.GUIOutcome.Phase != agent.GUIActionPhaseInputCommitted ||
		result.GUIOutcome.FailureCode != "ax_press_commit_unknown" {
		t.Fatalf("commit-unknown mapping = %+v / %q", result.GUIOutcome, result.Content)
	}
}

func consequentialSemanticPressFixture(window string) string {
	fingerprint := "axf_" + strings.Repeat("a", 64)
	return fmt.Sprintf(`{
		"schema_version":1,"app":"Slack","app_name":"Slack","bundle_id":"com.tinyspeck.slackmacgap",
		"pid":42,"window":%q,"window_title":%q,"window_id":7001,
		"window_frame":{"x":0,"y":0,"width":800,"height":600},"focused_ref":null,
		"elements":[{"ref":"e1","fingerprint":%q,"path":"window[0]/AXButton[0]","role":"AXButton","title":"Send","value_redacted":false,"enabled":true,"focused":false,"selected":false,"actions":["AXPress"],"children":[]}],
		"ref_paths":{"e1":{"path":"window[0]/AXButton[0]","role":"AXButton","fingerprint":%q}}
	}`, window, window, fingerprint, fingerprint)
}

func consequentialSemanticPressTool(t *testing.T, initialWindow, liveWindow string) (*ComputerUseTool, *fakeAXCaller, string, ConsequentialRiskDraftV1) {
	t.Helper()
	fake := newFakeAXCaller()
	tool := newTestComputerUse(fake)
	initial := consequentialSemanticPressFixture(initialWindow)
	tree, err := decodeComputerUseTree([]byte(initial))
	if err != nil {
		t.Fatal(err)
	}
	_, stateID := tool.publishComputerUseObservation(tree, "interactive", 25)
	fake.queue("read_tree", consequentialSemanticPressFixture(liveWindow))
	preflight, err := tool.PreflightConsequentialRiskV1(context.Background(),
		`{"action":"press","state_id":"`+stateID+`","ref":"e1","description":"ignored"}`,
		"toolu_risk_press")
	if err != nil || preflight.Status != ConsequentialRiskPreflightRequiredV1 || preflight.Draft == nil {
		t.Fatalf("preflight=%+v err=%v", preflight, err)
	}
	return tool, fake, stateID, *preflight.Draft
}

func TestConsequentialSemanticPressFreshStateDriftBurnsNoGrantAndNeverCallsHelper(t *testing.T) {
	tool, _, stateID, approved := consequentialSemanticPressTool(t, "general - Slack", "random - Slack")
	var consumed, helperCalls int
	tool.semanticPressExecutor = func(context.Context, SemanticPressRequestV2) (SemanticPressResultV2, error) {
		helperCalls++
		return SemanticPressResultV2{}, errors.New("must not run")
	}
	ctx := ContextWithConsequentialRiskExecutionV1(context.Background(),
		"cri_AAECAwQFBgcICQoLDA0ODw", approved, func(ConsequentialRiskDraftV1) error {
			consumed++
			return nil
		})
	ctx = agent.ContextWithToolInvocation(ctx, agent.ToolInvocation{ToolName: "computer_use", ToolUseID: "toolu_risk_press"})
	result, err := tool.Run(ctx, `{"action":"press","state_id":"`+stateID+`","ref":"e1","description":"ignored"}`)
	if err != nil || !result.IsError || consumed != 0 || helperCalls != 0 {
		t.Fatalf("result=%+v err=%v consumed=%d helperCalls=%d", result, err, consumed, helperCalls)
	}
}

func TestConsequentialSemanticPressConsumesExactDetailImmediatelyBeforeHelper(t *testing.T) {
	tool, _, stateID, approved := consequentialSemanticPressTool(t, "general - Slack", "general - Slack")
	var consumed, helperCalls int
	tool.semanticPressExecutor = func(_ context.Context, request SemanticPressRequestV2) (SemanticPressResultV2, error) {
		helperCalls++
		if consumed != 1 || request.RiskDestinationAssertion == nil ||
			request.RiskDestinationAssertion.Kind != "exact_window_title" ||
			request.RiskDestinationAssertion.ExpectedWindowTitle != "general - Slack" {
			t.Fatalf("commit assertion=%+v consumed=%d", request.RiskDestinationAssertion, consumed)
		}
		code := "postcondition_not_declared"
		return SemanticPressResultV2{SchemaVersion: 2, Status: "completed_unverified", CommitState: "committed", Phase: "post_verification", FailureCode: &code}, nil
	}
	ctx := ContextWithConsequentialRiskExecutionV1(context.Background(),
		"cri_AAECAwQFBgcICQoLDA0ODw", approved, func(rederived ConsequentialRiskDraftV1) error {
			consumed++
			if !EqualConsequentialRiskDraftV1(approved, rederived) {
				return errors.New("drift")
			}
			return nil
		})
	ctx = agent.ContextWithToolInvocation(ctx, agent.ToolInvocation{ToolName: "computer_use", ToolUseID: "toolu_risk_press"})
	result, err := tool.Run(ctx, `{"action":"press","state_id":"`+stateID+`","ref":"e1","description":"ignored"}`)
	if err != nil || result.IsError || consumed != 1 || helperCalls != 1 {
		t.Fatalf("result=%+v err=%v consumed=%d helperCalls=%d", result, err, consumed, helperCalls)
	}
}
