package tools

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func guardedOpenAIComputerRuntimeHarness(
	t *testing.T,
) (*computerUseCoordinateHarness, *OpenAIComputerActionRuntimeV1) {
	t.Helper()
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	harness.observe(t)
	public := wrapGUIExecutionGate(harness.tool)
	runtime, err := NewOpenAIComputerActionRuntimeV1(public)
	if err != nil {
		t.Fatalf("NewOpenAIComputerActionRuntimeV1: %v", err)
	}
	if runtime.public != public || runtime.raw != harness.tool {
		t.Fatal("runtime did not retain the guarded clone-local ComputerUseTool")
	}
	return harness, runtime
}

func TestOpenAIComputerActionRuntimeRequiresFinalGUIExecutionGate(t *testing.T) {
	if runtime, err := NewOpenAIComputerActionRuntimeV1(&ComputerUseTool{}); err == nil ||
		runtime != nil {
		t.Fatalf("raw ComputerUseTool accepted: runtime=%v err=%v", runtime, err)
	}
}

func TestOpenAIComputerTaskAppsSupersedeForegroundHintAndRefreshFrontmost(
	t *testing.T,
) {
	fake := newFakeAXCaller()
	fake.queue("launch_app", `{"result":"launched"}`)
	fake.queue("launch_app", `{"result":"launched"}`)
	fake.queue("focus", `{"result":"focused"}`)
	raw := &ComputerUseTool{
		client: fake,
		initialTarget: &ComputerUseInitialTargetV1{
			PID: 9, AppName: "Kocoro Desktop",
			BundleID: "run.shannon.shanclaw.dev",
		},
		snapshot: &computerUseSnapshot{id: "old"},
	}
	runtime := &OpenAIComputerActionRuntimeV1{raw: raw}

	err := runtime.LaunchAndFocusTaskAppsV1(
		context.Background(),
		[]OpenAIComputerTaskAppV1{
			{App: "Slack", BundleID: "com.tinyspeck.slackmacgap"},
			{App: "Calculator", BundleID: "com.apple.calculator"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if raw.initialTarget != nil || raw.snapshot != nil {
		t.Fatalf("stale foreground/bootstrap state survived: %+v / %+v",
			raw.initialTarget, raw.snapshot)
	}
	if len(fake.calls) != 3 ||
		fake.calls[0].method != "launch_app" ||
		fake.calls[1].method != "launch_app" ||
		fake.calls[2].method != "focus" {
		t.Fatalf("app preparation calls = %+v", fake.calls)
	}
}

func TestOpenAIComputerActionRuntimeProjectsFramedClickWithoutLeakingAuthority(t *testing.T) {
	harness, runtime := guardedOpenAIComputerRuntimeHarness(t)
	harness.fake.queue(
		"display_topology",
		marshalDisplayTopologyNoTest(harness.topology),
	)
	x, y := 4, 5
	plan, err := runtime.PlanOpenAIComputerActionV1(
		context.Background(),
		OpenAIComputerActionV1{
			Type: OpenAIComputerActionClickV1, Button: "left", X: &x, Y: &y,
		},
	)
	if err != nil {
		t.Fatalf("PlanOpenAIComputerActionV1: %v", err)
	}
	if !plan.Mutation || plan.Tool != runtime.public {
		t.Fatalf("click plan = %+v", plan)
	}
	var args computerUseArgs
	if err := json.Unmarshal([]byte(plan.Args), &args); err != nil {
		t.Fatal(err)
	}
	if args.Action != "click" || args.StateID != harness.tool.snapshot.id ||
		args.X == nil || int(*args.X) != x ||
		args.Y == nil || int(*args.Y) != y {
		t.Fatalf("translated click args = %+v", args)
	}
	for _, forbidden := range []string{
		"frame_id", "topology_id", "image_sha", "target_digest",
	} {
		if strings.Contains(plan.Args, forbidden) {
			t.Fatalf("internal authority %q leaked into plan: %s", forbidden, plan.Args)
		}
	}
}

func TestOpenAIComputerActionRuntimeKeepsAXHitOnVisibleCoordinatePath(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	harness.tree.Elements[0].Frame = harness.tree.WindowFrame
	harness.observe(t)
	harness.fake.queue(
		"display_topology",
		marshalDisplayTopologyNoTest(harness.topology),
	)
	runtime, err := NewOpenAIComputerActionRuntimeV1(
		wrapGUIExecutionGate(harness.tool),
	)
	if err != nil {
		t.Fatal(err)
	}
	x, y := 4, 5
	plan, err := runtime.PlanOpenAIComputerActionV1(
		context.Background(),
		OpenAIComputerActionV1{
			Type: OpenAIComputerActionClickV1, Button: "left", X: &x, Y: &y,
		},
	)
	if err != nil {
		t.Fatalf("PlanOpenAIComputerActionV1: %v", err)
	}
	var args computerUseArgs
	if err := json.Unmarshal([]byte(plan.Args), &args); err != nil {
		t.Fatal(err)
	}
	if args.Action != "click" || args.Ref != "" ||
		args.X == nil || int(*args.X) != x ||
		args.Y == nil || int(*args.Y) != y {
		t.Fatalf("AX-hit click did not remain on visible coordinate path: %+v", args)
	}
}

func TestOpenAIComputerActionRuntimeUsesUniqueTrustedFocusedAXRefForType(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	harness.tree.Elements[0].Focused = true
	focused := harness.tree.Elements[0].Ref
	harness.tree.FocusedRef = &focused
	harness.observe(t)
	runtime, err := NewOpenAIComputerActionRuntimeV1(
		wrapGUIExecutionGate(harness.tool),
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := runtime.PlanOpenAIComputerActionV1(
		context.Background(),
		OpenAIComputerActionV1{
			Type: OpenAIComputerActionTypeTextV1,
			Text: "private text",
		},
	)
	if err != nil {
		t.Fatalf("PlanOpenAIComputerActionV1: %v", err)
	}
	var args computerUseArgs
	if err := json.Unmarshal([]byte(plan.Args), &args); err != nil {
		t.Fatal(err)
	}
	if !plan.Mutation || args.Action != "type" || args.Ref != focused ||
		args.Text == nil || *args.Text != "private text" ||
		args.StateID != harness.tool.snapshot.id {
		t.Fatalf("translated type plan = %+v / %+v", plan, args)
	}
}

func TestOpenAIComputerActionRuntimeUsesVerifiedCoordinateFocusWithoutAXRefForType(
	t *testing.T,
) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	harness.tree.Elements = nil
	harness.tree.RefPaths = map[string]computerUseRefPath{}
	harness.observe(t)
	harness.tool.coordinateFocus = &computerUseCoordinateFocusV1{
		stateID:  harness.tool.snapshot.id,
		pid:      harness.tree.PID,
		bundleID: harness.tree.BundleID,
		windowID: uint32(*harness.tree.WindowID),
	}
	runtime, err := NewOpenAIComputerActionRuntimeV1(
		wrapGUIExecutionGate(harness.tool),
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := runtime.PlanOpenAIComputerActionV1(
		context.Background(),
		OpenAIComputerActionV1{
			Type: OpenAIComputerActionTypeTextV1,
			Text: "private text",
		},
	)
	if err != nil {
		t.Fatalf("PlanOpenAIComputerActionV1: %v", err)
	}
	var args computerUseArgs
	if err := json.Unmarshal([]byte(plan.Args), &args); err != nil {
		t.Fatal(err)
	}
	if !plan.Mutation || args.Action != "type" || args.Ref != "" ||
		args.Text == nil || *args.Text != "private text" ||
		args.StateID != harness.tool.snapshot.id {
		t.Fatalf("translated coordinate-focused type plan = %+v / %+v", plan, args)
	}
}

func TestOpenAIComputerActionRuntimePrefersLatestCoordinateFocusOverOldAXFocus(
	t *testing.T,
) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	harness.tree.Elements[0].Focused = true
	focused := harness.tree.Elements[0].Ref
	harness.tree.FocusedRef = &focused
	harness.observe(t)
	harness.tool.coordinateFocus = &computerUseCoordinateFocusV1{
		stateID:  harness.tool.snapshot.id,
		pid:      harness.tree.PID,
		bundleID: harness.tree.BundleID,
		windowID: uint32(*harness.tree.WindowID),
	}
	runtime, err := NewOpenAIComputerActionRuntimeV1(
		wrapGUIExecutionGate(harness.tool),
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := runtime.PlanOpenAIComputerActionV1(
		context.Background(),
		OpenAIComputerActionV1{
			Type: OpenAIComputerActionTypeTextV1,
			Text: "private text",
		},
	)
	if err != nil {
		t.Fatalf("PlanOpenAIComputerActionV1: %v", err)
	}
	var args computerUseArgs
	if err := json.Unmarshal([]byte(plan.Args), &args); err != nil {
		t.Fatal(err)
	}
	if args.Action != "type" || args.Ref != "" {
		t.Fatalf("old AX focus overrode the latest coordinate click: %+v", args)
	}
}

func TestOpenAIComputerActionRuntimeRejectsWindowBoundTypeWithoutVerifiedCoordinateFocus(
	t *testing.T,
) {
	harness, runtime := guardedOpenAIComputerRuntimeHarness(t)
	harness.tree.Elements = nil
	harness.tree.RefPaths = map[string]computerUseRefPath{}
	harness.tool.snapshot.elements = nil
	harness.tool.refs = map[string]refEntry{}

	plan, err := runtime.PlanOpenAIComputerActionV1(
		context.Background(),
		OpenAIComputerActionV1{
			Type: OpenAIComputerActionTypeTextV1,
			Text: "private text",
		},
	)
	if err == nil || !strings.Contains(err.Error(), "type target is unavailable") {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
}

func TestOpenAIComputerActionRuntimeProjectsEveryProviderDragWaypoint(t *testing.T) {
	harness, runtime := guardedOpenAIComputerRuntimeHarness(t)
	harness.fake.queue(
		"display_topology",
		marshalDisplayTopologyNoTest(harness.topology),
	)
	plan, err := runtime.PlanOpenAIComputerActionV1(
		context.Background(),
		OpenAIComputerActionV1{
			Type: OpenAIComputerActionDragV1,
			Path: []OpenAIComputerPointV1{
				{X: 1, Y: 2},
				{X: 3, Y: 4},
				{X: 5, Y: 6},
			},
		},
	)
	if err != nil {
		t.Fatalf("PlanOpenAIComputerActionV1: %v", err)
	}
	if !plan.Mutation ||
		!strings.Contains(plan.Args, `"path":[{"x":1,"y":2},{"x":3,"y":4},{"x":5,"y":6}]`) {
		t.Fatalf("polyline drag plan collapsed provider waypoints: %+v", plan)
	}
}

func TestOpenAIComputerActionRuntimeRejectsPixelScrollWithoutFrameAuthority(t *testing.T) {
	harness, runtime := guardedOpenAIComputerRuntimeHarness(t)
	harness.tool.coordinateArtifact = nil
	x, y, scrollX, scrollY := 7, 8, 0, -618
	callCount := len(harness.fake.calls)
	plan, err := runtime.PlanOpenAIComputerActionV1(
		context.Background(),
		OpenAIComputerActionV1{
			Type: OpenAIComputerActionScrollV1,
			X:    &x, Y: &y, ScrollX: &scrollX, ScrollY: &scrollY,
		},
	)
	if err == nil ||
		!strings.Contains(err.Error(), "coordinate authority is unavailable") {
		t.Fatalf("pixel scroll plan=%+v err=%v", plan, err)
	}
	if len(harness.fake.calls) != callCount {
		t.Fatalf("pixel scroll reached AX/legacy helper calls: %+v",
			harness.fake.calls[callCount:])
	}
}

func TestOpenAIComputerActionRuntimeProjectsOnlyExactSupportedUnion(t *testing.T) {
	_, runtime := guardedOpenAIComputerRuntimeHarness(t)
	tests := []struct {
		name   string
		action OpenAIComputerActionV1
		want   string
	}{
		{
			name: "keypress sequence",
			action: OpenAIComputerActionV1{
				Type: OpenAIComputerActionKeypressV1,
				Keys: []string{"command", "shift", "p", "a", "g", "e", "down"},
			},
			want: `"key_sequence":["p","a","g","e","down"],"modifiers":["command","shift"]`,
		},
		{
			name: "pixel scroll unavailable",
			action: OpenAIComputerActionV1{
				Type: OpenAIComputerActionScrollV1,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := runtime.PlanOpenAIComputerActionV1(
				context.Background(),
				test.action,
			)
			if test.want == "" {
				if err == nil {
					t.Fatalf("unsupported action projected: %+v", plan)
				}
				return
			}
			if err != nil || !strings.Contains(plan.Args, test.want) {
				t.Fatalf("plan = %+v err=%v, want %q", plan, err, test.want)
			}
		})
	}
}

func TestOpenAIComputerActionRuntimeProjectsPointerModifiers(t *testing.T) {
	harness, runtime := guardedOpenAIComputerRuntimeHarness(t)
	harness.fake.queue(
		"display_topology",
		marshalDisplayTopologyNoTest(harness.topology),
	)
	x, y := 4, 5
	plan, err := runtime.PlanOpenAIComputerActionV1(
		context.Background(),
		OpenAIComputerActionV1{
			Type: OpenAIComputerActionClickV1, Button: "left", X: &x, Y: &y,
			Keys: []string{"command", "shift"},
		},
	)
	if err != nil {
		t.Fatalf("PlanOpenAIComputerActionV1: %v", err)
	}
	var args computerUseArgs
	if err := json.Unmarshal([]byte(plan.Args), &args); err != nil {
		t.Fatal(err)
	}
	if want := []string{"command", "shift"}; !reflect.DeepEqual(args.Modifiers, want) {
		t.Fatalf("modifiers = %#v, want %#v", args.Modifiers, want)
	}
}

func TestOpenAIComputerActionRuntimePreservesEveryOfficialClickButton(t *testing.T) {
	for _, button := range []string{"left", "right", "wheel", "back", "forward"} {
		t.Run(button, func(t *testing.T) {
			harness, runtime := guardedOpenAIComputerRuntimeHarness(t)
			harness.fake.queue(
				"display_topology",
				marshalDisplayTopologyNoTest(harness.topology),
			)
			x, y := 4, 5
			plan, err := runtime.PlanOpenAIComputerActionV1(
				context.Background(),
				OpenAIComputerActionV1{
					Type:   OpenAIComputerActionClickV1,
					Button: button,
					X:      &x,
					Y:      &y,
				},
			)
			if err != nil {
				t.Fatalf("PlanOpenAIComputerActionV1: %v", err)
			}
			var args computerUseArgs
			if err := json.Unmarshal([]byte(plan.Args), &args); err != nil {
				t.Fatal(err)
			}
			if args.Action != "click" || args.Button != button || args.Clicks != 1 {
				t.Fatalf("button projection = %+v", args)
			}
		})
	}
}

func TestOpenAIComputerActionRuntimeSeparatesInternalRefreshFromFinalScreenshot(t *testing.T) {
	_, runtime := guardedOpenAIComputerRuntimeHarness(t)
	refresh, err := runtime.PlanOpenAIComputerObservationV1(
		"Refresh state",
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	final, err := runtime.PlanOpenAIComputerObservationV1(
		"Capture final",
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	var refreshArgs, finalArgs computerUseArgs
	if err := json.Unmarshal([]byte(refresh.Args), &refreshArgs); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(final.Args), &finalArgs); err != nil {
		t.Fatal(err)
	}
	if refresh.Mutation || final.Mutation ||
		refreshArgs.Action != "get_app_state" ||
		refreshArgs.IncludeScreenshot ||
		finalArgs.Action != "get_app_state" ||
		!finalArgs.IncludeScreenshot {
		t.Fatalf(
			"observation plans refresh=%+v/%+v final=%+v/%+v",
			refresh,
			refreshArgs,
			final,
			finalArgs,
		)
	}
}
