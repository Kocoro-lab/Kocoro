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
	if err := runtime.AuthorizeOpenAIComputerTypeAfterKeypressV1(
		OpenAIComputerActionV1{
			Type: OpenAIComputerActionKeypressV1,
			Keys: []string{"command", "l"},
		},
	); err != nil {
		t.Fatal(err)
	}
	typePlan, err := runtime.PlanOpenAIComputerActionV1(
		context.Background(),
		OpenAIComputerActionV1{
			Type: OpenAIComputerActionTypeTextV1,
			Text: "waylandz.com",
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
