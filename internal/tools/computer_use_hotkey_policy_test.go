package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

func TestComputerUseHotkeyDestinationAuthorityClassifierV1(t *testing.T) {
	blocked := []string{
		"return",
		" CmD + EnTeR ",
		"option + space",
		"control+SPACEBAR",
		"command+s",
		"cmd + shift + S",
		"command+option+p",
		"shift+cmd+w",
		"control + command + Q",
		"cmd+delete",
		"command + shift + backspace",
		"shift+delete",
		"option + SHIFT + BACKSPACE",
		"command+option+escape",
		"shift + cmd + alt + ESCAPE",
		"command+control+q",
	}
	for _, keys := range blocked {
		t.Run(keys, func(t *testing.T) {
			if !computerUseHotkeyRequiresDestinationAuthorityV1(keys) {
				t.Fatalf("dangerous raw hotkey was not classified: %q", keys)
			}
		})
	}

	for _, keys := range []string{
		"command+c",
		" CMD + C ",
		"command+shift+c",
		"control+tab",
		"option+left",
		"command+f",
	} {
		t.Run("allowed "+keys, func(t *testing.T) {
			if computerUseHotkeyRequiresDestinationAuthorityV1(keys) {
				t.Fatalf("ordinary navigation/copy hotkey was blocked: %q", keys)
			}
		})
	}
}

func TestComputerUseKeypressDestinationAuthorityClassifierV1(t *testing.T) {
	for _, test := range []struct {
		name      string
		modifiers []string
		keys      []string
	}{
		{name: "ordered return", keys: []string{"a", "RETURN"}},
		{name: "command save", modifiers: []string{"META"}, keys: []string{"a", "s"}},
		{name: "shift delete", modifiers: []string{"shift"}, keys: []string{"delete"}},
		{name: "force quit", modifiers: []string{"command", "option"}, keys: []string{"escape"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !computerUseKeypressRequiresDestinationAuthorityV1(test.modifiers, test.keys) {
				t.Fatalf("dangerous keypress was not classified: modifiers=%v keys=%v",
					test.modifiers, test.keys)
			}
		})
	}
	if computerUseKeypressRequiresDestinationAuthorityV1(
		[]string{"command", "shift"}, []string{"a", "b", "c"},
	) {
		t.Fatal("ordinary ordered keypress was blocked")
	}
}

func TestComputerUseLocationNavigationTextV1(t *testing.T) {
	for _, text := range []string{
		"waylandz.com",
		"https://waylandz.com/blog/",
		"http://localhost:3000",
		"https://127.0.0.1:8443/path",
	} {
		if !computerUseLocationNavigationTextV1(text) {
			t.Fatalf("navigation text rejected: %q", text)
		}
	}
	for _, text := range []string{
		"",
		"search terms",
		"user:password@example.com",
		"javascript:alert(1)",
		"https://example.com/\nnext",
	} {
		if computerUseLocationNavigationTextV1(text) {
			t.Fatalf("unsafe or non-location text accepted: %q", text)
		}
	}
}

func TestComputerUseNativeLocationShortcutTypeAllowsOneWindowBoundReturn(
	t *testing.T,
) {
	harness, runtime := guardedOpenAIComputerRuntimeHarness(t)
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
	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	harness.tool.targetBoundInputExecutor = func(
		_ context.Context,
		request TargetBoundInputRequestV1,
	) (TargetBoundInputResultV1, error) {
		failure := "postcondition_not_declared"
		return TargetBoundInputResultV1{
			SchemaVersion: 1, Status: "completed_unverified", Action: request.Action,
			InputCommitted:    true,
			ClipboardTouched:  request.Action == "type",
			ClipboardRestored: request.Action == "type",
			Phase:             "post_verification", FailureCode: &failure,
		}, nil
	}
	typed, err := harness.tool.Run(
		ContextWithOpenAINativeComputerActionV1(context.Background()),
		typePlan.Args,
	)
	if err != nil || typed.IsError || harness.tool.navigationCommit == nil {
		t.Fatalf("location type result=%+v err=%v commit=%+v",
			typed, err, harness.tool.navigationCommit)
	}

	stateID := harness.tool.snapshot.id
	returnArgs := fmt.Sprintf(
		`{"action":"keypress","state_id":%q,"key_sequence":["return"],"description":"Navigate to the typed URL"}`,
		stateID,
	)
	preflight, err := harness.tool.PreflightConsequentialRiskV1(
		context.Background(), returnArgs, "toolu_location_return_1")
	if err != nil || preflight.Status != ConsequentialRiskPreflightNoneV1 {
		t.Fatalf("location return preflight=%+v err=%v", preflight, err)
	}
	harness.fake.queue("read_tree", marshalComputerUseTree(t, harness.tree))
	pressed, err := harness.tool.Run(
		ContextWithOpenAINativeComputerActionV1(context.Background()),
		returnArgs,
	)
	if err != nil || pressed.IsError || harness.tool.navigationCommit != nil {
		t.Fatalf("location return result=%+v err=%v commit=%+v",
			pressed, err, harness.tool.navigationCommit)
	}

	reused, err := harness.tool.PreflightConsequentialRiskV1(
		context.Background(), returnArgs, "toolu_location_return_2")
	if err != nil || reused.Status != ConsequentialRiskPreflightBlockedV1 ||
		reused.FailureCode != ConsequentialRiskCodeUnsupportedPathV1 {
		t.Fatalf("reused location return preflight=%+v err=%v", reused, err)
	}
}

func TestComputerUseNonLocationKeypressTypeCannotAuthorizeReturn(
	t *testing.T,
) {
	harness, runtime := guardedOpenAIComputerRuntimeHarness(t)
	if err := runtime.AuthorizeOpenAIComputerTypeAfterKeypressV1(
		OpenAIComputerActionV1{
			Type: OpenAIComputerActionKeypressV1,
			Keys: []string{"tab"},
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
	if result, err := harness.tool.Run(
		ContextWithOpenAINativeComputerActionV1(context.Background()),
		typePlan.Args,
	); err != nil || result.IsError {
		t.Fatalf("ordinary type result=%+v err=%v", result, err)
	}
	if harness.tool.navigationCommit != nil {
		t.Fatal("non-location keypress minted navigation commit authority")
	}
	returnArgs := fmt.Sprintf(
		`{"action":"keypress","state_id":%q,"key_sequence":["return"],"description":"Must remain blocked"}`,
		harness.tool.snapshot.id,
	)
	preflight, err := harness.tool.PreflightConsequentialRiskV1(
		context.Background(), returnArgs, "toolu_non_location_return")
	if err != nil || preflight.Status != ConsequentialRiskPreflightBlockedV1 {
		t.Fatalf("non-location return preflight=%+v err=%v", preflight, err)
	}
}

func TestComputerUseDangerousRawHotkeyFailsBeforeExecutorAndConsumesState(t *testing.T) {
	requireComputerUseDarwin(t)
	for _, keys := range []string{
		"RETURN",
		" cmd + shift + S ",
		"option+SHIFT+backspace",
		"command+option+escape",
	} {
		t.Run(keys, func(t *testing.T) {
			fake := newFakeAXCaller()
			tool := newTestComputerUse(fake)
			stateID := observeNotes(t, tool, fake, treeFixture("Archive"))
			executed := false
			tool.targetBoundInputExecutor = func(
				context.Context, TargetBoundInputRequestV1,
			) (TargetBoundInputResultV1, error) {
				executed = true
				return TargetBoundInputResultV1{}, nil
			}

			result, err := tool.Run(context.Background(), fmt.Sprintf(
				`{"action":"hotkey","state_id":%q,"keys":%q,"description":"ordinary navigation"}`,
				stateID, keys))
			if err != nil || !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if executed {
				t.Fatal("dangerous raw hotkey reached target-bound executor")
			}
			if tool.snapshot != nil || tool.refs != nil || tool.coordinateArtifact != nil {
				t.Fatal("rejected hotkey retained current observation authority")
			}
			if result.GUIOutcome == nil ||
				result.GUIOutcome.Result != agent.GUIActionResultFailed ||
				result.GUIOutcome.Phase != agent.GUIActionPhaseActing ||
				result.GUIOutcome.FailureCode != ConsequentialRiskCodeUnsupportedPathV1 {
				t.Fatalf("rejected hotkey lost typed GUI outcome: %+v", result.GUIOutcome)
			}
			if strings.Contains(strings.ToLower(result.Content), strings.ToLower(strings.TrimSpace(keys))) {
				t.Fatalf("rejected hotkey leaked raw keys: %q", result.Content)
			}
		})
	}
}

func TestComputerUseOrdinaryCopyHotkeyStillReachesExecutor(t *testing.T) {
	requireComputerUseDarwin(t)
	fake := newFakeAXCaller()
	tool := newTestComputerUse(fake)
	stateID := observeNotes(t, tool, fake, treeFixture("Archive"))
	fake.queue("read_tree", treeFixture("Archive"))
	executed := false
	tool.targetBoundInputExecutor = func(
		_ context.Context, request TargetBoundInputRequestV1,
	) (TargetBoundInputResultV1, error) {
		executed = true
		failure := "postcondition_not_declared"
		return TargetBoundInputResultV1{
			SchemaVersion: 1, Status: "completed_unverified", Action: request.Action,
			InputCommitted: true, Phase: "post_verification", FailureCode: &failure,
		}, nil
	}

	result, err := tool.Run(context.Background(), fmt.Sprintf(
		`{"action":"hotkey","state_id":%q,"keys":" CMD + C ","description":"Copy"}`,
		stateID))
	if err != nil || result.IsError || !executed {
		t.Fatalf("ordinary copy hotkey result=%+v err=%v executed=%v", result, err, executed)
	}
}

func TestComputerUseDangerousRawHotkeyPreflightBlocksWithoutIntent(t *testing.T) {
	tool := &ComputerUseTool{}
	for _, keys := range []string{
		"enter",
		"cmd+shift+p",
		"shift+delete",
		"command+option+escape",
	} {
		t.Run(keys, func(t *testing.T) {
			result, err := tool.PreflightConsequentialRiskV1(
				context.Background(),
				fmt.Sprintf(`{"action":"hotkey","keys":%q,"description":"safe according to model prose"}`, keys),
				"toolu_hotkey_policy_1",
			)
			if err != nil || result.Status != ConsequentialRiskPreflightBlockedV1 ||
				result.FailureCode != ConsequentialRiskCodeUnsupportedPathV1 || result.Draft != nil {
				t.Fatalf("preflight=%+v err=%v", result, err)
			}
		})
	}

	allowed, err := tool.PreflightConsequentialRiskV1(
		context.Background(),
		`{"action":"hotkey","keys":"command+c","description":"send delete purchase"}`,
		"toolu_hotkey_policy_2",
	)
	if err != nil || allowed.Status != ConsequentialRiskPreflightNoneV1 || allowed.Draft != nil {
		t.Fatalf("ordinary copy preflight=%+v err=%v", allowed, err)
	}
}

func TestComputerUseDangerousNativeKeypressFailsBeforeExecutorAndPreflight(t *testing.T) {
	requireComputerUseDarwin(t)
	fake := newFakeAXCaller()
	tool := newTestComputerUse(fake)
	stateID := observeNotes(t, tool, fake, treeFixture("Archive"))
	executed := false
	tool.targetBoundInputExecutor = func(
		context.Context, TargetBoundInputRequestV1,
	) (TargetBoundInputResultV1, error) {
		executed = true
		return TargetBoundInputResultV1{}, nil
	}
	args := fmt.Sprintf(
		`{"action":"keypress","state_id":%q,"key_sequence":["a","RETURN"],"description":"continue"}`,
		stateID,
	)
	preflight, err := tool.PreflightConsequentialRiskV1(
		context.Background(), args, "toolu_keypress_policy_1")
	if err != nil || preflight.Status != ConsequentialRiskPreflightBlockedV1 ||
		preflight.FailureCode != ConsequentialRiskCodeUnsupportedPathV1 {
		t.Fatalf("preflight=%+v err=%v", preflight, err)
	}
	result, err := tool.Run(
		ContextWithOpenAINativeComputerActionV1(context.Background()), args)
	if err != nil || !result.IsError ||
		result.ErrorCategory != agent.ErrCategoryBusiness ||
		result.GUIOutcome == nil ||
		result.GUIOutcome.FailureCode != ConsequentialRiskCodeUnsupportedPathV1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if executed {
		t.Fatal("dangerous keypress reached target-bound executor")
	}
	if tool.snapshot != nil {
		t.Fatal("rejected keypress retained observation authority")
	}
}
