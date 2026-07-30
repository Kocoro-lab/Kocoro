package tools

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

func TestComputerUseOrdinaryWindowBoundHotkeysDoNotUseWorkflowSpecificDenylist(
	t *testing.T,
) {
	requireComputerUseDarwin(t)
	for _, keys := range []string{
		"command+shift+s",
		"command+option+escape",
	} {
		t.Run(keys, func(t *testing.T) {
			fake := newFakeAXCaller()
			tool := newTestComputerUse(fake)
			stateID := observeNotes(t, tool, fake, treeFixture("Archive"))
			args := fmt.Sprintf(
				`{"action":"hotkey","state_id":%q,"keys":%q,"description":"Window-bound keyboard action"}`,
				stateID,
				keys,
			)
			preflight, err := tool.PreflightConsequentialRiskV1(
				context.Background(),
				args,
				"toolu_generic_hotkey",
			)
			if err != nil || preflight.Status != ConsequentialRiskPreflightNoneV1 {
				t.Fatalf("preflight=%+v err=%v", preflight, err)
			}

			fake.queue("read_tree", treeFixture("Changed while focused"))
			executed := false
			tool.targetBoundInputExecutor = func(
				_ context.Context,
				request TargetBoundInputRequestV1,
			) (TargetBoundInputResultV1, error) {
				executed = true
				failure := "postcondition_not_declared"
				return TargetBoundInputResultV1{
					SchemaVersion:  1,
					Status:         "completed_unverified",
					Action:         request.Action,
					InputCommitted: true,
					Phase:          "post_verification",
					FailureCode:    &failure,
				}, nil
			}
			result, err := tool.Run(context.Background(), args)
			if err != nil || result.IsError || !executed {
				t.Fatalf("result=%+v err=%v executed=%v", result, err, executed)
			}
		})
	}
}

func TestComputerUseConsequentialKeyboardNeedsExactIntent(t *testing.T) {
	requireComputerUseDarwin(t)
	for _, raw := range []string{
		`{"action":"keypress","state_id":"s_state","key_sequence":["return"],"description":"Activate"}`,
		`{"action":"keypress","state_id":"s_state","key_sequence":["space"],"description":"Activate"}`,
		`{"action":"hotkey","state_id":"s_state","keys":"shift+delete","description":"Delete"}`,
	} {
		t.Run(raw, func(t *testing.T) {
			tool := newTestComputerUse(newFakeAXCaller())
			preflight, err := tool.PreflightConsequentialRiskV1(
				context.Background(),
				raw,
				"toolu_generic_keyboard",
			)
			if err != nil ||
				preflight.Status != ConsequentialRiskPreflightBlockedV1 ||
				preflight.FailureCode != ConsequentialRiskCodeUnsupportedPathV1 {
				t.Fatalf("preflight=%+v err=%v", preflight, err)
			}
			result, err := tool.Run(
				ContextWithOpenAINativeComputerActionV1(context.Background()),
				raw,
			)
			if err != nil || !result.IsError {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestComputerUsePlainReturnAllowsFreshOrdinaryEditableFocus(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	harness.tree = computerUseCoordinateFocusedTextTreeV1(harness.tree)
	title := "Document body"
	harness.tree.Elements[0].Title = &title
	harness.observe(t)
	raw := fmt.Sprintf(
		`{"action":"keypress","state_id":%q,"key_sequence":["return"],"description":"Insert a new line"}`,
		harness.tool.snapshot.id,
	)
	preflight, err := harness.tool.PreflightConsequentialRiskV1(
		context.Background(),
		raw,
		"toolu_plain_return",
	)
	if err != nil || preflight.Status != ConsequentialRiskPreflightNoneV1 {
		t.Fatalf("preflight=%+v err=%v", preflight, err)
	}
	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	executed := false
	harness.tool.targetBoundInputExecutor = func(
		_ context.Context,
		request TargetBoundInputRequestV1,
	) (TargetBoundInputResultV1, error) {
		executed = true
		failure := "postcondition_not_declared"
		return TargetBoundInputResultV1{
			SchemaVersion: 1, Status: "completed_unverified",
			Action: request.Action, InputCommitted: true,
			Phase: "post_verification", FailureCode: &failure,
		}, nil
	}
	result, err := harness.tool.Run(
		ContextWithOpenAINativeComputerActionV1(context.Background()),
		raw,
	)
	if err != nil || result.IsError || !executed {
		t.Fatalf("result=%+v err=%v executed=%v", result, err, executed)
	}
}

func TestComputerUsePlainReturnBlocksMessageComposerShortcut(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	harness.tree = computerUseCoordinateFocusedTextTreeV1(harness.tree)
	title := "Message Zoro"
	harness.tree.Elements[0].Title = &title
	harness.observe(t)
	raw := fmt.Sprintf(
		`{"action":"keypress","state_id":%q,"key_sequence":["return"],"description":"Send the message"}`,
		harness.tool.snapshot.id,
	)
	preflight, err := harness.tool.PreflightConsequentialRiskV1(
		context.Background(),
		raw,
		"toolu_send_return",
	)
	if err != nil ||
		preflight.Status != ConsequentialRiskPreflightBlockedV1 ||
		preflight.FailureCode != ConsequentialRiskCodeUnsupportedPathV1 {
		t.Fatalf("preflight=%+v err=%v", preflight, err)
	}
	executed := false
	harness.tool.targetBoundInputExecutor = func(
		_ context.Context,
		request TargetBoundInputRequestV1,
	) (TargetBoundInputResultV1, error) {
		executed = true
		return TargetBoundInputResultV1{}, nil
	}
	result, err := harness.tool.Run(
		ContextWithOpenAINativeComputerActionV1(context.Background()),
		raw,
	)
	if err != nil || !result.IsError || executed ||
		result.GUIOutcome == nil ||
		result.GUIOutcome.FailureCode !=
			ConsequentialRiskCodeUnsupportedPathV1 {
		t.Fatalf(
			"result=%+v err=%v executed=%v",
			result,
			err,
			executed,
		)
	}
}

func TestComputerUsePlainReturnRevalidatesFocusedElement(t *testing.T) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	harness.tree = computerUseCoordinateFocusedTextTreeV1(harness.tree)
	title := "Document body"
	harness.tree.Elements[0].Title = &title
	harness.observe(t)
	raw := fmt.Sprintf(
		`{"action":"keypress","state_id":%q,"key_sequence":["return"],"description":"Insert a new line"}`,
		harness.tool.snapshot.id,
	)
	changed := cloneComputerUseTree(t, harness.tree)
	changed.FocusedRef = nil
	changed.Elements[0].Focused = false
	harness.fake.queue("read_tree", marshalComputerUseTree(t, changed))
	executed := false
	harness.tool.targetBoundInputExecutor = func(
		_ context.Context,
		request TargetBoundInputRequestV1,
	) (TargetBoundInputResultV1, error) {
		executed = true
		return TargetBoundInputResultV1{}, nil
	}
	result, err := harness.tool.Run(
		ContextWithOpenAINativeComputerActionV1(context.Background()),
		raw,
	)
	if err != nil || !result.IsError || executed ||
		result.GUIOutcome == nil ||
		result.GUIOutcome.FailureCode !=
			"keyboard_focused_element_changed" {
		t.Fatalf(
			"result=%+v err=%v executed=%v",
			result,
			err,
			executed,
		)
	}
}

func TestComputerUseModifiedSpaceRemainsOrdinaryNavigation(t *testing.T) {
	requireComputerUseDarwin(t)
	raw := `{"action":"keypress","state_id":"s_state","modifiers":["command"],"key_sequence":["space"],"description":"Switch app"}`
	tool := newTestComputerUse(newFakeAXCaller())
	preflight, err := tool.PreflightConsequentialRiskV1(
		context.Background(),
		raw,
		"toolu_command_space",
	)
	if err != nil || preflight.Status != ConsequentialRiskPreflightNoneV1 {
		t.Fatalf("preflight=%+v err=%v", preflight, err)
	}
}

func TestComputerUseVerifiedLocationTypeAuthorizesOneReturn(
	t *testing.T,
) {
	harness, runtime := guardedOpenAIComputerRuntimeHarness(t)
	harness.tree.Elements = nil
	harness.tree.RefPaths = map[string]computerUseRefPath{}
	harness.tree.FocusedRef = nil
	harness.observe(t)
	keypress := OpenAIComputerActionV1{
		Type: OpenAIComputerActionKeypressV1,
		Keys: []string{"command", "l"},
	}
	if _, err := runtime.PlanOpenAIComputerActionV1(
		context.Background(),
		keypress,
	); err != nil {
		t.Fatal(err)
	}
	if err := runtime.AuthorizeOpenAIComputerTypeAfterKeypressV1(keypress); err != nil {
		t.Fatal(err)
	}
	typePlan, err := runtime.PlanOpenAIComputerActionV1(
		context.Background(),
		OpenAIComputerActionV1{
			Type: OpenAIComputerActionTypeTextV1,
			Text: "example.com",
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	var actions []string
	harness.tool.targetBoundInputExecutor = func(
		_ context.Context,
		request TargetBoundInputRequestV1,
	) (TargetBoundInputResultV1, error) {
		actions = append(actions, request.Action)
		failure := "postcondition_not_declared"
		return TargetBoundInputResultV1{
			SchemaVersion:     1,
			Status:            "completed_unverified",
			Action:            request.Action,
			InputCommitted:    true,
			ClipboardTouched:  request.Action == "type",
			ClipboardRestored: request.Action == "type",
			Phase:             "post_verification",
			FailureCode:       &failure,
		}, nil
	}
	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	typed, err := harness.tool.Run(
		ContextWithOpenAINativeComputerActionV1(context.Background()),
		typePlan.Args,
	)
	if err != nil || typed.IsError {
		t.Fatalf("type result=%+v err=%v", typed, err)
	}
	if len(actions) != 1 {
		t.Fatalf("window-bound URL type actions=%v", actions)
	}

	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	returnArgs := fmt.Sprintf(
		`{"action":"keypress","state_id":%q,"key_sequence":["return"],"description":"Activate the current target"}`,
		harness.tool.snapshot.id,
	)
	pressed, err := harness.tool.Run(
		ContextWithOpenAINativeComputerActionV1(context.Background()),
		returnArgs,
	)
	if err != nil || pressed.IsError {
		t.Fatalf("return result=%+v err=%v", pressed, err)
	}
	if !reflect.DeepEqual(actions, []string{"type", "keypress"}) {
		t.Fatalf("generic keyboard actions=%v", actions)
	}
	blocked, err := harness.tool.PreflightConsequentialRiskV1(
		context.Background(),
		returnArgs,
		"toolu_reused_return",
	)
	if err != nil || blocked.Status != ConsequentialRiskPreflightBlockedV1 {
		t.Fatalf("reused return preflight=%+v err=%v", blocked, err)
	}
}

func TestComputerUseLocationReturnSurvivesTypedFieldFingerprintChange(
	t *testing.T,
) {
	requireComputerUseDarwin(t)
	harness := newComputerUseCoordinateHarness(t)
	locationTitle := "Address and Search"
	harness.tree.Elements = []computerUseElement{{
		Ref: "e1", Fingerprint: "axf_location_before",
		Path: "window[0]/AXTextField[0]", Role: "AXTextField",
		Title: &locationTitle, Focused: true, ValueRedacted: false,
		Enabled: boolPointer(true),
	}}
	harness.tree.RefPaths = map[string]computerUseRefPath{
		"e1": {
			Path: "window[0]/AXTextField[0]", Role: "AXTextField",
			Fingerprint: "axf_location_before",
		},
	}
	focused := "e1"
	harness.tree.FocusedRef = &focused
	harness.observe(t)
	runtime, err := NewOpenAIComputerActionRuntimeV1(
		wrapGUIExecutionGate(harness.tool),
	)
	if err != nil {
		t.Fatal(err)
	}
	keypress := OpenAIComputerActionV1{
		Type: OpenAIComputerActionKeypressV1,
		Keys: []string{"command", "l"},
	}
	if _, err := runtime.PlanOpenAIComputerActionV1(
		context.Background(),
		keypress,
	); err != nil {
		t.Fatal(err)
	}
	if err := runtime.AuthorizeOpenAIComputerTypeAfterKeypressV1(keypress); err != nil {
		t.Fatal(err)
	}
	typePlan, err := runtime.PlanOpenAIComputerActionV1(
		context.Background(),
		OpenAIComputerActionV1{
			Type: OpenAIComputerActionTypeTextV1,
			Text: "example.com",
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	var actions []string
	harness.tool.targetBoundInputExecutor = func(
		_ context.Context,
		request TargetBoundInputRequestV1,
	) (TargetBoundInputResultV1, error) {
		actions = append(actions, request.Action)
		failure := "postcondition_not_declared"
		return TargetBoundInputResultV1{
			SchemaVersion: 1, Status: "completed_unverified",
			Action: request.Action, InputCommitted: true,
			ClipboardTouched:  request.Action == "type",
			ClipboardRestored: request.Action == "type",
			Phase:             "post_verification",
			FailureCode:       &failure,
		}, nil
	}
	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	typed, err := harness.tool.Run(
		ContextWithOpenAINativeComputerActionV1(context.Background()),
		typePlan.Args,
	)
	if err != nil || typed.IsError {
		t.Fatalf("type result=%+v err=%v", typed, err)
	}

	afterType := cloneComputerUseTree(t, harness.tree)
	afterType.Elements[0].Fingerprint = "axf_location_after"
	afterType.RefPaths["e1"] = computerUseRefPath{
		Path: "window[0]/AXTextField[0]", Role: "AXTextField",
		Fingerprint: "axf_location_after",
	}
	harness.fake.queue("read_tree", marshalComputerUseTree(t, afterType))
	returnArgs := fmt.Sprintf(
		`{"action":"keypress","state_id":%q,"key_sequence":["return"],"description":"Navigate to the typed URL"}`,
		harness.tool.snapshot.id,
	)
	pressed, err := harness.tool.Run(
		ContextWithOpenAINativeComputerActionV1(context.Background()),
		returnArgs,
	)
	if err != nil || pressed.IsError {
		t.Fatalf("return result=%+v err=%v", pressed, err)
	}
	if !reflect.DeepEqual(actions, []string{"type", "keypress"}) {
		t.Fatalf("location navigation actions=%v", actions)
	}

	// The relaxed value fingerprint must not turn into a generic same-window
	// Return grant: a different focused AX path still fails before execution.
	harness.tool.navigationCommit = &computerUseNavigationCommitV1{
		pid:      afterType.PID,
		bundleID: afterType.BundleID,
		windowID: uint32(*afterType.WindowID),
		path:     "window[0]/AXTextField[0]",
		role:     "AXTextField",
	}
	pathChanged := cloneComputerUseTree(t, afterType)
	pathChanged.Elements[0].Path = "window[0]/AXTextField[1]"
	pathChanged.RefPaths["e1"] = computerUseRefPath{
		Path: "window[0]/AXTextField[1]", Role: "AXTextField",
		Fingerprint: "axf_location_after",
	}
	harness.fake.queue("read_tree", marshalComputerUseTree(t, pathChanged))
	blocked, err := harness.tool.Run(
		ContextWithOpenAINativeComputerActionV1(context.Background()),
		returnArgs,
	)
	if err != nil || !blocked.IsError || blocked.GUIOutcome == nil ||
		blocked.GUIOutcome.FailureCode != "keyboard_target_unavailable" {
		t.Fatalf("focused path drift result=%+v err=%v", blocked, err)
	}
	if !reflect.DeepEqual(actions, []string{"type", "keypress"}) {
		t.Fatalf("focused path drift executed another action: %v", actions)
	}
}
