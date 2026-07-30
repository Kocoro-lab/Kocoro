package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

func semanticScrollTreeFixture(title string) string {
	return `{
		"schema_version":1,"app":"Notes","app_name":"Notes","bundle_id":"com.apple.Notes",
		"pid":42,"window":"Note","window_title":"Note","window_id":7001,
		"window_frame":{"x":0,"y":0,"width":800,"height":600},"focused_ref":null,
		"elements":[{"ref":"e3","fingerprint":"axf_scroll_target","path":"window[0]/AXScrollArea[0]","role":"AXScrollArea","title":"` + title + `","value_redacted":false,"enabled":true,"focused":false,"selected":false,"actions":[],"children":[]}],
		"ref_paths":{"e3":{"path":"window[0]/AXScrollArea[0]","role":"AXScrollArea","fingerprint":"axf_scroll_target"}}
	}`
}

func semanticScrollTestTool(t *testing.T) (*ComputerUseTool, *fakeAXCaller, string) {
	t.Helper()
	fake := newFakeAXCaller()
	tool := newTestComputerUse(fake)
	stateID := observeNotes(t, tool, fake, semanticScrollTreeFixture("List"))
	fake.queue("read_tree", semanticScrollTreeFixture("List"))
	return tool, fake, stateID
}

func TestComputerUseSemanticScrollRequiresExactAuthorityAndNeverCallsLegacyScroll(t *testing.T) {
	requireComputerUseDarwin(t)
	tool, fake, stateID := semanticScrollTestTool(t)
	var executed *SemanticScrollRequestV1
	tool.semanticScrollExecutor = func(
		_ context.Context, request SemanticScrollRequestV1,
	) (SemanticScrollResultV1, error) {
		executed = &request
		initial, final := 0.25, 0.5
		postcondition := "scroll_value_changed_in_direction"
		return SemanticScrollResultV1{
			SchemaVersion: 1, Status: "verified", CommitState: "committed",
			Phase: "post_verification", Postcondition: &postcondition,
			InitialValue: &initial, FinalValue: &final,
			StepsCompleted: request.Steps, ExpectedSteps: request.Steps,
		}, nil
	}
	result, err := tool.Run(context.Background(), fmt.Sprintf(
		`{"action":"scroll","state_id":%q,"ref":"e3","dx":0,"dy":1,"description":"Scroll list down"}`,
		stateID))
	if err != nil || result.IsError || result.GUIOutcome == nil ||
		result.GUIOutcome.Result != agent.GUIActionResultVerified {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if executed == nil || executed.PID != 42 || executed.BundleID != "com.apple.Notes" ||
		executed.WindowID != 7001 || executed.Ref != "e3" ||
		executed.Path != "window[0]/AXScrollArea[0]" ||
		executed.ExpectedRole != "AXScrollArea" ||
		executed.ExpectedFingerprint != "axf_scroll_target" ||
		executed.Axis != "vertical" || executed.Direction != "increment" || executed.Steps != 1 {
		t.Fatalf("request lost authority: %+v", executed)
	}
	for _, call := range fake.calls {
		if call.method == "scroll" || call.method == "mouse_event" {
			t.Fatalf("used legacy/global input RPC: %+v", call)
		}
	}
	if tool.snapshot != nil || tool.refs != nil {
		t.Fatal("scroll did not consume observation authority")
	}
}

func TestComputerUseNativeBatchKeepsSemanticScrollObservationUntilBatchEnd(
	t *testing.T,
) {
	requireComputerUseDarwin(t)
	tool, fake, stateID := semanticScrollTestTool(t)
	fake.queue("read_tree", semanticScrollTreeFixture("List"))
	tool.semanticScrollExecutor = func(
		_ context.Context, request SemanticScrollRequestV1,
	) (SemanticScrollResultV1, error) {
		initial, final := 0.25, 0.5
		postcondition := "scroll_value_changed_in_direction"
		return SemanticScrollResultV1{
			SchemaVersion:  1,
			Status:         "verified",
			CommitState:    "committed",
			Phase:          "post_verification",
			Postcondition:  &postcondition,
			InitialValue:   &initial,
			FinalValue:     &final,
			StepsCompleted: request.Steps,
			ExpectedSteps:  request.Steps,
		}, nil
	}
	ctx := ContextWithOpenAINativeComputerActionV1(context.Background())
	for attempt := 0; attempt < 2; attempt++ {
		result, err := tool.Run(ctx, fmt.Sprintf(
			`{"action":"scroll","state_id":%q,"ref":"e3","dx":0,"dy":1,"description":"Scroll exact target"}`,
			stateID,
		))
		if err != nil || result.IsError {
			t.Fatalf("native semantic scroll %d result=%+v err=%v",
				attempt+1, result, err)
		}
	}
	if tool.snapshot == nil || tool.refs == nil {
		t.Fatal("native provider batch lost its source observation between actions")
	}
}

func TestComputerUseSemanticScrollSeparatesLaneTransitionFromUserInput(
	t *testing.T,
) {
	targetForeground := "target_foreground_interference"
	laneOutcome := computerUseSemanticScrollGUIOutcomeV1(
		SemanticScrollResultV1{
			SchemaVersion: 1,
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
	userOutcome := computerUseSemanticScrollGUIOutcomeV1(
		SemanticScrollResultV1{
			SchemaVersion: 1,
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

func TestComputerUseSemanticScrollMapsProviderSignsToExactAXDirection(t *testing.T) {
	for _, test := range []struct {
		dx, dy          int
		axis, direction string
		steps           int
	}{
		{dx: 3, axis: "horizontal", direction: "increment", steps: 3},
		{dx: -4, axis: "horizontal", direction: "decrement", steps: 4},
		{dy: 5, axis: "vertical", direction: "increment", steps: 5},
		{dy: -6, axis: "vertical", direction: "decrement", steps: 6},
	} {
		axis, direction, steps, ok := computerUseSemanticScrollDeltaV1(test.dx, test.dy)
		if !ok || axis != test.axis || direction != test.direction || steps != test.steps {
			t.Fatalf("dx=%d dy=%d => %s/%s/%d/%v", test.dx, test.dy, axis, direction, steps, ok)
		}
	}
}

func TestComputerUseSemanticScrollRejectsParameterBoundariesBeforeMutation(t *testing.T) {
	requireComputerUseDarwin(t)
	for _, delta := range []string{
		`"dx":0,"dy":0`, `"dx":1,"dy":1`, `"dx":0,"dy":11`, `"dx":-11,"dy":0`,
	} {
		tool, fake, stateID := semanticScrollTestTool(t)
		fake.calls = nil
		executed := false
		tool.semanticScrollExecutor = func(context.Context, SemanticScrollRequestV1) (SemanticScrollResultV1, error) {
			executed = true
			return SemanticScrollResultV1{}, nil
		}
		result, err := tool.Run(context.Background(), fmt.Sprintf(
			`{"action":"scroll","state_id":%q,"ref":"e3",%s,"description":"Scroll"}`,
			stateID, delta))
		if err != nil || !result.IsError || result.ErrorCategory != agent.ErrCategoryValidation || executed {
			t.Fatalf("delta=%s result=%+v err=%v executed=%v", delta, result, err, executed)
		}
		if len(fake.calls) != 0 {
			t.Fatalf("invalid delta reached AX: %+v", fake.calls)
		}
	}
}

func TestComputerUseSemanticScrollStaleOrFallbackNeverClaimsSuccess(t *testing.T) {
	requireComputerUseDarwin(t)
	t.Run("stale", func(t *testing.T) {
		tool, fake, _ := semanticScrollTestTool(t)
		fake.calls = nil
		result, err := tool.Run(context.Background(),
			`{"action":"scroll","state_id":"stale","ref":"e3","dy":1,"description":"Scroll"}`)
		if err != nil || !result.IsError || !strings.Contains(result.Content, "stale") || len(fake.calls) != 0 {
			t.Fatalf("result=%+v err=%v calls=%+v", result, err, fake.calls)
		}
	})
	t.Run("fallback required", func(t *testing.T) {
		tool, _, stateID := semanticScrollTestTool(t)
		tool.semanticScrollExecutor = func(context.Context, SemanticScrollRequestV1) (SemanticScrollResultV1, error) {
			code := "ax_scroll_metric_unsupported"
			return SemanticScrollResultV1{
				SchemaVersion: 1, Status: "fallback_required", CommitState: "not_committed",
				Phase: "preflight", FailureCode: &code,
				StepsCompleted: 0, ExpectedSteps: 1,
			}, nil
		}
		result, err := tool.Run(context.Background(), fmt.Sprintf(
			`{"action":"scroll","state_id":%q,"ref":"e3","dy":1,"description":"Scroll"}`,
			stateID))
		if err != nil || !result.IsError || result.GUIOutcome == nil ||
			result.GUIOutcome.Result != agent.GUIActionResultFailed ||
			result.GUIOutcome.FailureCode != "ax_scroll_metric_unsupported" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})
}

func TestComputerUseSemanticScrollMapsTypedCancellation(t *testing.T) {
	requireComputerUseDarwin(t)
	tool, _, stateID := semanticScrollTestTool(t)
	tool.semanticScrollExecutor = func(
		context.Context, SemanticScrollRequestV1,
	) (SemanticScrollResultV1, error) {
		code := "controller_cancelled"
		return SemanticScrollResultV1{
			SchemaVersion: 1, Status: "cancelled", CommitState: "committed",
			Phase: "cancelled", FailureCode: &code,
			StepsCompleted: 2, ExpectedSteps: 3,
		}, nil
	}
	result, err := tool.Run(context.Background(), fmt.Sprintf(
		`{"action":"scroll","state_id":%q,"ref":"e3","dy":3,"description":"Scroll"}`,
		stateID))
	if err != nil || !result.IsError || result.GUIOutcome == nil ||
		result.GUIOutcome.Result != agent.GUIActionResultCancelled ||
		result.GUIOutcome.Phase != agent.GUIActionPhaseInputCommitted ||
		result.GUIOutcome.FailureCode != "controller_cancelled" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
