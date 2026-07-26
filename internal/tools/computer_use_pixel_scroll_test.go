package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

func TestComputerUsePixelScrollMapsExactFramePointAndPreservesProviderDeltas(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	harness.observe(t)
	stateID := harness.tool.snapshot.id
	frame := harness.tool.coordinateArtifact.Frame()
	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))
	var executed *CoordinatePixelScrollRequestV1
	harness.tool.coordinatePixelScrollExecutor = func(
		_ context.Context, request CoordinatePixelScrollRequestV1,
	) (CoordinatePixelScrollResultV1, error) {
		executed = &request
		ack := coordinatePixelScrollAcknowledgementV1(request)
		point := request.QuartzPoint
		code := "scroll_postcondition_not_declared"
		return CoordinatePixelScrollResultV1{
			SchemaVersion: 1, Status: "committed_unverified",
			PointerMoveCommitState: "committed", ScrollCommitState: "committed",
			Phase: "post_verification", FailureCode: &code, RetrySafe: false,
			Requested: &ack,
			PointerEndpoint: &CoordinateMouseEventPointerEndpointV1{
				Requested: point, Observed: &point,
				Tolerance: coordinateMouseEndpointToleranceV1, Verified: true,
			},
		}, nil
	}
	result, err := harness.tool.Run(ContextWithOpenAINativeComputerActionV1(
		context.Background()), fmt.Sprintf(`{
		"action":"pixel_scroll","state_id":%q,"x":7,"y":8,
		"scroll_x":37,"scroll_y":-618,"description":"OpenAI native scroll"
	}`, stateID))
	if err != nil || result.IsError || executed == nil {
		t.Fatalf("result=%+v request=%+v err=%v", result, executed, err)
	}
	mapped, err := MapCoordinatePixelCenterV1(
		frame,
		CoordinateTopologyRefV1{
			TopologyID: harness.topology.TopologyID,
			Generation: harness.topology.Generation,
		},
		stateID, frame.FrameID, harness.now, 7, 8)
	if err != nil {
		t.Fatal(err)
	}
	if executed.ProviderDeltaX != 37 || executed.ProviderDeltaY != -618 ||
		executed.Unit != "pixel" || executed.TargetPolicy != "same_window" ||
		executed.QuartzPoint != (CoordinateMouseEventPointV1{X: mapped.X, Y: mapped.Y}) ||
		executed.DisplayID != mapped.DisplayID ||
		executed.PID != harness.tree.PID ||
		executed.BundleID != harness.tree.BundleID ||
		executed.WindowID != uint32(*harness.tree.WindowID) ||
		executed.ExpectedWindowQuartzBounds != frame.CapturedQuartzRect {
		t.Fatalf("strict pixel scroll request lost authority: %+v", *executed)
	}
	affine := frame.TransformRegions[0].Affine
	wantAxis1, wantAxis2, ok := coordinatePixelScrollCGDeltasV1(
		37, -618, affine.A, affine.D)
	if !ok ||
		executed.ProviderToQuartzScaleX != affine.A ||
		executed.ProviderToQuartzScaleY != affine.D ||
		executed.CGPointDeltaAxis1 != wantAxis1 ||
		executed.CGPointDeltaAxis2 != wantAxis2 {
		t.Fatalf("pixel delta transform was not bound to frame: %+v frame=%+v",
			*executed, affine)
	}
	if result.GUIOutcome == nil ||
		result.GUIOutcome.Result != agent.GUIActionResultCompletedUnverified ||
		result.GUIOutcome.Phase != agent.GUIActionPhaseVerifying ||
		result.GUIOutcome.Pointer == nil {
		t.Fatalf("typed outcome=%+v", result.GUIOutcome)
	}
	if harness.tool.snapshot == nil || harness.tool.coordinateArtifact == nil {
		t.Fatal("native pixel scroll consumed ordered-batch observation authority")
	}
	if got := fakeAXMethods(harness.fake.calls[len(harness.fake.calls)-2:]); !reflect.DeepEqual(
		got, []string{"read_tree", "display_topology"}) {
		t.Fatalf("preflight sequence=%v", got)
	}
}

func TestComputerUsePixelScrollDescriptorKeepsExactSyntheticTargetAuthority(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	harness.observe(t)
	stateID := harness.tool.snapshot.id
	descriptor, err := harness.tool.DescribeGUIAction(
		ContextWithOpenAINativeComputerActionV1(context.Background()), fmt.Sprintf(`{
			"action":"pixel_scroll","state_id":%q,"x":7,"y":8,
			"scroll_x":37,"scroll_y":-618,"description":"native scroll"
		}`, stateID))
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.ExecutionPath != "synthetic_coordinate" ||
		descriptor.TargetBundleID != harness.tree.BundleID ||
		descriptor.Effect != agent.GUIActionMutation {
		t.Fatalf("descriptor dropped exact target authority: %+v", descriptor)
	}
}

func TestComputerUsePixelScrollRejectsTinyFrameShearBeforeHelperCommit(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	harness.observe(t)
	stateID := harness.tool.snapshot.id
	harness.tool.coordinateArtifact.frame.TransformRegions[0].Affine.B = 1e-12
	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))
	called := false
	harness.tool.coordinatePixelScrollExecutor = func(
		context.Context, CoordinatePixelScrollRequestV1,
	) (CoordinatePixelScrollResultV1, error) {
		called = true
		return CoordinatePixelScrollResultV1{}, nil
	}
	result, err := harness.tool.Run(ContextWithOpenAINativeComputerActionV1(
		context.Background()), fmt.Sprintf(`{
		"action":"pixel_scroll","state_id":%q,"x":7,"y":8,
		"scroll_x":37,"scroll_y":-618,"description":"native scroll"
	}`, stateID))
	if err != nil || !result.IsError ||
		!strings.Contains(result.Content, "axis-aligned") || called {
		t.Fatalf("sheared frame result=%+v called=%v err=%v", result, called, err)
	}
}

func TestComputerUsePixelScrollMoveCommittedScrollNotCommittedIsTerminalUnverified(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	harness.observe(t)
	stateID := harness.tool.snapshot.id
	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	harness.fake.queue("display_topology", marshalDisplayTopology(t, harness.topology))
	harness.tool.coordinatePixelScrollExecutor = func(
		_ context.Context, request CoordinatePixelScrollRequestV1,
	) (CoordinatePixelScrollResultV1, error) {
		ack := coordinatePixelScrollAcknowledgementV1(request)
		point := request.QuartzPoint
		code := "scroll_not_committed"
		return CoordinatePixelScrollResultV1{
			SchemaVersion: 1, Status: "committed_unverified",
			PointerMoveCommitState: "committed",
			ScrollCommitState:      "not_committed",
			Phase:                  "between_commits", FailureCode: &code,
			Requested: &ack,
			PointerEndpoint: &CoordinateMouseEventPointerEndpointV1{
				Requested: point, Observed: &point,
				Tolerance: coordinateMouseEndpointToleranceV1, Verified: true,
			},
		}, nil
	}
	result, err := harness.tool.Run(ContextWithOpenAINativeComputerActionV1(
		context.Background()), fmt.Sprintf(`{
		"action":"pixel_scroll","state_id":%q,"x":7,"y":8,
		"scroll_x":37,"scroll_y":-618,"description":"native scroll"
	}`, stateID))
	if err != nil || result.IsError || result.GUIOutcome == nil ||
		result.GUIOutcome.Result != agent.GUIActionResultCompletedUnverified ||
		result.GUIOutcome.Phase != agent.GUIActionPhaseInputCommitted ||
		result.GUIOutcome.FailureCode != "scroll_not_committed" {
		t.Fatalf("partial commit was flattened: result=%+v err=%v", result, err)
	}
}

func TestComputerUsePixelScrollIsNotCallableFromGenericToolRoute(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	args := `{"action":"pixel_scroll","description":"generic route"}`
	result, err := harness.tool.Run(context.Background(), args)
	if err != nil || !result.IsError ||
		!strings.Contains(result.Content, "restricted to an admitted OpenAI") {
		t.Fatalf("generic run result=%+v err=%v", result, err)
	}
	if _, err := harness.tool.DescribeGUIAction(context.Background(), args); err == nil ||
		!strings.Contains(err.Error(), "restricted to an admitted OpenAI") {
		t.Fatalf("generic descriptor err=%v", err)
	}
	if len(harness.fake.calls) != 0 {
		t.Fatalf("generic route reached AX: %+v", harness.fake.calls)
	}
}

func TestOpenAIComputerActionRuntimeProjectsExactPixelScrollUnion(t *testing.T) {
	harness, runtime := guardedOpenAIComputerRuntimeHarness(t)
	harness.fake.queue("display_topology", marshalDisplayTopologyNoTest(harness.topology))
	x, y, scrollX, scrollY := 7, 8, 37, -618
	plan, err := runtime.PlanOpenAIComputerActionV1(
		context.Background(),
		OpenAIComputerActionV1{
			Type: OpenAIComputerActionScrollV1,
			X:    &x, Y: &y, ScrollX: &scrollX, ScrollY: &scrollY,
		},
	)
	if err != nil {
		t.Fatalf("PlanOpenAIComputerActionV1: %v", err)
	}
	var args computerUseArgs
	if err := json.Unmarshal([]byte(plan.Args), &args); err != nil {
		t.Fatal(err)
	}
	if !plan.Mutation || args.Action != "pixel_scroll" ||
		args.X == nil || int(*args.X) != x ||
		args.Y == nil || int(*args.Y) != y ||
		args.ScrollX == nil || int(*args.ScrollX) != scrollX ||
		args.ScrollY == nil || int(*args.ScrollY) != scrollY {
		t.Fatalf("projected args=%+v plan=%+v", args, plan)
	}
}
