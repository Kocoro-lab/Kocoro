package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

func TestComputerUseCoordinateObservationPublishesOnlyStableFramedWindow(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	harness.queueObservation(harness.tree, harness.tree)

	result, err := harness.tool.Run(context.Background(), `{
		"action":"get_app_state","include_screenshot":true,
		"description":"Inspect exact window"
	}`)
	if err != nil || result.IsError {
		t.Fatalf("observation result=%+v err=%v", result, err)
	}
	if len(result.Images) != 1 || harness.tool.coordinateArtifact == nil {
		t.Fatalf("stable observation did not atomically publish image/artifact: %+v", result)
	}
	wantMethods := []string{"read_tree", "display_topology", "capture_coordinate_window", "read_tree"}
	if got := fakeAXMethods(harness.fake.calls); !reflect.DeepEqual(got, wantMethods) {
		t.Fatalf("AX sequence = %v, want %v", got, wantMethods)
	}
	stateID := harness.tool.snapshot.id
	frame := harness.tool.coordinateArtifact.Frame()
	if frame.StateID != stateID || frame.FrameID != "frame-computer-use-001" ||
		frame.TargetPID == nil || *frame.TargetPID != harness.tree.PID ||
		frame.TargetBundleID == nil || *frame.TargetBundleID != harness.tree.BundleID ||
		frame.TargetWindowID == nil || *frame.TargetWindowID != *harness.tree.WindowID {
		t.Fatalf("artifact authority does not match AX observation: %+v", frame)
	}
	if !reflect.DeepEqual(result.Images[0], harness.tool.coordinateArtifact.ImageBlock()) {
		t.Fatal("published image is not the exact artifact image")
	}
	if strings.Contains(result.Content, frame.FrameID) || strings.Contains(result.Content, "frame_id") {
		t.Fatalf("internal frame identity leaked to model text: %s", result.Content)
	}
}

func TestComputerUseCoordinateObservationRejectsEveryTreeStabilityAxis(t *testing.T) {
	requireComputerUseDarwin(t)
	baseHarness := newComputerUseCoordinateHarness(t)
	base := baseHarness.tree
	mutations := []struct {
		name   string
		mutate func(*computerUseTree)
	}{
		{name: "tree state", mutate: func(tree *computerUseTree) {
			title := "Changed"
			tree.Elements[0].Title = &title
		}},
		{name: "pid", mutate: func(tree *computerUseTree) { tree.PID++ }},
		{name: "bundle", mutate: func(tree *computerUseTree) { tree.BundleID += ".changed" }},
		{name: "window id", mutate: func(tree *computerUseTree) { value := *tree.WindowID + 1; tree.WindowID = &value }},
		{name: "window bounds", mutate: func(tree *computerUseTree) { tree.WindowFrame.X++ }},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			harness := newComputerUseCoordinateHarness(t)
			before := cloneComputerUseTree(t, base)
			after := cloneComputerUseTree(t, base)
			test.mutate(&after)
			harness.queueObservation(before, after)
			result, err := harness.tool.Run(context.Background(), `{
				"action":"get_app_state","include_screenshot":true,
				"description":"Inspect exact window"
			}`)
			if err != nil || result.IsError {
				t.Fatalf("observation result=%+v err=%v", result, err)
			}
			if len(result.Images) != 0 || harness.tool.coordinateArtifact != nil {
				t.Fatal("unstable AX window published actionable image/artifact")
			}
			if !strings.Contains(result.Content, "elements:") || !strings.Contains(result.Content, "screenshot_warning:") {
				t.Fatalf("fallback did not retain AX text with warning: %s", result.Content)
			}
		})
	}
}

func TestComputerUseObservationFailureAndNonScreenshotClearPriorArtifact(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	harness.queueObservation(harness.tree, harness.tree)
	if result, err := harness.tool.Run(context.Background(), `{
		"action":"get_app_state","include_screenshot":true,"description":"Install artifact"
	}`); err != nil || result.IsError || harness.tool.coordinateArtifact == nil {
		t.Fatalf("initial observation result=%+v err=%v", result, err)
	}

	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	harness.fake.queue("display_topology", `{}`)
	failed, err := harness.tool.Run(context.Background(), `{
		"action":"get_app_state","include_screenshot":true,"description":"Refresh artifact"
	}`)
	if err != nil || failed.IsError || !strings.Contains(failed.Content, "screenshot_warning:") {
		t.Fatalf("failed screenshot fallback=%+v err=%v", failed, err)
	}
	if harness.tool.coordinateArtifact != nil {
		t.Fatal("failed framed observation retained prior artifact")
	}

	harness.tool.coordinateArtifact = &harness.artifact
	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	plain, err := harness.tool.Run(context.Background(), `{
		"action":"get_app_state","description":"AX only"
	}`)
	if err != nil || plain.IsError || len(plain.Images) != 0 || harness.tool.coordinateArtifact != nil {
		t.Fatalf("AX-only observation retained artifact: result=%+v err=%v", plain, err)
	}
}

func TestComputerUseStandaloneScreenshotIsExplicitlyNonActionableAndClearsArtifact(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	harness.tool.coordinateArtifact = &harness.artifact
	harness.tool.captureScreen = func(width int) (string, agent.ImageBlock, error) {
		if width != DefaultAPIWidth {
			t.Fatalf("capture width = %d", width)
		}
		return "", agent.ImageBlock{MediaType: "image/png", Data: "encoded"}, nil
	}
	result, err := harness.tool.Run(context.Background(), `{
		"action":"screenshot","description":"Capture overview"
	}`)
	if err != nil || result.IsError || len(result.Images) != 1 {
		t.Fatalf("standalone screenshot result=%+v err=%v", result, err)
	}
	if !strings.Contains(result.Content, "coordinate_space: none") ||
		!strings.Contains(result.Content, "actionable: false") {
		t.Fatalf("standalone screenshot did not disclose coordinate contract: %s", result.Content)
	}
	if harness.tool.coordinateArtifact != nil {
		t.Fatal("standalone screenshot retained actionable artifact")
	}
}

func TestComputerUseCoordinateClickMapsCurrentArtifactAndConsumesIt(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	harness.observe(t)
	stateID := harness.tool.snapshot.id
	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))

	var executed *CoordinateMouseEventRequestV1
	harness.tool.coordinateExecutor = func(_ context.Context, request CoordinateMouseEventRequestV1) (CoordinateMouseEventResultV1, error) {
		executed = &request
		endpoint := CoordinateMouseEventPointerEndpointV1{
			Requested: request.QuartzPoint, Observed: &request.QuartzPoint,
			Tolerance: coordinateMouseEndpointToleranceV1, Verified: true,
		}
		failure := "click_postcondition_not_declared"
		return CoordinateMouseEventResultV1{
			SchemaVersion: 1, Status: "completed_unverified", Action: "click",
			PrimaryActionCommitted: true, PointerMotionCommitted: true,
			Phase: "post_verification", FailureCode: &failure, RetrySafe: false,
			PointerEndpoint: &endpoint,
		}, nil
	}

	result, err := harness.tool.Run(context.Background(), fmt.Sprintf(`{
		"action":"click","state_id":%q,"x":0,"y":0,"button":"right","clicks":2,
		"description":"Coordinate fallback"
	}`, stateID))
	if err != nil || result.IsError {
		t.Fatalf("coordinate click result=%+v err=%v", result, err)
	}
	if executed == nil {
		t.Fatal("typed coordinate executor was not called")
	}
	if result.GUIOutcome == nil || result.GUIOutcome.Result != agent.GUIActionResultCompletedUnverified ||
		result.GUIOutcome.Phase != agent.GUIActionPhaseVerifying || result.GUIOutcome.Pointer == nil ||
		result.GUIOutcome.Pointer.DisplayID != 9 ||
		result.GUIOutcome.Pointer.TopologyID != harness.topology.TopologyID ||
		result.GUIOutcome.Pointer.TopologyGeneration != harness.topology.Generation ||
		result.GUIOutcome.Pointer.X != -99.5 || result.GUIOutcome.Pointer.Y != 200.5 {
		t.Fatalf("coordinate typed outcome lost authority: %+v", result.GUIOutcome)
	}
	if executed.Action != "click" || executed.Button == nil || *executed.Button != "right" ||
		executed.ClickCount == nil || *executed.ClickCount != 2 ||
		executed.QuartzPoint.X != -99.5 || executed.QuartzPoint.Y != 200.5 ||
		executed.DisplayID != 9 || executed.HelperBootID != harness.topology.HelperBootID {
		t.Fatalf("coordinate request lost mapped authority: %+v", *executed)
	}
	wantDeadline := harness.now.Add(time.Second).Format(time.RFC3339Nano)
	if executed.CommitDeadlineAt != wantDeadline {
		t.Fatalf("deadline = %s, want %s", executed.CommitDeadlineAt, wantDeadline)
	}
	if !strings.Contains(result.Content, "do not retry automatically") ||
		strings.Contains(result.Content, "frame_id") || strings.Contains(result.Content, "frame-computer-use-001") {
		t.Fatalf("coordinate result contract leaked or omitted safety: %s", result.Content)
	}
	if harness.tool.snapshot != nil || harness.tool.refs != nil || harness.tool.coordinateArtifact != nil {
		t.Fatal("coordinate attempt did not consume snapshot/artifact")
	}
	if got := fakeAXMethods(harness.fake.calls[len(harness.fake.calls)-2:]); !reflect.DeepEqual(got, []string{"read_tree", "display_topology"}) {
		t.Fatalf("coordinate preflight calls = %v", got)
	}
	for _, call := range harness.fake.calls {
		if call.method == "mouse_event" {
			t.Fatal("coordinate path delegated to legacy mouse_event")
		}
	}
}

func TestComputerUseCoordinateClickPreservesOfficialOpenAIButtons(t *testing.T) {
	requireComputerUseDarwin(t)
	for _, button := range []string{"wheel", "back", "forward"} {
		t.Run(button, func(t *testing.T) {
			harness := newComputerUseCoordinateHarness(t)
			harness.observe(t)
			stateID := harness.tool.snapshot.id
			harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
			harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))

			var executed *CoordinateMouseEventRequestV1
			harness.tool.coordinateExecutor = func(
				_ context.Context,
				request CoordinateMouseEventRequestV1,
			) (CoordinateMouseEventResultV1, error) {
				executed = &request
				endpoint := CoordinateMouseEventPointerEndpointV1{
					Requested: request.QuartzPoint,
					Observed:  &request.QuartzPoint,
					Tolerance: coordinateMouseEndpointToleranceV1,
					Verified:  true,
				}
				failure := "click_postcondition_not_declared"
				return CoordinateMouseEventResultV1{
					SchemaVersion:          1,
					Status:                 "completed_unverified",
					Action:                 "click",
					PrimaryActionCommitted: true,
					PointerMotionCommitted: true,
					Phase:                  "post_verification",
					FailureCode:            &failure,
					RetrySafe:              false,
					PointerEndpoint:        &endpoint,
				}, nil
			}

			result, err := harness.tool.Run(
				context.Background(),
				fmt.Sprintf(`{
					"action":"click","state_id":%q,"x":0,"y":0,
					"button":%q,"description":"OpenAI official click button"
				}`, stateID, button),
			)
			if err != nil || result.IsError {
				t.Fatalf("coordinate click result=%+v err=%v", result, err)
			}
			if executed == nil || executed.Button == nil || *executed.Button != button {
				t.Fatalf("coordinate request lost button: %+v", executed)
			}
		})
	}
}

func TestComputerUseCoordinateRiskClickConsumesExactGrantImmediatelyBeforeHelper(t *testing.T) {
	requireComputerUseDarwin(t)
	harness, stateID := coordinateRiskHarness(t, "Send", nil)
	args := fmt.Sprintf(`{
		"action":"click","state_id":%q,"x":0,"y":0,"description":"ignored"
	}`, stateID)
	harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))
	approved, err := harness.tool.PreflightConsequentialRiskV1(
		context.Background(), args, "toolu_coordinate_risk_execute")
	if err != nil || approved.Draft == nil {
		t.Fatalf("approved=%+v err=%v", approved, err)
	}
	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))

	consumed := 0
	helperCalled := false
	harness.tool.coordinateExecutor = func(
		_ context.Context, request CoordinateMouseEventRequestV1,
	) (CoordinateMouseEventResultV1, error) {
		helperCalled = true
		if consumed != 1 {
			t.Fatalf("helper called before exact grant burn: consumed=%d", consumed)
		}
		assertion := request.RiskAssertion
		if assertion == nil || assertion.Kind != "consequential_click_v1" ||
			assertion.RiskKind != "send" || assertion.ElementRef != "e1" ||
			assertion.ExpectedRole != approved.Draft.Target.Role ||
			assertion.ExpectedFingerprint != approved.Draft.Target.Fingerprint ||
			assertion.CoordinateAuthority != *approved.Draft.Target.CoordinateAuthority ||
			assertion.DestinationAssertion.ExpectedWindowTitle != "Fixture Window" {
			t.Fatalf("helper request lost exact risk assertion: %+v", request)
		}
		failure := "click_postcondition_not_declared"
		return CoordinateMouseEventResultV1{
			SchemaVersion: 1, Status: "completed_unverified", Action: "click",
			PrimaryActionCommitted: true, PointerMotionCommitted: true,
			Phase: "post_verification", FailureCode: &failure,
			PointerEndpoint: &CoordinateMouseEventPointerEndpointV1{
				Requested: request.QuartzPoint, Observed: &request.QuartzPoint,
				Tolerance: coordinateMouseEndpointToleranceV1, Verified: true,
			},
		}, nil
	}
	ctx := ContextWithConsequentialRiskExecutionV1(
		context.Background(), "cri_AAECAwQFBgcICQoLDA0ODw", *approved.Draft,
		func(rederived ConsequentialRiskDraftV1) error {
			if !EqualConsequentialRiskDraftV1(rederived, *approved.Draft) {
				t.Fatal("grant consumer received drifted coordinate draft")
			}
			consumed++
			return nil
		})
	result, err := harness.tool.Run(ctx, args)
	if err != nil || result.IsError || !helperCalled || consumed != 1 {
		t.Fatalf("result=%+v err=%v helper=%v consumed=%d", result, err, helperCalled, consumed)
	}
}

func TestComputerUseCoordinateRiskClickWithoutGrantAndVariantGrantNeverReachHelper(t *testing.T) {
	for _, test := range []struct {
		name, suffix, wantCode string
		withGrant              bool
	}{
		{name: "missing grant", wantCode: ConsequentialRiskCodeMissingGrantV1},
		{name: "right cannot borrow", suffix: `,"button":"right"`, wantCode: ConsequentialRiskCodeUnsupportedPathV1, withGrant: true},
		{name: "double cannot borrow", suffix: `,"clicks":2`, wantCode: ConsequentialRiskCodeUnsupportedPathV1, withGrant: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireComputerUseDarwin(t)
			harness, stateID := coordinateRiskHarness(t, "Send", nil)
			baseArgs := fmt.Sprintf(`{
				"action":"click","state_id":%q,"x":0,"y":0,"description":"ignored"%s
			}`, stateID, test.suffix)
			var ctx context.Context = agent.ContextWithToolInvocation(
				context.Background(), agent.ToolInvocation{
					ToolName: "computer_use", ToolUseID: "toolu_coordinate_variant",
				})
			consumed := 0
			if test.withGrant {
				approvedArgs := fmt.Sprintf(`{
					"action":"click","state_id":%q,"x":0,"y":0,"description":"ignored"
				}`, stateID)
				harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))
				approved, err := harness.tool.PreflightConsequentialRiskV1(
					context.Background(), approvedArgs, "toolu_coordinate_variant")
				if err != nil || approved.Draft == nil {
					t.Fatalf("approved=%+v err=%v", approved, err)
				}
				ctx = ContextWithConsequentialRiskExecutionV1(
					ctx, "cri_AAECAwQFBgcICQoLDA0ODw", *approved.Draft,
					func(ConsequentialRiskDraftV1) error { consumed++; return nil })
			}
			harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
			harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))
			helperCalled := false
			harness.tool.coordinateExecutor = func(
				context.Context, CoordinateMouseEventRequestV1,
			) (CoordinateMouseEventResultV1, error) {
				helperCalled = true
				return CoordinateMouseEventResultV1{}, nil
			}
			result, err := harness.tool.Run(ctx, baseArgs)
			if err != nil || !result.IsError || helperCalled || consumed != 0 ||
				result.GUIOutcome == nil || result.GUIOutcome.FailureCode != test.wantCode {
				t.Fatalf("result=%+v err=%v helper=%v consumed=%d", result, err, helperCalled, consumed)
			}
		})
	}
}

func TestComputerUseCoordinateRiskGrantCannotSurviveReobservation(t *testing.T) {
	requireComputerUseDarwin(t)
	harness, oldStateID := coordinateRiskHarness(t, "Send", nil)
	oldArgs := fmt.Sprintf(`{
		"action":"click","state_id":%q,"x":0,"y":0,"description":"ignored"
	}`, oldStateID)
	harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))
	approved, err := harness.tool.PreflightConsequentialRiskV1(
		context.Background(), oldArgs, "toolu_coordinate_reobserve")
	if err != nil || approved.Draft == nil {
		t.Fatalf("approved=%+v err=%v", approved, err)
	}

	// A new observation at the same pixel but with a different exact AX
	// fingerprint mints a new state/frame authority. The old grant must not be
	// consumed for that newly observed target.
	newFingerprint := "axf_" + strings.Repeat("d", 64)
	harness.tree.Elements[0].Fingerprint = newFingerprint
	harness.tree.RefPaths["e1"] = computerUseRefPath{
		Path: "window[0]/AXButton[0]", Role: "AXButton", Fingerprint: newFingerprint,
	}
	harness.observe(t)
	newStateID := harness.tool.snapshot.id
	if newStateID == oldStateID {
		t.Fatal("fixture did not create a new observed state")
	}
	newArgs := fmt.Sprintf(`{
		"action":"click","state_id":%q,"x":0,"y":0,"description":"ignored"
	}`, newStateID)
	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))

	consumed := 0
	helperCalled := false
	harness.tool.coordinateExecutor = func(
		context.Context, CoordinateMouseEventRequestV1,
	) (CoordinateMouseEventResultV1, error) {
		helperCalled = true
		return CoordinateMouseEventResultV1{}, nil
	}
	ctx := ContextWithConsequentialRiskExecutionV1(
		context.Background(), "cri_AAECAwQFBgcICQoLDA0ODw", *approved.Draft,
		func(ConsequentialRiskDraftV1) error { consumed++; return nil })
	result, err := harness.tool.Run(ctx, newArgs)
	if err != nil || !result.IsError || helperCalled || consumed != 0 ||
		result.GUIOutcome == nil ||
		result.GUIOutcome.FailureCode != ConsequentialRiskCodeGrantMismatchV1 {
		t.Fatalf("result=%+v err=%v helper=%v consumed=%d", result, err, helperCalled, consumed)
	}
}

func TestComputerUseCoordinateRiskGrantCannotAuthorizeDragOrKeyboard(t *testing.T) {
	for _, actionArgs := range []string{
		`{"action":"drag","state_id":%q,"start_x":0,"start_y":0,"end_x":1,"end_y":1,"description":"ignored"}`,
		`{"action":"hotkey","state_id":%q,"keys":"CMD+RETURN","description":"ignored"}`,
		`{"action":"type","state_id":%q,"ref":"e1","text":"ignored","description":"ignored"}`,
	} {
		t.Run(actionArgs, func(t *testing.T) {
			requireComputerUseDarwin(t)
			harness, stateID := coordinateRiskHarness(t, "Send", nil)
			clickArgs := fmt.Sprintf(`{
				"action":"click","state_id":%q,"x":0,"y":0,"description":"ignored"
			}`, stateID)
			harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))
			approved, err := harness.tool.PreflightConsequentialRiskV1(
				context.Background(), clickArgs, "toolu_coordinate_other_action")
			if err != nil || approved.Draft == nil {
				t.Fatalf("approved=%+v err=%v", approved, err)
			}
			consumed := 0
			ctx := ContextWithConsequentialRiskExecutionV1(
				context.Background(), "cri_AAECAwQFBgcICQoLDA0ODw", *approved.Draft,
				func(ConsequentialRiskDraftV1) error { consumed++; return nil })
			result, err := harness.tool.Run(ctx, fmt.Sprintf(actionArgs, stateID))
			if err != nil || !result.IsError || consumed != 0 ||
				result.GUIOutcome == nil ||
				result.GUIOutcome.FailureCode != ConsequentialRiskCodeGrantMismatchV1 {
				t.Fatalf("result=%+v err=%v consumed=%d", result, err, consumed)
			}
		})
	}
}

func TestComputerUseCoordinateClickSurfacesUserInterferenceForCoordinatorPause(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	harness.observe(t)
	stateID := harness.tool.snapshot.id
	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))
	harness.tool.coordinateExecutor = func(
		_ context.Context,
		request CoordinateMouseEventRequestV1,
	) (CoordinateMouseEventResultV1, error) {
		observed := request.QuartzPoint
		observed.X += 4
		failure := "physical_input_interference"
		return CoordinateMouseEventResultV1{
			SchemaVersion: 1, Status: "user_interference", Action: "click",
			PrimaryActionCommitted: true, PointerMotionCommitted: true,
			Phase: "user_interference", FailureCode: &failure, RetrySafe: false,
			PointerEndpoint: &CoordinateMouseEventPointerEndpointV1{
				Requested: request.QuartzPoint, Observed: &observed,
				Tolerance: coordinateMouseEndpointToleranceV1, Verified: false,
			},
		}, nil
	}

	result, err := harness.tool.Run(context.Background(), fmt.Sprintf(`{
		"action":"click","state_id":%q,"x":0,"y":0,
		"description":"Yield to user"
	}`, stateID))
	if err != nil || !result.IsError {
		t.Fatalf("interference result=%+v err=%v", result, err)
	}
	if result.GUIOutcome == nil ||
		result.GUIOutcome.Result != agent.GUIActionResultUserInterference ||
		result.GUIOutcome.FailureCode != "physical_input_interference" {
		t.Fatalf("interference did not reach coordinator outcome: %+v", result.GUIOutcome)
	}
	if !strings.Contains(result.Content, "user interference") ||
		!strings.Contains(result.Content, "re-observe") {
		t.Fatalf("interference guidance missing: %s", result.Content)
	}
}

func TestComputerUseCoordinateActionUsesCaptureTimeCGBoundsAfterToleratedAXOffset(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	// AX and CGWindow bounds are correlated with a documented +/-2 point
	// tolerance. The actionable frame must retain the exact CG bounds captured
	// by the helper rather than later passing the offset AX bounds to an exact
	// CGWindow preflight.
	harness.tree.WindowFrame.X++
	harness.observe(t)
	stateID := harness.tool.snapshot.id
	frame := harness.tool.coordinateArtifact.Frame()
	if frame.CapturedQuartzRect.X == harness.tree.WindowFrame.X {
		t.Fatal("fixture did not exercise asymmetric AX/CG bounds")
	}
	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))

	var executed *CoordinateMouseEventRequestV1
	harness.tool.coordinateExecutor = func(_ context.Context, request CoordinateMouseEventRequestV1) (CoordinateMouseEventResultV1, error) {
		executed = &request
		endpoint := CoordinateMouseEventPointerEndpointV1{
			Requested: request.QuartzPoint, Observed: &request.QuartzPoint,
			Tolerance: coordinateMouseEndpointToleranceV1, Verified: true,
		}
		return CoordinateMouseEventResultV1{
			SchemaVersion: 1, Status: "completed", Action: "move",
			PrimaryActionCommitted: true, PointerMotionCommitted: true,
			Phase: "post_verification", PointerEndpoint: &endpoint,
		}, nil
	}
	result, err := harness.tool.Run(context.Background(), fmt.Sprintf(`{
		"action":"move","state_id":%q,"x":0,"y":0,"description":"Move"
	}`, stateID))
	if err != nil || result.IsError || executed == nil {
		t.Fatalf("coordinate move result=%+v request=%+v err=%v", result, executed, err)
	}
	if result.GUIOutcome == nil || result.GUIOutcome.Result != agent.GUIActionResultVerified ||
		result.GUIOutcome.Pointer == nil || result.GUIOutcome.Pointer.DisplayID != 9 {
		t.Fatalf("verified move typed outcome=%+v", result.GUIOutcome)
	}
	if executed.ExpectedWindowQuartzBounds != frame.CapturedQuartzRect {
		t.Fatalf("helper exact bounds = %+v, want capture-time CG bounds %+v",
			executed.ExpectedWindowQuartzBounds, frame.CapturedQuartzRect)
	}
}

func TestComputerUseCoordinateActionRejectsArtifactTargetAuthorityMismatch(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	harness.observe(t)
	stateID := harness.tool.snapshot.id
	wrongPID := harness.tree.PID + 1
	harness.tool.coordinateArtifact.frame.TargetPID = &wrongPID
	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))

	called := false
	harness.tool.coordinateExecutor = func(_ context.Context, request CoordinateMouseEventRequestV1) (CoordinateMouseEventResultV1, error) {
		called = true
		endpoint := CoordinateMouseEventPointerEndpointV1{
			Requested: request.QuartzPoint, Observed: &request.QuartzPoint,
			Tolerance: coordinateMouseEndpointToleranceV1, Verified: true,
		}
		return CoordinateMouseEventResultV1{
			SchemaVersion: 1, Status: "completed", Action: "move",
			PrimaryActionCommitted: true, PointerMotionCommitted: true,
			Phase: "post_verification", PointerEndpoint: &endpoint,
		}, nil
	}
	result, err := harness.tool.Run(context.Background(), fmt.Sprintf(`{
		"action":"move","state_id":%q,"x":0,"y":0,"description":"Move"
	}`, stateID))
	if err != nil || !result.IsError || !strings.Contains(result.Content, "authority") {
		t.Fatalf("mismatched artifact authority result=%+v err=%v", result, err)
	}
	if called {
		t.Fatal("mismatched artifact target authority reached executor")
	}
}

func TestComputerUseCoordinateFrameAuthorityUsesSymmetricTwoPointAXCGTolerance(t *testing.T) {
	pid := 4242
	bundleID := "com.example.fixture"
	windowID := 7001
	frame := CoordinateFrameV1{
		TargetPID:      &pid,
		TargetBundleID: &bundleID,
		TargetWindowID: &windowID,
		CapturedQuartzRect: CoordinateQuartzRectV1{
			X: -100, Y: 100, Width: 800, Height: 600,
		},
	}
	target := CaptureCoordinateWindowRequestV1{
		PID: pid, BundleID: bundleID, WindowID: uint32(windowID),
		ExpectedQuartzBounds: CoordinateQuartzRectV1{
			X: -98, Y: 98, Width: 802, Height: 598,
		},
	}
	if err := computerUseCoordinateFrameMatchesCurrentTargetV1(frame, target); err != nil {
		t.Fatalf("mixed +2/-2 AX/CG offsets should correlate: %v", err)
	}

	target.ExpectedQuartzBounds.X = -97.999999
	if err := computerUseCoordinateFrameMatchesCurrentTargetV1(frame, target); err == nil {
		t.Fatal("AX/CG offset beyond +2 points unexpectedly correlated")
	}
	target.ExpectedQuartzBounds = CoordinateQuartzRectV1{
		X: -102, Y: 102, Width: 798, Height: 602,
	}
	if err := computerUseCoordinateFrameMatchesCurrentTargetV1(frame, target); err != nil {
		t.Fatalf("opposite mixed -2/+2 AX/CG offsets should correlate: %v", err)
	}
	target.ExpectedQuartzBounds.Y = 102.000001
	if err := computerUseCoordinateFrameMatchesCurrentTargetV1(frame, target); err == nil {
		t.Fatal("AX/CG offset beyond +2 points unexpectedly correlated")
	}
}

func TestComputerUseCoordinateAttemptRequiresMatchingStateAndArtifactAndAlwaysConsumes(t *testing.T) {
	requireComputerUseDarwin(t)
	for _, test := range []struct {
		name   string
		args   func(string) string
		mutate func(*computerUseCoordinateHarness)
		want   string
	}{
		{
			name: "missing state id",
			args: func(string) string { return `{"action":"move","x":0,"y":0,"description":"Move"}` },
			want: "state_id",
		},
		{
			name: "stale state id",
			args: func(string) string { return `{"action":"move","state_id":"stale","x":0,"y":0,"description":"Move"}` },
			want: "stale state_id",
		},
		{
			name:   "missing artifact",
			mutate: func(h *computerUseCoordinateHarness) { h.tool.coordinateArtifact = nil },
			args: func(state string) string {
				return fmt.Sprintf(`{"action":"move","state_id":%q,"x":0,"y":0,"description":"Move"}`, state)
			},
			want: "actionable screenshot",
		},
		{
			name: "outside image",
			args: func(state string) string {
				return fmt.Sprintf(`{"action":"move","state_id":%q,"x":999,"y":0,"description":"Move"}`, state)
			},
			want: "outside_final_image",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newComputerUseCoordinateHarness(t)
			harness.observe(t)
			stateID := harness.tool.snapshot.id
			if test.mutate != nil {
				test.mutate(harness)
			}
			if test.name == "outside image" {
				harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
				harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))
			}
			called := false
			harness.tool.coordinateExecutor = func(context.Context, CoordinateMouseEventRequestV1) (CoordinateMouseEventResultV1, error) {
				called = true
				return CoordinateMouseEventResultV1{}, nil
			}
			result, err := harness.tool.Run(context.Background(), test.args(stateID))
			if err != nil || !result.IsError || !strings.Contains(result.Content, test.want) {
				t.Fatalf("attempt result=%+v err=%v, want %q", result, err, test.want)
			}
			if called {
				t.Fatal("invalid coordinate attempt reached executor")
			}
			if harness.tool.snapshot != nil || harness.tool.refs != nil || harness.tool.coordinateArtifact != nil {
				t.Fatal("failed coordinate attempt did not consume state/artifact")
			}
		})
	}
}

func TestComputerUseCoordinateCommitUnknownIsNonRetryableBusinessError(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	harness.observe(t)
	stateID := harness.tool.snapshot.id
	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))
	harness.tool.coordinateExecutor = func(context.Context, CoordinateMouseEventRequestV1) (CoordinateMouseEventResultV1, error) {
		return CoordinateMouseEventResultV1{}, &CoordinateMouseEventCommitUnknownErrorV1{cause: errors.New("EOF")}
	}
	result, err := harness.tool.Run(context.Background(), fmt.Sprintf(`{
		"action":"move","state_id":%q,"x":0,"y":0,"description":"Move"
	}`, stateID))
	if err != nil || !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness || result.IsRetryable ||
		!strings.Contains(result.Content, "commit status is unknown") ||
		!strings.Contains(result.Content, "do not retry automatically") ||
		result.GUIOutcome == nil ||
		result.GUIOutcome.Result != agent.GUIActionResultCompletedUnverified ||
		result.GUIOutcome.Phase != agent.GUIActionPhaseInputCommitted ||
		result.GUIOutcome.FailureCode != "commit_unknown" {
		t.Fatalf("commit-unknown result=%+v err=%v", result, err)
	}
}

func TestComputerUseCoordinateInvalidHelperResultIsCompletedUnverified(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	harness.observe(t)
	stateID := harness.tool.snapshot.id
	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))
	harness.tool.coordinateExecutor = func(
		context.Context, CoordinateMouseEventRequestV1,
	) (CoordinateMouseEventResultV1, error) {
		return CoordinateMouseEventResultV1{}, nil
	}
	result, err := harness.tool.Run(context.Background(), fmt.Sprintf(`{
		"action":"move","state_id":%q,"x":0,"y":0,"description":"Move"
	}`, stateID))
	if err != nil || !result.IsError ||
		result.GUIOutcome == nil ||
		result.GUIOutcome.Result != agent.GUIActionResultCompletedUnverified ||
		result.GUIOutcome.Phase != agent.GUIActionPhaseInputCommitted ||
		result.GUIOutcome.FailureCode != "invalid_helper_result" {
		t.Fatalf("invalid helper result=%+v err=%v", result, err)
	}
}

func TestComputerUseCoordinateExplicitHelperFailureRemainsDistinctFromCommitUnknown(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	harness.observe(t)
	stateID := harness.tool.snapshot.id
	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))
	harness.tool.coordinateExecutor = func(context.Context, CoordinateMouseEventRequestV1) (CoordinateMouseEventResultV1, error) {
		return DecodeCoordinateMouseEventResultV1(
			loadCoordinateFixture(t, "coordinate_mouse_event.response.failed.stale_topology.v1.json"))
	}
	result, err := harness.tool.Run(context.Background(), fmt.Sprintf(`{
		"action":"click","state_id":%q,"x":0,"y":0,"description":"Click"
	}`, stateID))
	if err != nil || !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness || result.IsRetryable ||
		!strings.Contains(result.Content, "failed during preflight: stale_topology") ||
		strings.Contains(result.Content, "commit status is unknown") {
		t.Fatalf("explicit helper failure result=%+v err=%v", result, err)
	}
}

func TestComputerUseCoordinateDeadlineIsCappedByFrameExpiry(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	harness.observe(t)
	stateID := harness.tool.snapshot.id
	nearExpiry := harness.fixture.captureAt.Add(computerUseCoordinateFrameTTLV1 - 250*time.Millisecond)
	harness.tool.coordinateNow = func() time.Time { return nearExpiry }
	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))
	var deadline string
	harness.tool.coordinateExecutor = func(_ context.Context, request CoordinateMouseEventRequestV1) (CoordinateMouseEventResultV1, error) {
		deadline = request.CommitDeadlineAt
		endpoint := CoordinateMouseEventPointerEndpointV1{
			Requested: request.QuartzPoint, Observed: &request.QuartzPoint,
			Tolerance: coordinateMouseEndpointToleranceV1, Verified: true,
		}
		return CoordinateMouseEventResultV1{
			SchemaVersion: 1, Status: "completed", Action: "move",
			PrimaryActionCommitted: true, PointerMotionCommitted: true,
			Phase: "post_verification", PointerEndpoint: &endpoint,
		}, nil
	}
	result, err := harness.tool.Run(context.Background(), fmt.Sprintf(`{
		"action":"move","state_id":%q,"x":0,"y":0,"description":"Move"
	}`, stateID))
	if err != nil || result.IsError {
		t.Fatalf("move result=%+v err=%v", result, err)
	}
	want := harness.fixture.captureAt.Add(computerUseCoordinateFrameTTLV1).Format(time.RFC3339Nano)
	if deadline != want {
		t.Fatalf("deadline = %s, want frame expiry %s", deadline, want)
	}
}

func TestComputerUseCoordinateDefaultsMatchCanonicalProfileAndBoundRawCapture(t *testing.T) {
	if computerUseCoordinateFrameTTLV1 != CoordinateFrameMaxTTLV1 {
		t.Fatalf("computer_use frame TTL = %s, want CoordinateFrameMaxTTLV1",
			computerUseCoordinateFrameTTLV1)
	}
	if CoordinateFrameMaxTTLV1 != 30*time.Second {
		t.Fatalf("computer_use frame TTL = %s, want exact v1 maximum 30s",
			computerUseCoordinateFrameTTLV1)
	}
	profile, err := DecodeCoordinateImageProfileV1(
		loadCoordinateFixture(t, "coordinate_image_profile.default.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := defaultComputerUseCoordinateImageProfileV1(); !reflect.DeepEqual(got, profile) {
		t.Fatalf("production coordinate profile drifted from fixture:\n got=%+v\nwant=%+v", got, profile)
	}
	limits := defaultComputerUseCoordinateCaptureLimitsV1()
	if limits.MaxRawBytes != computerUseCoordinateMaxRawCaptureBytesV1 ||
		limits.MaxNDJSONBytes != computerUseCoordinateMaxCaptureNDJSONBytesV1 ||
		limits.MaxPixels != computerUseCoordinateMaxCapturePixelsV1 ||
		base64.StdEncoding.EncodedLen(limits.MaxRawBytes)+64*1024 > limits.MaxNDJSONBytes ||
		limits.MaxNDJSONBytes >= axMaxResponseLine {
		t.Fatalf("unsafe production coordinate capture limits: %+v", limits)
	}
}

func TestComputerUseCoordinatePathHasNoLegacyScalingOrMouseEvent(t *testing.T) {
	source, err := os.ReadFile("computer_use.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"scaleXY(", "ScaleCoordinates(", "ClampCoordinates(", `"mouse_event"`} {
		if strings.Contains(string(source), forbidden) {
			t.Fatalf("computer_use coordinate path retains forbidden legacy mechanism %q", forbidden)
		}
	}
}

type computerUseCoordinateHarness struct {
	fake     *fakeAXCaller
	tool     *ComputerUseTool
	fixture  coordinateFinalizerFixture
	topology DisplayTopologyV1
	tree     computerUseTree
	artifact CoordinateWindowArtifactV1
	now      time.Time
}

func newComputerUseCoordinateHarness(t *testing.T) *computerUseCoordinateHarness {
	t.Helper()
	captureAt := time.Date(2026, 7, 22, 12, 3, 30, 0, time.UTC)
	fixture := newCoordinateFinalizerFixture(t, 16, 12, 1, coordinatePatternPixels, captureAt)
	tree := computerUseTree{
		SchemaVersion: 1,
		AppName:       "Fixture App",
		App:           "Fixture App",
		BundleID:      fixture.input.CaptureRequest.BundleID,
		PID:           fixture.input.CaptureRequest.PID,
		WindowTitle:   "Fixture Window",
		Window:        "Fixture Window",
		WindowID:      computerUseIntPointer(int(fixture.input.CaptureRequest.WindowID)),
		WindowFrame: &computerUseFrame{
			X:      fixture.input.CaptureRequest.ExpectedQuartzBounds.X,
			Y:      fixture.input.CaptureRequest.ExpectedQuartzBounds.Y,
			Width:  fixture.input.CaptureRequest.ExpectedQuartzBounds.Width,
			Height: fixture.input.CaptureRequest.ExpectedQuartzBounds.Height,
		},
		Elements: []computerUseElement{{
			Ref: "e1", Fingerprint: "axf_coordinate", Path: "window[0]/AXButton[0]",
			Role: "AXButton", Title: stringPointer("Target"), ValueRedacted: false,
			Enabled: boolPointer(true), Actions: []string{"AXPress"},
		}},
		RefPaths: map[string]computerUseRefPath{
			"e1": {Path: "window[0]/AXButton[0]", Role: "AXButton", Fingerprint: "axf_coordinate"},
		},
	}
	fake := newFakeAXCaller()
	now := captureAt.Add(time.Second)
	tool := &ComputerUseTool{
		client:            fake,
		coordinateNow:     func() time.Time { return now },
		coordinateFrameID: func() (string, error) { return "frame-computer-use-001", nil },
	}
	artifact, err := FinalizeCoordinateWindowV1(CoordinateWindowFinalizeInputV1{
		CapturePayload: fixture.input.CapturePayload, CaptureRequest: fixture.input.CaptureRequest,
		CurrentTopology: fixture.input.CurrentTopology, CaptureLimits: fixture.input.CaptureLimits,
		StateID: computerUseStateID(tree), Profile: defaultComputerUseCoordinateImageProfileV1(),
		Now: now, TTL: computerUseCoordinateFrameTTLV1, FrameID: "frame-computer-use-001",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &computerUseCoordinateHarness{
		fake: fake, tool: tool, fixture: fixture, topology: fixture.input.CurrentTopology,
		tree: tree, artifact: artifact, now: now,
	}
}

func (h *computerUseCoordinateHarness) queueObservation(before, after computerUseTree) {
	h.fake.queue("read_tree", marshalComputerUseTreeNoTest(before))
	h.fake.queue("display_topology", marshalDisplayTopologyNoTest(h.topology))
	h.fake.queue("capture_coordinate_window", string(h.fixture.input.CapturePayload))
	h.fake.queue("read_tree", marshalComputerUseTreeNoTest(after))
}

func (h *computerUseCoordinateHarness) observe(t *testing.T) {
	t.Helper()
	h.queueObservation(h.tree, h.tree)
	result, err := h.tool.Run(context.Background(), `{
		"action":"get_app_state","include_screenshot":true,"description":"Observe target"
	}`)
	if err != nil || result.IsError || h.tool.coordinateArtifact == nil {
		t.Fatalf("observe result=%+v err=%v", result, err)
	}
}

func cloneComputerUseTree(t *testing.T, tree computerUseTree) computerUseTree {
	t.Helper()
	decoded, err := decodeComputerUseTree([]byte(marshalComputerUseTree(t, tree)))
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func marshalComputerUseTree(t *testing.T, tree computerUseTree) string {
	t.Helper()
	return marshalComputerUseTreeNoTest(tree)
}

func marshalComputerUseTreeNoTest(tree computerUseTree) string {
	payload, err := json.Marshal(tree)
	if err != nil {
		panic(err)
	}
	return string(payload)
}

func marshalDisplayTopology(t *testing.T, topology DisplayTopologyV1) string {
	t.Helper()
	return marshalDisplayTopologyNoTest(topology)
}

func marshalDisplayTopologyNoTest(topology DisplayTopologyV1) string {
	payload, err := json.Marshal(topology)
	if err != nil {
		panic(err)
	}
	return string(payload)
}

func fakeAXMethods(calls []fakeAXCall) []string {
	methods := make([]string, 0, len(calls))
	for _, call := range calls {
		methods = append(methods, call.method)
	}
	return methods
}

func requireComputerUseDarwin(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("computer_use runtime is macOS-only")
	}
}

func stringPointer(value string) *string { return &value }

func boolPointer(value bool) *bool { return &value }

func computerUseIntPointer(value int) *int { return &value }
