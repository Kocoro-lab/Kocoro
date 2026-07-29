package tools

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

func TestComputerUseDragAndSelectTextAreProviderVisibleWithoutFrameIdentity(t *testing.T) {
	info := (&ComputerUseTool{}).Info()
	properties, ok := info.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatal("computer_use schema has no properties")
	}
	for _, name := range []string{
		"start_x", "start_y", "end_x", "end_y", "duration_ms", "range",
	} {
		if _, exists := properties[name]; !exists {
			t.Errorf("computer_use schema omitted %q", name)
		}
	}
	if _, leaked := properties["frame_id"]; leaked {
		t.Fatal("internal CoordinateFrame identity leaked into provider schema")
	}
	action, ok := properties["action"].(map[string]any)
	if !ok {
		t.Fatal("computer_use action schema is malformed")
	}
	description, _ := action["description"].(string)
	for _, actionName := range []string{"drag", "select_text"} {
		if !strings.Contains(description, actionName) {
			t.Errorf("provider action schema omitted %q: %s", actionName, description)
		}
		args := `{"action":"` + actionName + `"}`
		if (&ComputerUseTool{}).IsSafeArgs(args) || (&ComputerUseTool{}).IsReadOnlyCall(args) {
			t.Errorf("%s must be a mutation", actionName)
		}
	}
}

func TestComputerUseDragMapsBothCurrentFramePointsAndReturnsTypedPointer(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	harness.observe(t)
	stateID := harness.tool.snapshot.id
	frame := harness.tool.coordinateArtifact.Frame()
	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))

	start, err := MapCoordinatePixelCenterV1(
		frame,
		CoordinateTopologyRefV1{TopologyID: harness.topology.TopologyID, Generation: harness.topology.Generation},
		stateID, frame.FrameID, harness.now, 0, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	end, err := MapCoordinatePixelCenterV1(
		frame,
		CoordinateTopologyRefV1{TopologyID: harness.topology.TopologyID, Generation: harness.topology.Generation},
		stateID, frame.FrameID, harness.now, float64(frame.FinalImage.WidthPX-1), float64(frame.FinalImage.HeightPX-1),
	)
	if err != nil {
		t.Fatal(err)
	}

	var executed *CoordinateDragRequestV1
	harness.tool.coordinateDragExecutor = func(
		_ context.Context, request CoordinateDragRequestV1,
	) (CoordinateDragResultV1, error) {
		executed = &request
		endpoint := CoordinateMouseEventPointerEndpointV1{
			Requested: request.EndQuartzPoint, Observed: &request.EndQuartzPoint,
			Tolerance: coordinateMouseEndpointToleranceV1, Verified: true,
		}
		failureCode := "drop_postcondition_not_declared"
		return CoordinateDragResultV1{
			SchemaVersion: 1, Status: "completed_unverified", DragCommitted: true,
			MouseDownCommitted: true, PointerMotionCommitted: true, MouseUpCommitted: true,
			PossibleDropSideEffect: true, Phase: "post_verification", RetrySafe: false,
			FailureCode: &failureCode, PointerEndpoint: &endpoint,
		}, nil
	}

	result, err := harness.tool.Run(context.Background(), fmt.Sprintf(`{
		"action":"drag","state_id":%q,"start_x":0,"start_y":0,
		"end_x":%d,"end_y":%d,"duration_ms":420,"description":"Drag item"
	}`, stateID, frame.FinalImage.WidthPX-1, frame.FinalImage.HeightPX-1))
	if err != nil || result.IsError || executed == nil {
		t.Fatalf("drag result=%+v request=%+v err=%v", result, executed, err)
	}
	if executed.StartQuartzPoint != (CoordinateMouseEventPointV1{X: start.X, Y: start.Y}) ||
		executed.EndQuartzPoint != (CoordinateMouseEventPointV1{X: end.X, Y: end.Y}) ||
		executed.StartDisplayID != start.DisplayID || executed.EndDisplayID != end.DisplayID ||
		executed.PID != harness.tree.PID || executed.BundleID != harness.tree.BundleID ||
		executed.WindowID != uint32(*harness.tree.WindowID) ||
		executed.ExpectedWindowQuartzBounds != frame.CapturedQuartzRect ||
		executed.HelperBootID != harness.topology.HelperBootID ||
		executed.DurationMS != 420 || executed.Button != "left" ||
		executed.EndTargetPolicy != "same_window" {
		t.Fatalf("drag request lost exact authority: %+v", *executed)
	}
	if result.GUIOutcome == nil || result.GUIOutcome.Result != agent.GUIActionResultCompletedUnverified ||
		result.GUIOutcome.Phase != agent.GUIActionPhaseVerifying || result.GUIOutcome.Pointer == nil ||
		!result.GUIOutcome.SameObservationContinuationSafe ||
		result.GUIOutcome.Pointer.DisplayID != end.DisplayID ||
		result.GUIOutcome.Pointer.TopologyID != harness.topology.TopologyID ||
		result.GUIOutcome.Pointer.TopologyGeneration != harness.topology.Generation ||
		result.GUIOutcome.Pointer.X != end.X || result.GUIOutcome.Pointer.Y != end.Y {
		t.Fatalf("drag typed outcome lost pointer authority: %+v", result.GUIOutcome)
	}
	if harness.tool.snapshot != nil || harness.tool.refs != nil || harness.tool.coordinateArtifact != nil {
		t.Fatal("drag attempt did not consume state and CoordinateFrame")
	}
	if got := fakeAXMethods(harness.fake.calls[len(harness.fake.calls)-2:]); !reflect.DeepEqual(got, []string{"read_tree", "display_topology"}) {
		t.Fatalf("drag preflight sequence=%v", got)
	}
	if strings.Contains(result.Content, frame.FrameID) || strings.Contains(result.Content, "frame_id") {
		t.Fatalf("drag result leaked internal frame identity: %s", result.Content)
	}
}

func TestComputerUseDragMapsEveryProviderWaypointIntoStrictHelperAuthority(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	harness.observe(t)
	stateID := harness.tool.snapshot.id
	frame := harness.tool.coordinateArtifact.Frame()
	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))

	var executed *CoordinateDragRequestV1
	harness.tool.coordinateDragExecutor = func(
		_ context.Context, request CoordinateDragRequestV1,
	) (CoordinateDragResultV1, error) {
		executed = &request
		endpoint := request.EndQuartzPoint
		failureCode := "drop_postcondition_not_declared"
		return CoordinateDragResultV1{
			SchemaVersion: 1, Status: "completed_unverified", DragCommitted: true,
			MouseDownCommitted: true, PointerMotionCommitted: true, MouseUpCommitted: true,
			PossibleDropSideEffect: true, Phase: "post_verification", RetrySafe: false,
			FailureCode: &failureCode,
			PointerEndpoint: &CoordinateMouseEventPointerEndpointV1{
				Requested: endpoint, Observed: &endpoint,
				Tolerance: coordinateMouseEndpointToleranceV1, Verified: true,
			},
		}, nil
	}

	result, err := harness.tool.Run(context.Background(), fmt.Sprintf(`{
		"action":"drag","state_id":%q,
		"path":[{"x":0,"y":0},{"x":7,"y":5},{"x":15,"y":11}],
		"duration_ms":420,"description":"Drag along provider path"
	}`, stateID))
	if err != nil || result.IsError || executed == nil {
		t.Fatalf("polyline result=%+v request=%+v err=%v", result, executed, err)
	}
	if len(executed.Waypoints) != 3 {
		t.Fatalf("helper waypoints = %+v", executed.Waypoints)
	}
	for index, pixel := range []OpenAIComputerPointV1{
		{X: 0, Y: 0}, {X: 7, Y: 5}, {X: 15, Y: 11},
	} {
		mapped, mapErr := MapCoordinatePixelCenterV1(
			frame,
			CoordinateTopologyRefV1{
				TopologyID: harness.topology.TopologyID,
				Generation: harness.topology.Generation,
			},
			stateID, frame.FrameID, harness.now,
			float64(pixel.X), float64(pixel.Y),
		)
		if mapErr != nil {
			t.Fatal(mapErr)
		}
		if executed.Waypoints[index] != (CoordinateDragWaypointV1{
			DisplayID:   mapped.DisplayID,
			QuartzPoint: CoordinateMouseEventPointV1{X: mapped.X, Y: mapped.Y},
		}) {
			t.Fatalf("waypoint[%d]=%+v, want mapped %+v",
				index, executed.Waypoints[index], mapped)
		}
	}
}

func TestComputerUseDragFailureAndCommitUnknownConsumeState(t *testing.T) {
	requireComputerUseDarwin(t)
	for _, test := range []struct {
		name string
		run  func(*computerUseCoordinateHarness, string) agent.ToolResult
		want string
	}{
		{
			name: "missing end coordinate",
			run: func(h *computerUseCoordinateHarness, stateID string) agent.ToolResult {
				result, _ := h.tool.Run(context.Background(), fmt.Sprintf(
					`{"action":"drag","state_id":%q,"start_x":0,"start_y":0,"end_x":1,"description":"Drag"}`,
					stateID))
				return result
			},
			want: "start_x+start_y+end_x+end_y",
		},
		{
			name: "commit unknown",
			run: func(h *computerUseCoordinateHarness, stateID string) agent.ToolResult {
				h.fake.queue("read_tree", marshalComputerUseTree(t, h.tree))
				h.fake.queue("display_topology", marshalDisplayTopology(t, h.topology))
				h.tool.coordinateDragExecutor = func(context.Context, CoordinateDragRequestV1) (CoordinateDragResultV1, error) {
					return CoordinateDragResultV1{}, &CoordinateDragCommitUnknownErrorV1{cause: errors.New("EOF")}
				}
				result, _ := h.tool.Run(context.Background(), fmt.Sprintf(
					`{"action":"drag","state_id":%q,"start_x":0,"start_y":0,"end_x":1,"end_y":1,"description":"Drag"}`,
					stateID))
				return result
			},
			want: "commit status is unknown",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newComputerUseCoordinateHarness(t)
			harness.observe(t)
			result := test.run(harness, harness.tool.snapshot.id)
			if !result.IsError || !strings.Contains(result.Content, test.want) {
				t.Fatalf("result=%+v, want %q", result, test.want)
			}
			if harness.tool.snapshot != nil || harness.tool.refs != nil || harness.tool.coordinateArtifact != nil {
				t.Fatal("failed drag attempt retained state")
			}
		})
	}
}

func TestComputerUseDragUserInterferenceReturnsTypedCleanupOutcome(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	harness.observe(t)
	stateID := harness.tool.snapshot.id
	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))
	harness.tool.coordinateDragExecutor = func(
		_ context.Context, request CoordinateDragRequestV1,
	) (CoordinateDragResultV1, error) {
		observed := request.EndQuartzPoint
		observed.X--
		endpoint := CoordinateMouseEventPointerEndpointV1{
			Requested: request.EndQuartzPoint, Observed: &observed,
			Tolerance: coordinateMouseEndpointToleranceV1, Verified: true,
		}
		failureCode := "pointer_interference"
		return CoordinateDragResultV1{
			SchemaVersion: 1, Status: "user_interference", DragCommitted: true,
			MouseDownCommitted: true, PointerMotionCommitted: true, MouseUpCommitted: true,
			PossibleDropSideEffect: true, Phase: "cleanup", FailureCode: &failureCode,
			RetrySafe: false, PointerEndpoint: &endpoint,
		}, nil
	}
	result, err := harness.tool.Run(context.Background(), fmt.Sprintf(`{
		"action":"drag","state_id":%q,"start_x":0,"start_y":0,
		"end_x":1,"end_y":1,"description":"Drag item"
	}`, stateID))
	if err != nil || !result.IsError || !strings.Contains(result.Content, "user interference") {
		t.Fatalf("user interference result=%+v err=%v", result, err)
	}
	if result.GUIOutcome == nil || result.GUIOutcome.Result != agent.GUIActionResultUserInterference ||
		result.GUIOutcome.Phase != agent.GUIActionPhaseInputCommitted ||
		result.GUIOutcome.FailureCode != "pointer_interference" || result.GUIOutcome.Pointer == nil {
		t.Fatalf("cleanup outcome=%+v", result.GUIOutcome)
	}
	if harness.tool.snapshot != nil || harness.tool.refs != nil || harness.tool.coordinateArtifact != nil {
		t.Fatal("user-interference drag retained state")
	}
}

func TestComputerUseDragRejectsFrameWithoutDurationAndCleanupBudget(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	harness.observe(t)
	stateID := harness.tool.snapshot.id
	expiresAt, err := time.Parse(time.RFC3339Nano, harness.tool.coordinateArtifact.Frame().ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	nearExpiry := expiresAt.Add(-600 * time.Millisecond)
	harness.tool.coordinateNow = func() time.Time { return nearExpiry }
	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))
	called := false
	harness.tool.coordinateDragExecutor = func(
		context.Context, CoordinateDragRequestV1,
	) (CoordinateDragResultV1, error) {
		called = true
		return CoordinateDragResultV1{}, nil
	}
	result, err := harness.tool.Run(context.Background(), fmt.Sprintf(`{
		"action":"drag","state_id":%q,"start_x":0,"start_y":0,
		"end_x":1,"end_y":1,"duration_ms":350,"description":"Drag item"
	}`, stateID))
	if err != nil || !result.IsError || !strings.Contains(result.Content, "expires too soon") {
		t.Fatalf("short frame budget result=%+v err=%v", result, err)
	}
	if called {
		t.Fatal("drag reached helper without enough duration + cleanup budget")
	}
	if harness.tool.snapshot != nil || harness.tool.refs != nil || harness.tool.coordinateArtifact != nil {
		t.Fatal("short-budget drag retained state")
	}
}

func TestComputerUseSelectTextUsesAXRangeFirstAndReturnsVerifiedOutcome(t *testing.T) {
	requireComputerUseDarwin(t)
	windowID := 7001
	tool, fake, stateID := semanticPressTestTool(t, &windowID)
	tool.snapshot.bundleID = "com.apple.Notes"
	tool.snapshot.app = "Notes"

	var executed *SemanticTextSelectionRequestV2
	tool.semanticTextSelectionExecutor = func(
		_ context.Context, request SemanticTextSelectionRequestV2,
	) (SemanticTextSelectionResultV2, error) {
		executed = &request
		postcondition := "selected_range_matches"
		selected := request.Range
		return SemanticTextSelectionResultV2{
			SchemaVersion: 2, Status: "verified", CommitState: "committed",
			Phase: "post_verification", RetrySafe: false, Postcondition: &postcondition,
			SelectedRange: &selected,
		}, nil
	}

	result, err := tool.Run(context.Background(), fmt.Sprintf(`{
		"action":"select_text","state_id":%q,"ref":"e1",
		"range":{"location":3,"length":5},"description":"Select text"
	}`, stateID))
	if err != nil || result.IsError || executed == nil {
		t.Fatalf("select_text result=%+v request=%+v err=%v", result, executed, err)
	}
	if executed.PID != 42 || executed.BundleID != "com.apple.Notes" || executed.WindowID != 7001 ||
		executed.Ref != "e1" || executed.Path != "window[0]/AXButton[0]" ||
		executed.ExpectedRole != "AXButton" || executed.ExpectedFingerprint != "axf_target" ||
		executed.Range != (SemanticTextRangeV2{Location: 3, Length: 5}) ||
		executed.FallbackPolicy != "report_unsupported" {
		t.Fatalf("semantic selection request lost AX authority: %+v", *executed)
	}
	if result.GUIOutcome == nil || result.GUIOutcome.Result != agent.GUIActionResultVerified ||
		result.GUIOutcome.Phase != agent.GUIActionPhaseVerifying || result.GUIOutcome.Pointer != nil {
		t.Fatalf("semantic selection typed outcome=%+v", result.GUIOutcome)
	}
	if got := fakeAXMethods(fake.calls); !reflect.DeepEqual(got, []string{"read_tree"}) {
		t.Fatalf("select_text must remain AX-first, calls=%v", got)
	}
	if tool.snapshot != nil || tool.refs != nil || tool.coordinateArtifact != nil {
		t.Fatal("select_text did not consume state")
	}
}

func TestComputerUseSelectTextFallbackRequiredIsExplicitAndNeverCoordinates(t *testing.T) {
	requireComputerUseDarwin(t)
	windowID := 7001
	tool, fake, stateID := semanticPressTestTool(t, &windowID)
	tool.snapshot.bundleID = "com.tinyspeck.slackmacgap"
	tool.snapshot.app = "Slack"
	tool.semanticTextSelectionExecutor = func(
		_ context.Context, _ SemanticTextSelectionRequestV2,
	) (SemanticTextSelectionResultV2, error) {
		failureCode := "ax_text_range_unsupported"
		return SemanticTextSelectionResultV2{
			SchemaVersion: 2, Status: "fallback_required", CommitState: "not_committed",
			Phase: "preflight", FailureCode: &failureCode, RetrySafe: false,
		}, nil
	}

	result, err := tool.Run(context.Background(), fmt.Sprintf(`{
		"action":"select_text","state_id":%q,"ref":"e1",
		"range":{"location":0,"length":4},"description":"Select Electron text"
	}`, stateID))
	if err != nil || !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness ||
		!strings.Contains(result.Content, "fallback_required") ||
		!strings.Contains(result.Content, "coordinate drag") {
		t.Fatalf("fallback_required result=%+v err=%v", result, err)
	}
	if result.GUIOutcome == nil || result.GUIOutcome.Result != agent.GUIActionResultFailed ||
		result.GUIOutcome.Phase != agent.GUIActionPhaseObserving ||
		result.GUIOutcome.FailureCode != "ax_text_range_unsupported" {
		t.Fatalf("fallback_required typed outcome=%+v", result.GUIOutcome)
	}
	if got := fakeAXMethods(fake.calls); !reflect.DeepEqual(got, []string{"read_tree"}) {
		t.Fatalf("fallback_required secretly delegated to coordinate path: %v", got)
	}
	if tool.snapshot != nil || tool.refs != nil || tool.coordinateArtifact != nil {
		t.Fatal("fallback_required retained state")
	}
}

func TestComputerUseSelectTextCompletedUnverifiedReturnsTypedFailureCode(t *testing.T) {
	requireComputerUseDarwin(t)
	windowID := 7001
	tool, _, stateID := semanticPressTestTool(t, &windowID)
	tool.snapshot.bundleID = "com.apple.Notes"
	tool.semanticTextSelectionExecutor = func(
		_ context.Context, _ SemanticTextSelectionRequestV2,
	) (SemanticTextSelectionResultV2, error) {
		failureCode := "selected_range_not_observed"
		return SemanticTextSelectionResultV2{
			SchemaVersion: 2, Status: "completed_unverified", CommitState: "committed",
			Phase: "post_verification", FailureCode: &failureCode, RetrySafe: false,
		}, nil
	}
	result, err := tool.Run(context.Background(), fmt.Sprintf(`{
		"action":"select_text","state_id":%q,"ref":"e1",
		"range":{"location":1,"length":2},"description":"Select text"
	}`, stateID))
	if err != nil || result.IsError || !strings.Contains(result.Content, "completed_unverified") {
		t.Fatalf("completed_unverified result=%+v err=%v", result, err)
	}
	if result.GUIOutcome == nil ||
		result.GUIOutcome.Result != agent.GUIActionResultCompletedUnverified ||
		result.GUIOutcome.Phase != agent.GUIActionPhaseVerifying ||
		result.GUIOutcome.FailureCode != "selected_range_not_observed" {
		t.Fatalf("semantic completed_unverified outcome=%+v", result.GUIOutcome)
	}
	if tool.snapshot != nil || tool.refs != nil || tool.coordinateArtifact != nil {
		t.Fatal("completed_unverified selection retained state")
	}
}

func TestComputerUseSelectTextMapsPhysicalInterferenceWithoutLeakingSelectionAuthority(t *testing.T) {
	requireComputerUseDarwin(t)
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed_%t", committed), func(t *testing.T) {
			windowID := 7001
			tool, _, stateID := semanticPressTestTool(t, &windowID)
			tool.snapshot.bundleID = "com.apple.Notes"
			tool.semanticTextSelectionExecutor = func(
				_ context.Context, _ SemanticTextSelectionRequestV2,
			) (SemanticTextSelectionResultV2, error) {
				failureCode := "physical_input_interference"
				commitState := "not_committed"
				if committed {
					commitState = "committed"
				}
				return SemanticTextSelectionResultV2{
					SchemaVersion: 2, Status: "user_interference",
					CommitState: commitState, Phase: "user_interference",
					FailureCode: &failureCode, RetrySafe: false,
				}, nil
			}
			const secretDescription = "never-echo-selection-description-9927"
			result, err := tool.Run(context.Background(), fmt.Sprintf(`{
				"action":"select_text","state_id":%q,"ref":"e1",
				"range":{"location":17,"length":23},"description":%q
			}`, stateID, secretDescription))
			if err != nil || !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness {
				t.Fatalf("user_interference result=%+v err=%v", result, err)
			}
			wantPhase := agent.GUIActionPhaseActing
			if committed {
				wantPhase = agent.GUIActionPhaseInputCommitted
			}
			if result.GUIOutcome == nil ||
				result.GUIOutcome.Result != agent.GUIActionResultUserInterference ||
				result.GUIOutcome.Phase != wantPhase ||
				result.GUIOutcome.FailureCode != "physical_input_interference" {
				t.Fatalf("user_interference GUI outcome=%+v", result.GUIOutcome)
			}
			for _, sensitive := range []string{
				secretDescription, stateID, "e1", "axf_target", "17", "23",
			} {
				if strings.Contains(result.Content, sensitive) {
					t.Fatalf("select_text result leaked %q: %s", sensitive, result.Content)
				}
			}
			if !strings.Contains(result.Content, "do not retry automatically") ||
				!strings.Contains(result.Content, "re-observe") {
				t.Fatalf("interference result invited unsafe retry: %s", result.Content)
			}
		})
	}
}

func TestComputerUseSelectTextMapsInterferenceMonitorLossByCommitBoundary(t *testing.T) {
	requireComputerUseDarwin(t)
	for _, test := range []struct {
		name      string
		status    string
		committed bool
		phase     string
		want      agent.GUIActionResult
		wantError bool
	}{
		{
			name: "precommit_failed", status: "failed", committed: false,
			phase: "preflight", want: agent.GUIActionResultFailed, wantError: true,
		},
		{
			name: "postcommit_unverified", status: "completed_unverified", committed: true,
			phase: "post_verification", want: agent.GUIActionResultCompletedUnverified,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			windowID := 7001
			tool, _, stateID := semanticPressTestTool(t, &windowID)
			tool.snapshot.bundleID = "com.apple.Notes"
			tool.semanticTextSelectionExecutor = func(
				_ context.Context, _ SemanticTextSelectionRequestV2,
			) (SemanticTextSelectionResultV2, error) {
				failureCode := "interference_detection_unavailable"
				commitState := "not_committed"
				if test.committed {
					commitState = "committed"
				}
				return SemanticTextSelectionResultV2{
					SchemaVersion: 2, Status: test.status,
					CommitState: commitState, Phase: test.phase,
					FailureCode: &failureCode, RetrySafe: false,
				}, nil
			}
			result, err := tool.Run(context.Background(), fmt.Sprintf(`{
				"action":"select_text","state_id":%q,"ref":"e1",
				"range":{"location":1,"length":2},"description":"Select text"
			}`, stateID))
			if err != nil || result.IsError != test.wantError {
				t.Fatalf("monitor loss result=%+v err=%v", result, err)
			}
			if result.GUIOutcome == nil || result.GUIOutcome.Result != test.want ||
				result.GUIOutcome.FailureCode != "interference_detection_unavailable" {
				t.Fatalf("monitor loss GUI outcome=%+v", result.GUIOutcome)
			}
			if !strings.Contains(result.Content, "do not retry automatically") ||
				!strings.Contains(result.Content, "re-observe") {
				t.Fatalf("monitor loss result invited unsafe retry: %s", result.Content)
			}
		})
	}
}

func TestComputerUseSelectTextCommitUnknownUsesInputCommittedAndNeverInvitesRetry(t *testing.T) {
	requireComputerUseDarwin(t)
	for _, test := range []struct {
		name string
		run  func(context.Context, SemanticTextSelectionRequestV2) (SemanticTextSelectionResultV2, error)
	}{
		{
			name: "typed acknowledgement",
			run: func(context.Context, SemanticTextSelectionRequestV2) (SemanticTextSelectionResultV2, error) {
				failureCode := "ax_selection_commit_unknown"
				return SemanticTextSelectionResultV2{
					SchemaVersion: 2, Status: "completed_unverified", CommitState: "unknown",
					Phase: "action", FailureCode: &failureCode, RetrySafe: false,
				}, nil
			},
		},
		{
			name: "transport acknowledgement loss",
			run: func(context.Context, SemanticTextSelectionRequestV2) (SemanticTextSelectionResultV2, error) {
				return SemanticTextSelectionResultV2{},
					newSemanticTextSelectionCommitUnknownV2(errors.New("EOF"))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			windowID := 7001
			tool, _, stateID := semanticPressTestTool(t, &windowID)
			tool.snapshot.bundleID = "com.apple.Notes"
			tool.semanticTextSelectionExecutor = test.run
			result, err := tool.Run(context.Background(), fmt.Sprintf(`{
				"action":"select_text","state_id":%q,"ref":"e1",
				"range":{"location":1,"length":2},"description":"Select text"
			}`, stateID))
			if err != nil || result.GUIOutcome == nil ||
				result.GUIOutcome.Result != agent.GUIActionResultCompletedUnverified ||
				result.GUIOutcome.Phase != agent.GUIActionPhaseInputCommitted {
				t.Fatalf("commit unknown result=%+v err=%v", result, err)
			}
			if !strings.Contains(result.Content, "do not retry automatically") ||
				!strings.Contains(result.Content, "re-observe") {
				t.Fatalf("commit unknown invited unsafe retry: %s", result.Content)
			}
		})
	}
}

func TestComputerUseSelectTextValidatesRangeBeforeMutationAndConsumesState(t *testing.T) {
	requireComputerUseDarwin(t)
	windowID := 7001
	tool, fake, stateID := semanticPressTestTool(t, &windowID)
	tool.snapshot.bundleID = "com.apple.Notes"
	called := false
	tool.semanticTextSelectionExecutor = func(context.Context, SemanticTextSelectionRequestV2) (SemanticTextSelectionResultV2, error) {
		called = true
		return SemanticTextSelectionResultV2{}, nil
	}
	result, err := tool.Run(context.Background(), fmt.Sprintf(`{
		"action":"select_text","state_id":%q,"ref":"e1",
		"range":{"location":0,"length":0},"description":"Select text"
	}`, stateID))
	if err != nil || !result.IsError || !strings.Contains(result.Content, "range") {
		t.Fatalf("invalid range result=%+v err=%v", result, err)
	}
	if called || len(fake.calls) != 0 {
		t.Fatalf("invalid range reached mutation: called=%v calls=%+v", called, fake.calls)
	}
	if tool.snapshot != nil || tool.refs != nil || tool.coordinateArtifact != nil {
		t.Fatal("invalid select_text attempt retained state")
	}
}

func TestComputerUseDragAndSelectTextDescriptorsKeepExactObservedBundle(t *testing.T) {
	for _, test := range []struct {
		action string
		path   string
	}{
		{action: "drag", path: "synthetic_coordinate"},
		{action: "select_text", path: "accessibility"},
	} {
		t.Run(test.action, func(t *testing.T) {
			tool := &ComputerUseTool{snapshot: &computerUseSnapshot{
				id: "s_state", app: "Notes", bundleID: "com.apple.Notes", pid: 42,
			}}
			descriptor, err := tool.DescribeGUIAction(
				context.Background(), `{"action":"`+test.action+`","state_id":"s_state","description":"act"}`)
			if err != nil {
				t.Fatal(err)
			}
			if descriptor.Effect != agent.GUIActionMutation || descriptor.ActionKind != test.action ||
				descriptor.ExecutionPath != test.path || descriptor.TargetBundleID != "com.apple.Notes" ||
				descriptor.TargetAppName != "Notes" {
				t.Fatalf("descriptor=%+v", descriptor)
			}
		})
	}
}

func TestComputerUseDragDefaultDurationFitsMutationDeadline(t *testing.T) {
	if computerUseDefaultDragDurationV1 < 120 || computerUseDefaultDragDurationV1 > 800 {
		t.Fatalf("unsafe default drag duration %d", computerUseDefaultDragDurationV1)
	}
	if time.Duration(computerUseDefaultDragDurationV1)*time.Millisecond+
		computerUseDragDeadlineOverheadV1 >= computerUseMutationDeadlineV1 {
		t.Fatalf("drag duration %d plus cleanup does not fit mutation deadline %s",
			computerUseDefaultDragDurationV1, computerUseMutationDeadlineV1)
	}
}
