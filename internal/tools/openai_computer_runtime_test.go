package tools

import (
	"context"
	"encoding/json"
	"errors"
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

func backgroundKeyboardRuntimeHarness(
	t *testing.T,
) (*computerUseCoordinateHarness, *OpenAIComputerActionRuntimeV1, string) {
	t.Helper()
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	focused := "e2"
	harness.tree.Elements = []computerUseElement{{
		Ref: "e2", Fingerprint: "axf_e2", Path: "window[0]/AXTextField[0]",
		Role: "AXTextField", Title: stringPointer("Body"),
		Value: stringPointer("before"), ValueRedacted: false,
		Enabled: boolPointer(true), Focused: true,
	}}
	harness.tree.RefPaths = map[string]computerUseRefPath{
		"e2": {
			Path: "window[0]/AXTextField[0]", Role: "AXTextField",
			Fingerprint: "axf_e2",
		},
	}
	harness.tree.FocusedRef = &focused
	harness.observe(t)
	public := wrapGUIExecutionGate(harness.tool)
	runtime, err := NewOpenAIComputerActionRuntimeV1(public)
	if err != nil {
		t.Fatal(err)
	}
	runtime.executionLane = OpenAIComputerExecutionBackgroundSemanticV1
	runtime.backgroundRequired = true
	runtime.backgroundTarget = &OpenAIComputerTaskAppV1{
		App: harness.tree.App, BundleID: harness.tree.BundleID,
		PID: harness.tree.PID, LaunchDate: "2026-07-28T06:00:00Z",
	}
	harness.tool.backgroundInputAuthority = &computerUseBackgroundInputAuthorityV1{
		targetLaunchDate:             "2026-07-28T06:00:00Z",
		preservedFrontmostPID:        84,
		preservedFrontmostBundleID:   "com.apple.TextEdit",
		preservedFrontmostLaunchDate: "2026-07-28T05:00:00Z",
	}
	return harness, runtime, focused
}

func TestOpenAIComputerBackgroundTypeUsesTargetedKeyboardLane(t *testing.T) {
	harness, runtime, focused := backgroundKeyboardRuntimeHarness(t)
	plan, err := runtime.PlanOpenAIComputerActionV1(
		context.Background(),
		OpenAIComputerActionV1{
			Type: OpenAIComputerActionTypeTextV1,
			Text: "private text",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	var args computerUseArgs
	if err := json.Unmarshal([]byte(plan.Args), &args); err != nil {
		t.Fatal(err)
	}
	if !plan.Mutation || args.Action != "type" || args.Ref != focused ||
		args.Text == nil || *args.Text != "private text" ||
		args.StateID != harness.tool.snapshot.id ||
		args.ExecutionLane != "background_keyboard" ||
		args.ForegroundFallback {
		t.Fatalf("background type plan = %+v / %+v", plan, args)
	}
}

func TestOpenAIComputerBackgroundSafeKeypressUsesTargetedKeyboardLane(
	t *testing.T,
) {
	_, runtime, focused := backgroundKeyboardRuntimeHarness(t)
	plan, err := runtime.PlanOpenAIComputerActionV1(
		context.Background(),
		OpenAIComputerActionV1{
			Type: OpenAIComputerActionKeypressV1,
			Keys: []string{"left"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	var args computerUseArgs
	if err := json.Unmarshal([]byte(plan.Args), &args); err != nil {
		t.Fatal(err)
	}
	if args.Action != "keypress" || args.Ref != focused ||
		!reflect.DeepEqual(args.KeySequence, []string{"left"}) ||
		args.ExecutionLane != "background_keyboard" ||
		args.ForegroundFallback {
		t.Fatalf("background keypress plan = %+v", args)
	}
}

func TestOpenAIComputerBackgroundConsequentialKeyFailsBeforeExecution(
	t *testing.T,
) {
	_, runtime, _ := backgroundKeyboardRuntimeHarness(t)
	_, err := runtime.PlanOpenAIComputerActionV1(
		context.Background(),
		OpenAIComputerActionV1{
			Type: OpenAIComputerActionKeypressV1,
			Keys: []string{"return"},
		},
	)
	var planErr *OpenAIComputerActionPlanErrorV1
	if !errors.As(err, &planErr) ||
		planErr.FailureCode !=
			"background_keyboard_consequential_key_unsupported" {
		t.Fatalf("background Return error = %v", err)
	}
}

func TestOpenAIComputerBackgroundControlTextFailsBeforeExecution(t *testing.T) {
	_, runtime, _ := backgroundKeyboardRuntimeHarness(t)
	_, err := runtime.PlanOpenAIComputerActionV1(
		context.Background(),
		OpenAIComputerActionV1{
			Type: OpenAIComputerActionTypeTextV1,
			Text: "line one\nline two",
		},
	)
	var planErr *OpenAIComputerActionPlanErrorV1
	if !errors.As(err, &planErr) ||
		planErr.FailureCode != "background_keyboard_control_text_unsupported" {
		t.Fatalf("background control text error = %v", err)
	}
}

func TestOpenAIComputerRequiredBackgroundTypeWithoutProcessAuthorityFailsClosed(
	t *testing.T,
) {
	_, runtime, _ := backgroundKeyboardRuntimeHarness(t)
	runtime.raw.backgroundInputAuthority = nil
	_, err := runtime.PlanOpenAIComputerActionV1(
		context.Background(),
		OpenAIComputerActionV1{
			Type: OpenAIComputerActionTypeTextV1,
			Text: "must not type",
		},
	)
	var planErr *OpenAIComputerActionPlanErrorV1
	if !errors.As(err, &planErr) ||
		planErr.FailureCode != "background_keyboard_target_unavailable" {
		t.Fatalf("missing background authority error = %v", err)
	}
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
	fake.queue(
		"prepare_task_app",
		`{"app":"Slack","bundle_id":"com.tinyspeck.slackmacgap","pid":101}`,
	)
	fake.queue(
		"prepare_task_app",
		`{"app":"Calculator","bundle_id":"com.apple.calculator","pid":202}`,
	)
	fake.queue("focus", `{"result":"focused"}`)
	raw := &ComputerUseTool{
		client: fake,
		initialTarget: &ComputerUseInitialTargetV1{
			PID: 9, AppName: "Kocoro Desktop",
			BundleID: "run.shannon.shanclaw.dev",
		},
		snapshot: &computerUseSnapshot{id: "old"},
	}
	runtime := &OpenAIComputerActionRuntimeV1{
		raw: raw,
		taskAppExcludedPIDs: func() []int {
			return []int{9002}
		},
	}

	apps := []OpenAIComputerTaskAppV1{
		{
			App: "Slack", BundleID: "com.tinyspeck.slackmacgap",
			PID: 101,
		},
		{
			App: "Calculator", BundleID: "com.apple.calculator",
			PID: 202,
		},
	}
	err := runtime.LaunchAndFocusTaskAppsV1(
		context.Background(),
		apps,
	)
	if err != nil {
		t.Fatal(err)
	}
	if raw.initialTarget != nil || raw.snapshot != nil {
		t.Fatalf("stale foreground/bootstrap state survived: %+v / %+v",
			raw.initialTarget, raw.snapshot)
	}
	if len(fake.calls) != 3 ||
		fake.calls[0].method != "prepare_task_app" ||
		fake.calls[1].method != "prepare_task_app" ||
		fake.calls[2].method != "focus" {
		t.Fatalf("app preparation calls = %+v", fake.calls)
	}
	for index, expectedPID := range []int{101, 202} {
		if fake.calls[index].params["pid"] != expectedPID ||
			!reflect.DeepEqual(
				fake.calls[index].params["excluded_pids"],
				[]int{9002},
			) {
			t.Fatalf("prepare params %d = %+v", index, fake.calls[index].params)
		}
	}
	if fake.calls[2].params["pid"] != 101 ||
		fake.calls[2].params["bundle_id"] !=
			"com.tinyspeck.slackmacgap" ||
		!reflect.DeepEqual(
			fake.calls[2].params["excluded_pids"],
			[]int{9002},
		) {
		t.Fatalf("exact focus params = %+v", fake.calls[2].params)
	}
}

func TestOpenAIComputerTaskAppResolutionExcludesManagedAutomationPIDs(
	t *testing.T,
) {
	fake := newFakeAXCaller()
	fake.queue(
		"resolve_app_identity",
		`{"app":"Google Chrome","bundle_id":"com.google.Chrome","pid":77}`,
	)
	runtime := &OpenAIComputerActionRuntimeV1{
		raw: &ComputerUseTool{client: fake},
		taskAppExcludedPIDs: func() []int {
			return []int{9002, 9002, -1}
		},
	}

	app, err := runtime.ResolveTaskAppV1(context.Background(), "Google Chrome")
	if err != nil {
		t.Fatal(err)
	}
	if app.PID != 77 || app.BundleID != "com.google.Chrome" {
		t.Fatalf("resolved app = %+v", app)
	}
	if len(fake.calls) != 1 ||
		!reflect.DeepEqual(
			fake.calls[0].params["excluded_pids"],
			[]int{9002},
		) {
		t.Fatalf("resolve params = %+v", fake.calls)
	}
}

func TestOpenAIComputerTaskPreparationPinsExistingPIDAndLearnsLaunchedPID(
	t *testing.T,
) {
	fake := newFakeAXCaller()
	fake.queue(
		"prepare_task_app",
		`{"app":"Slack","bundle_id":"com.tinyspeck.slackmacgap","pid":55}`,
	)
	fake.queue(
		"prepare_task_app",
		`{"app":"Calculator","bundle_id":"com.apple.calculator","pid":88}`,
	)
	fake.queue("focus", `{"result":"focused"}`)
	runtime := &OpenAIComputerActionRuntimeV1{
		raw: &ComputerUseTool{client: fake},
		taskAppExcludedPIDs: func() []int {
			return []int{9002}
		},
	}
	apps := []OpenAIComputerTaskAppV1{
		{App: "Slack", BundleID: "com.tinyspeck.slackmacgap"},
		{
			App: "Calculator", BundleID: "com.apple.calculator",
			PID: 88,
		},
	}

	if err := runtime.LaunchAndFocusTaskAppsV1(
		context.Background(),
		apps,
	); err != nil {
		t.Fatal(err)
	}
	if apps[0].PID != 55 || apps[1].PID != 88 {
		t.Fatalf("prepared app identities = %+v", apps)
	}
	if _, present := fake.calls[0].params["pid"]; present {
		t.Fatalf("unlaunched app carried invented pid: %+v", fake.calls[0].params)
	}
	if fake.calls[1].params["pid"] != 88 {
		t.Fatalf("existing app lost exact pid: %+v", fake.calls[1].params)
	}
}

func TestOpenAIComputerTaskPreparationRejectsExistingPIDSubstitution(
	t *testing.T,
) {
	fake := newFakeAXCaller()
	fake.queue(
		"prepare_task_app",
		`{"app":"Google Chrome","bundle_id":"com.google.Chrome","pid":78}`,
	)
	runtime := &OpenAIComputerActionRuntimeV1{
		raw: &ComputerUseTool{client: fake},
		taskAppExcludedPIDs: func() []int {
			return []int{9002}
		},
	}

	err := runtime.LaunchAndFocusTaskAppsV1(
		context.Background(),
		[]OpenAIComputerTaskAppV1{{
			App: "Google Chrome", BundleID: "com.google.Chrome", PID: 77,
		}},
	)
	if err == nil || !strings.Contains(err.Error(), "changed exact pid") {
		t.Fatalf("pid substitution error = %v", err)
	}
}

func TestOpenAIComputerForegroundAllowedPreparationUsesForegroundPath(
	t *testing.T,
) {
	fake := newFakeAXCaller()
	fake.queue(
		"prepare_task_app",
		`{"app":"Calculator","bundle_id":"com.apple.calculator","pid":77}`,
	)
	raw := &ComputerUseTool{client: fake}
	runtime := &OpenAIComputerActionRuntimeV1{
		raw: raw,
		taskAppExcludedPIDs: func() []int {
			return []int{9002}
		},
	}
	apps := []OpenAIComputerTaskAppV1{{
		App: "Calculator", BundleID: "com.apple.calculator", PID: 77,
	}}

	lane, err := runtime.PrepareTaskAppsV1(
		context.Background(),
		apps,
		OpenAIComputerTaskPreparationOptionsV1{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if lane != OpenAIComputerExecutionForegroundV1 {
		t.Fatalf("preparation lane = %q", lane)
	}
	if len(fake.calls) != 1 ||
		fake.calls[0].method != "prepare_task_app" ||
		fake.calls[0].params["pid"] != 77 ||
		fake.calls[0].params["bundle_id"] != "com.apple.calculator" {
		t.Fatalf("foreground preparation calls = %+v", fake.calls)
	}
}

func TestOpenAIComputerTaskPreparationInstallsExactBackgroundKeyboardAuthority(
	t *testing.T,
) {
	fake := newFakeAXCaller()
	fake.queue(
		"bind_background_task_app",
		`{"app":"Calculator","bundle_id":"com.apple.calculator","pid":77,`+
			`"launch_date":"2026-07-28T06:00:00Z",`+
			`"preserved_frontmost_pid":84,`+
			`"preserved_frontmost_bundle_id":"com.apple.TextEdit",`+
			`"preserved_frontmost_launch_date":"2026-07-28T05:00:00Z"}`,
	)
	raw := &ComputerUseTool{client: fake}
	runtime := &OpenAIComputerActionRuntimeV1{raw: raw}
	apps := []OpenAIComputerTaskAppV1{{
		App: "Calculator", BundleID: "com.apple.calculator", PID: 77,
	}}

	lane, err := runtime.PrepareTaskAppsV1(
		context.Background(),
		apps,
		OpenAIComputerTaskPreparationOptionsV1{RequireBackground: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	authority := raw.backgroundInputAuthority
	if lane != OpenAIComputerExecutionBackgroundSemanticV1 ||
		authority == nil ||
		authority.targetLaunchDate != "2026-07-28T06:00:00Z" ||
		authority.preservedFrontmostPID != 84 ||
		authority.preservedFrontmostBundleID != "com.apple.TextEdit" ||
		authority.preservedFrontmostLaunchDate != "2026-07-28T05:00:00Z" {
		t.Fatalf("background keyboard authority = %+v, lane=%q", authority, lane)
	}
}

func TestOpenAIComputerTaskPreparationRejectsPartialBackgroundProcessAuthority(
	t *testing.T,
) {
	fake := newFakeAXCaller()
	fake.queue(
		"bind_background_task_app",
		`{"app":"Calculator","bundle_id":"com.apple.calculator","pid":77,`+
			`"launch_date":"2026-07-28T06:00:00Z",`+
			`"preserved_frontmost_pid":84}`,
	)
	runtime := &OpenAIComputerActionRuntimeV1{
		raw: &ComputerUseTool{client: fake},
	}
	_, err := runtime.PrepareTaskAppsV1(
		context.Background(),
		[]OpenAIComputerTaskAppV1{{
			App: "Calculator", BundleID: "com.apple.calculator", PID: 77,
		}},
		OpenAIComputerTaskPreparationOptionsV1{RequireBackground: true},
	)
	if err == nil || !strings.Contains(
		err.Error(), "invalid preserved frontmost identity") {
		t.Fatalf("partial background authority error = %v", err)
	}
}

func TestOpenAIComputerForegroundAllowedPreparationDoesNotProbeBackground(
	t *testing.T,
) {
	fake := newFakeAXCaller()
	fake.queue(
		"prepare_task_app",
		`{"app":"Calculator","bundle_id":"com.apple.calculator","pid":77}`,
	)
	runtime := &OpenAIComputerActionRuntimeV1{
		raw: &ComputerUseTool{client: fake},
	}
	apps := []OpenAIComputerTaskAppV1{{
		App: "Calculator", BundleID: "com.apple.calculator", PID: 77,
	}}

	lane, err := runtime.PrepareTaskAppsV1(
		context.Background(),
		apps,
		OpenAIComputerTaskPreparationOptionsV1{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if lane != OpenAIComputerExecutionForegroundV1 {
		t.Fatalf("foreground lane = %q", lane)
	}
	if len(fake.calls) != 1 ||
		fake.calls[0].method != "prepare_task_app" {
		t.Fatalf("foreground preparation probed background: %+v", fake.calls)
	}
}

func TestOpenAIComputerTaskRequiredBackgroundBindNeverActivatesTarget(
	t *testing.T,
) {
	fake := newFakeAXCaller()
	fake.errors["bind_background_task_app"] = []error{
		errors.New("background target is unavailable"),
	}
	runtime := &OpenAIComputerActionRuntimeV1{
		raw: &ComputerUseTool{client: fake},
	}
	apps := []OpenAIComputerTaskAppV1{{
		App: "Calculator", BundleID: "com.apple.calculator", PID: 77,
	}}

	_, err := runtime.PrepareTaskAppsV1(
		context.Background(),
		apps,
		OpenAIComputerTaskPreparationOptionsV1{
			RequireBackground: true,
		},
	)
	if err == nil || !strings.Contains(err.Error(), "required background bind") {
		t.Fatalf("required background error = %v", err)
	}
	if len(fake.calls) != 1 ||
		fake.calls[0].method != "bind_background_task_app" {
		t.Fatalf("required background preparation activated target: %+v", fake.calls)
	}
	if !runtime.backgroundRequired || runtime.foregroundFallback {
		t.Fatalf("required background state = %+v", runtime)
	}
}

func TestOpenAIComputerTaskInitialObservationPinsFirstAppHint(t *testing.T) {
	fake := newFakeAXCaller()
	fake.queue(
		"read_tree",
		`{"schema_version":1,"app":"Google Chrome","app_name":"Google Chrome",`+
			`"bundle_id":"com.google.Chrome","pid":77,"window":"waylandz.com",`+
			`"window_title":"waylandz.com","window_id":7001,`+
			`"window_frame":{"x":0,"y":0,"width":1200,"height":800},`+
			`"elements":[],"ref_paths":{}}`,
	)
	raw := &ComputerUseTool{
		client:   fake,
		snapshot: &computerUseSnapshot{id: "stale"},
	}
	runtime := &OpenAIComputerActionRuntimeV1{
		public: raw,
		raw:    raw,
	}
	plan, err := runtime.PlanOpenAIComputerTaskInitialObservationV1(
		&OpenAIComputerTaskAppV1{
			App:      "Google Chrome",
			BundleID: "com.google.Chrome",
			PID:      77,
		},
		"Capture the initial browser state",
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if raw.snapshot != nil {
		t.Fatal("initial app observation retained stale state")
	}
	var args computerUseArgs
	if err := json.Unmarshal([]byte(plan.Args), &args); err != nil {
		t.Fatal(err)
	}
	if args.Action != "get_app_state" ||
		args.App != "" ||
		args.IncludeScreenshot {
		t.Fatalf("initial observation args = %+v", args)
	}
	if raw.initialTarget == nil ||
		raw.initialTarget.PID != 77 ||
		raw.initialTarget.AppName != "Google Chrome" ||
		raw.initialTarget.BundleID != "com.google.Chrome" {
		t.Fatalf("exact initial target = %+v", raw.initialTarget)
	}
	if result, runErr := plan.Tool.Run(
		ContextWithOpenAINativeComputerActionV1(context.Background()),
		plan.Args,
	); runErr != nil || result.IsError {
		t.Fatalf("initial observation result = %+v err=%v", result, runErr)
	}
	if len(fake.calls) != 1 ||
		fake.calls[0].method != "read_tree" ||
		fake.calls[0].params["pid"] != 77 {
		t.Fatalf("initial observation calls = %+v", fake.calls)
	}

	next, err := runtime.PlanOpenAIComputerObservationV1(
		"Follow the actual frontmost app",
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	args = computerUseArgs{}
	if err := json.Unmarshal([]byte(next.Args), &args); err != nil {
		t.Fatal(err)
	}
	if args.App != "" {
		t.Fatalf("continuation froze optional app hint: %+v", args)
	}
	if raw.initialTarget != nil {
		t.Fatalf("continuation retained first-app target: %+v", raw.initialTarget)
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

func TestOpenAIComputerBackgroundClickUsesUniqueAXPressWithoutCoordinateInput(
	t *testing.T,
) {
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
	runtime.executionLane = OpenAIComputerExecutionBackgroundSemanticV1
	runtime.backgroundTarget = &OpenAIComputerTaskAppV1{
		App: harness.tree.App, BundleID: harness.tree.BundleID, PID: harness.tree.PID,
	}
	x, y := 4, 5
	plan, err := runtime.PlanOpenAIComputerActionV1(
		context.Background(),
		OpenAIComputerActionV1{
			Type: OpenAIComputerActionClickV1, Button: "left", X: &x, Y: &y,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	var args computerUseArgs
	if err := json.Unmarshal([]byte(plan.Args), &args); err != nil {
		t.Fatal(err)
	}
	if args.Action != "press" || args.Ref != harness.tree.Elements[0].Ref ||
		args.X != nil || args.Y != nil ||
		args.ExecutionLane != "background_semantic" ||
		args.ForegroundFallback {
		t.Fatalf("background semantic plan = %+v", args)
	}
}

func TestOpenAIComputerBackgroundScrollUsesUniqueAXTargetWithoutPointerInput(
	t *testing.T,
) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	harness.tree.Elements[0].Frame = harness.tree.WindowFrame
	harness.tree.Elements[0].Role = "AXScrollArea"
	harness.tree.Elements[0].Path = "window[0]/AXScrollArea[0]"
	harness.tree.Elements[0].Actions = nil
	harness.tree.RefPaths["e1"] = computerUseRefPath{
		Path:        "window[0]/AXScrollArea[0]",
		Role:        "AXScrollArea",
		Fingerprint: harness.tree.Elements[0].Fingerprint,
	}
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
	runtime.executionLane = OpenAIComputerExecutionBackgroundSemanticV1
	runtime.backgroundRequired = true
	runtime.backgroundTarget = &OpenAIComputerTaskAppV1{
		App: harness.tree.App, BundleID: harness.tree.BundleID, PID: harness.tree.PID,
	}
	x, y, scrollX, scrollY := 4, 5, 0, 450
	plan, err := runtime.PlanOpenAIComputerActionV1(
		context.Background(),
		OpenAIComputerActionV1{
			Type: OpenAIComputerActionScrollV1,
			X:    &x, Y: &y, ScrollX: &scrollX, ScrollY: &scrollY,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	var args computerUseArgs
	if err := json.Unmarshal([]byte(plan.Args), &args); err != nil {
		t.Fatal(err)
	}
	if args.Action != "scroll" || args.Ref != harness.tree.Elements[0].Ref ||
		args.DX != 0 || args.DY != 5 ||
		args.X != nil || args.Y != nil ||
		args.ExecutionLane != "background_semantic" ||
		args.ForegroundFallback {
		t.Fatalf("background semantic scroll plan = %+v", args)
	}
}

func TestOpenAIComputerUnsupportedBackgroundActionFallsBackMonotonically(
	t *testing.T,
) {
	harness, runtime := guardedOpenAIComputerRuntimeHarness(t)
	runtime.executionLane = OpenAIComputerExecutionBackgroundSemanticV1
	runtime.backgroundTarget = &OpenAIComputerTaskAppV1{
		App: harness.tree.App, BundleID: harness.tree.BundleID, PID: harness.tree.PID,
	}
	harness.fake.queue(
		"display_topology",
		marshalDisplayTopologyNoTest(harness.topology),
	)
	x, y := 4, 5
	plan, err := runtime.PlanOpenAIComputerActionV1(
		context.Background(),
		OpenAIComputerActionV1{
			Type: OpenAIComputerActionMoveV1,
			X:    &x,
			Y:    &y,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	var args computerUseArgs
	if err := json.Unmarshal([]byte(plan.Args), &args); err != nil {
		t.Fatal(err)
	}
	if args.ExecutionLane != "foreground" || !args.ForegroundFallback {
		t.Fatalf("foreground fallback plan = %+v", args)
	}
	if runtime.executionLane != OpenAIComputerExecutionForegroundV1 ||
		!runtime.foregroundFallback ||
		runtime.backgroundTarget != nil ||
		!harness.tool.foregroundFallbackRestorePending {
		t.Fatalf("runtime did not transition monotonically: %+v", runtime)
	}

	next, err := runtime.PlanOpenAIComputerObservationV1(
		"Capture after fallback",
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	args = computerUseArgs{}
	if err := json.Unmarshal([]byte(next.Args), &args); err != nil {
		t.Fatal(err)
	}
	if args.ExecutionLane != "foreground" || !args.ForegroundFallback {
		t.Fatalf("fallback was not retained by later observation: %+v", args)
	}
}

func TestOpenAIComputerRequiredBackgroundActionNeverFallsBack(
	t *testing.T,
) {
	harness, runtime := guardedOpenAIComputerRuntimeHarness(t)
	runtime.executionLane = OpenAIComputerExecutionBackgroundSemanticV1
	runtime.backgroundRequired = true
	runtime.backgroundTarget = &OpenAIComputerTaskAppV1{
		App: harness.tree.App, BundleID: harness.tree.BundleID, PID: harness.tree.PID,
	}
	x, y := 4, 5
	_, err := runtime.PlanOpenAIComputerActionV1(
		context.Background(),
		OpenAIComputerActionV1{
			Type: OpenAIComputerActionMoveV1,
			X:    &x,
			Y:    &y,
		},
	)
	var planErr *OpenAIComputerActionPlanErrorV1
	if !errors.As(err, &planErr) ||
		planErr.FailureCode != "background_action_unsupported" {
		t.Fatalf("required background action error = %v", err)
	}
	if runtime.executionLane != OpenAIComputerExecutionBackgroundSemanticV1 ||
		runtime.foregroundFallback ||
		runtime.backgroundTarget == nil {
		t.Fatalf("required background action changed lane: %+v", runtime)
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

func TestOpenAIComputerActionRuntimeBindsPostKeypressObservationForOneType(
	t *testing.T,
) {
	harness, runtime := guardedOpenAIComputerRuntimeHarness(t)
	harness.tree.Elements = nil
	harness.tree.RefPaths = map[string]computerUseRefPath{}
	harness.tree.FocusedRef = nil
	harness.observe(t)

	if err := runtime.AuthorizeOpenAIComputerTypeAfterKeypressV1(
		OpenAIComputerActionV1{
			Type: OpenAIComputerActionKeypressV1,
			Keys: []string{"command", "l"},
		},
	); err != nil {
		t.Fatalf("AuthorizeOpenAIComputerTypeAfterKeypressV1: %v", err)
	}
	plan, err := runtime.PlanOpenAIComputerActionV1(
		context.Background(),
		OpenAIComputerActionV1{
			Type: OpenAIComputerActionTypeTextV1,
			Text: "waylandz.com",
		},
	)
	if err != nil {
		t.Fatalf("PlanOpenAIComputerActionV1: %v", err)
	}
	var args computerUseArgs
	if err := json.Unmarshal([]byte(plan.Args), &args); err != nil {
		t.Fatal(err)
	}
	if args.Action != "type" || args.Ref != "" ||
		args.StateID != harness.tool.snapshot.id {
		t.Fatalf("post-keypress type plan = %+v", args)
	}

	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	harness.tool.targetBoundInputExecutor = func(
		_ context.Context,
		request TargetBoundInputRequestV1,
	) (TargetBoundInputResultV1, error) {
		failure := "postcondition_not_declared"
		return TargetBoundInputResultV1{
			SchemaVersion: 1, Status: "completed_unverified", Action: request.Action,
			InputCommitted: true, ClipboardTouched: true, ClipboardRestored: true,
			Phase: "post_verification", FailureCode: &failure,
		}, nil
	}
	typed, err := harness.tool.Run(
		ContextWithOpenAINativeComputerActionV1(context.Background()),
		plan.Args,
	)
	if err != nil || typed.IsError {
		t.Fatalf("post-keypress type result=%+v err=%v", typed, err)
	}

	if harness.tool.coordinateFocus != nil {
		t.Fatal("post-keypress one-shot coordinate target was reusable")
	}
	secondPlan, err := runtime.PlanOpenAIComputerActionV1(
		context.Background(),
		OpenAIComputerActionV1{
			Type: OpenAIComputerActionTypeTextV1,
			Text: "must not reuse",
		},
	)
	if err == nil || secondPlan.Args != "" ||
		!strings.Contains(err.Error(), "type target is unavailable") {
		t.Fatalf("one-shot window focus was reusable: plan=%+v err=%v",
			secondPlan, err)
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
	var planErr *OpenAIComputerActionPlanErrorV1
	if !errors.As(err, &planErr) ||
		planErr.FailureCode != "coordinate_authority_unavailable" {
		t.Fatalf("pixel scroll typed plan error = %v", err)
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
	harness, runtime := guardedOpenAIComputerRuntimeHarness(t)
	refresh, err := runtime.PlanOpenAIComputerObservationV1(
		"Refresh state",
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if harness.tool.snapshot != nil || harness.tool.coordinateArtifact != nil {
		t.Fatal("internal refresh retained the previous app observation")
	}
	harness.observe(t)
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
	if harness.tool.snapshot != nil || harness.tool.coordinateArtifact != nil {
		t.Fatal("final provider screenshot retained the previous app observation")
	}
}

func TestOpenAIComputerActionRuntimeInternalRefreshFollowsNewFrontmostApp(
	t *testing.T,
) {
	harness, runtime := guardedOpenAIComputerRuntimeHarness(t)
	frame := harness.tool.snapshot.expectedWindowAXBounds
	harness.tool.coordinateFocus = &computerUseCoordinateFocusV1{
		stateID:                harness.tool.snapshot.id,
		pid:                    harness.tree.PID,
		bundleID:               harness.tree.BundleID,
		windowID:               uint32(*harness.tree.WindowID),
		expectedWindowAXBounds: *frame,
		filter:                 harness.tool.snapshot.filter,
		budget:                 harness.tool.snapshot.budget,
	}
	next := harness.tree
	next.App = "Calculator"
	next.BundleID = "com.apple.calculator"
	next.PID = 222
	nextWindowID := 333
	next.WindowID = &nextWindowID
	encoded, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	harness.fake.queue("read_tree", string(encoded))

	refresh, err := runtime.PlanOpenAIComputerObservationV1(
		"Follow the frontmost app after Command-Tab",
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := harness.tool.Run(
		ContextWithOpenAINativeComputerActionV1(context.Background()),
		refresh.Args,
	)
	if err != nil || result.IsError {
		t.Fatalf("refresh result=%+v err=%v", result, err)
	}
	if harness.tool.snapshot == nil ||
		harness.tool.snapshot.bundleID != "com.apple.calculator" ||
		harness.tool.snapshot.pid != 222 ||
		harness.tool.snapshot.windowID == nil ||
		*harness.tool.snapshot.windowID != nextWindowID {
		t.Fatalf("refresh retained old target: %+v", harness.tool.snapshot)
	}
	if harness.tool.coordinateFocus != nil {
		t.Fatalf(
			"frontmost app switch retained old click focus: %+v",
			harness.tool.coordinateFocus,
		)
	}
}

func TestOpenAIComputerActionRuntimeCarriesVerifiedClickFocusAcrossSameWindowObservation(
	t *testing.T,
) {
	harness, runtime := guardedOpenAIComputerRuntimeHarness(t)
	frame := harness.tool.snapshot.expectedWindowAXBounds
	harness.tool.coordinateFocus = &computerUseCoordinateFocusV1{
		stateID:                harness.tool.snapshot.id,
		pid:                    harness.tree.PID,
		bundleID:               harness.tree.BundleID,
		windowID:               uint32(*harness.tree.WindowID),
		expectedWindowAXBounds: *frame,
		filter:                 harness.tool.snapshot.filter,
		budget:                 harness.tool.snapshot.budget,
	}
	harness.fake.queue(
		"read_tree",
		marshalComputerUseTree(t, harness.tree),
	)

	observation, err := runtime.PlanOpenAIComputerObservationV1(
		"Observe the same window after a verified click",
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := harness.tool.Run(
		ContextWithOpenAINativeComputerActionV1(context.Background()),
		observation.Args,
	)
	if err != nil || result.IsError {
		t.Fatalf("observation result=%+v err=%v", result, err)
	}
	if harness.tool.coordinateFocus == nil ||
		harness.tool.coordinateFocus.stateID != harness.tool.snapshot.id {
		t.Fatalf(
			"same-window observation lost verified click focus: focus=%+v snapshot=%+v",
			harness.tool.coordinateFocus,
			harness.tool.snapshot,
		)
	}
	typed, err := runtime.PlanOpenAIComputerActionV1(
		context.Background(),
		OpenAIComputerActionV1{
			Type: OpenAIComputerActionTypeTextV1,
			Text: "type after observation",
		},
	)
	if err != nil {
		t.Fatalf("type after same-window observation: %v", err)
	}
	var args computerUseArgs
	if err := json.Unmarshal([]byte(typed.Args), &args); err != nil {
		t.Fatal(err)
	}
	if args.Action != "type" || args.Ref != "" {
		t.Fatalf("same-window type projection = %+v", args)
	}
}
