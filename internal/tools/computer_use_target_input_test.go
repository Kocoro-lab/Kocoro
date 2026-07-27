package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

func focusedTreeFixture(title string) string {
	tree := strings.Replace(treeFixture(title), `"focused_ref":null`, `"focused_ref":"e2"`, 1)
	return strings.Replace(
		tree,
		`"role":"AXTextField","title":"Body","value":"hello","value_redacted":false,"enabled":true,"focused":false`,
		`"role":"AXTextField","title":"Body","value":"hello","value_redacted":false,"enabled":true,"focused":true`,
		1)
}

func TestComputerUseTargetBoundTypeUsesLatestExactStateAndRedactsContent(t *testing.T) {
	requireComputerUseDarwin(t)
	fake := newFakeAXCaller()
	tool := newTestComputerUse(fake)
	stateID := observeNotes(t, tool, fake, focusedTreeFixture("Save"))
	fake.queue("read_tree", focusedTreeFixture("Save"))
	secret := "customer secret 🔐"
	var executed *TargetBoundInputRequestV1
	tool.targetBoundInputExecutor = func(
		_ context.Context, request TargetBoundInputRequestV1,
	) (TargetBoundInputResultV1, error) {
		executed = &request
		failure := "postcondition_not_declared"
		return TargetBoundInputResultV1{
			SchemaVersion: 1, Status: "completed_unverified", Action: "type",
			InputCommitted: true, ClipboardTouched: true, ClipboardRestored: true,
			Phase: "post_verification", FailureCode: &failure,
		}, nil
	}

	result, err := tool.Run(context.Background(), fmt.Sprintf(
		`{"action":"type","state_id":%q,"ref":"e2","text":%q,"description":"Type value"}`,
		stateID, secret))
	if err != nil || result.IsError {
		t.Fatalf("target-bound type result=%+v err=%v", result, err)
	}
	if executed == nil || executed.PID != 42 || executed.BundleID != "com.apple.Notes" ||
		executed.WindowID != 7001 || executed.ExpectedWindowAXBounds.Width != 800 ||
		executed.Ref == nil || *executed.Ref != "e2" ||
		executed.Path == nil || *executed.Path != "window[0]/AXTextField[0]" ||
		executed.ExpectedRole == nil || *executed.ExpectedRole != "AXTextField" ||
		executed.ExpectedFingerprint == nil || *executed.ExpectedFingerprint != "axf_e2" ||
		executed.Text == nil || *executed.Text != secret || executed.Key != nil ||
		executed.Modifiers != nil {
		t.Fatalf("target-bound request lost exact authority: %+v", executed)
	}
	if strings.Contains(result.Content, secret) || strings.Contains(result.Content, "customer") {
		t.Fatalf("typed content leaked into tool result: %q", result.Content)
	}
	if result.GUIOutcome == nil ||
		result.GUIOutcome.Result != agent.GUIActionResultCompletedUnverified ||
		result.GUIOutcome.Phase != agent.GUIActionPhaseVerifying {
		t.Fatalf("typed acknowledgement lost GUI outcome: %+v", result.GUIOutcome)
	}
	if tool.snapshot != nil || tool.refs != nil {
		t.Fatal("target-bound input did not consume state authority")
	}
	for _, call := range fake.calls {
		if call.method == "type_text" || call.method == "key_event" {
			t.Fatalf("target-bound input delegated to legacy global RPC: %+v", call)
		}
	}
}

func TestComputerUseTargetBoundHotkeyFocusDriftFailsWithoutPausingLease(t *testing.T) {
	requireComputerUseDarwin(t)
	fake := newFakeAXCaller()
	tool := newTestComputerUse(fake)
	stateID := observeNotes(t, tool, fake, treeFixture("Save"))
	fake.queue("read_tree", treeFixture("Save"))
	tool.targetBoundInputExecutor = func(
		_ context.Context, request TargetBoundInputRequestV1,
	) (TargetBoundInputResultV1, error) {
		failure := "frontmost_process_mismatch"
		return TargetBoundInputResultV1{
			SchemaVersion: 1, Status: "failed", Action: request.Action,
			Phase: "preflight", FailureCode: &failure,
		}, nil
	}

	result, err := tool.Run(context.Background(), fmt.Sprintf(
		`{"action":"hotkey","state_id":%q,"keys":"command+c","description":"Copy"}`,
		stateID))
	if err != nil || !result.IsError {
		t.Fatalf("interference result=%+v err=%v", result, err)
	}
	if result.GUIOutcome == nil ||
		result.GUIOutcome.Result != agent.GUIActionResultFailed ||
		result.GUIOutcome.FailureCode != "frontmost_process_mismatch" {
		t.Fatalf("target change was not typed user interference: %+v", result.GUIOutcome)
	}
	if strings.Contains(result.Content, "command") || strings.Contains(result.Content, "shift+p") {
		t.Fatalf("hotkey content leaked into result: %q", result.Content)
	}
}

func TestComputerUseTargetBoundHotkeyPropagatesPhysicalInterference(t *testing.T) {
	requireComputerUseDarwin(t)
	fake := newFakeAXCaller()
	tool := newTestComputerUse(fake)
	stateID := observeNotes(t, tool, fake, treeFixture("Save"))
	fake.queue("read_tree", treeFixture("Save"))
	tool.targetBoundInputExecutor = func(
		_ context.Context, request TargetBoundInputRequestV1,
	) (TargetBoundInputResultV1, error) {
		failure := "physical_input_interference"
		return TargetBoundInputResultV1{
			SchemaVersion: 1, Status: "user_interference", Action: request.Action,
			InputCommitted: true, Phase: "user_interference", FailureCode: &failure,
		}, nil
	}

	result, err := tool.Run(context.Background(), fmt.Sprintf(
		`{"action":"hotkey","state_id":%q,"keys":"command+c","description":"Copy"}`,
		stateID))
	if err != nil || !result.IsError || result.GUIOutcome == nil ||
		result.GUIOutcome.Result != agent.GUIActionResultUserInterference ||
		result.GUIOutcome.FailureCode != "physical_input_interference" ||
		!strings.Contains(result.Content, "re-observe") {
		t.Fatalf("physical interference result=%+v err=%v", result, err)
	}
}

func TestComputerUseTargetBoundTypeMapsExactReadbackToVerifiedWithoutLeakingContent(t *testing.T) {
	requireComputerUseDarwin(t)
	fake := newFakeAXCaller()
	tool := newTestComputerUse(fake)
	stateID := observeNotes(t, tool, fake, focusedTreeFixture("Save"))
	fake.queue("read_tree", focusedTreeFixture("Save"))
	postcondition := "target_value_matches_expected_edit"
	tool.targetBoundInputExecutor = func(
		_ context.Context, _ TargetBoundInputRequestV1,
	) (TargetBoundInputResultV1, error) {
		return TargetBoundInputResultV1{
			SchemaVersion: 1, Status: "verified", Action: "type",
			InputCommitted: true, ClipboardTouched: true, ClipboardRestored: true,
			Phase: "post_verification", Postcondition: &postcondition,
		}, nil
	}

	secret := "customer secret"
	result, err := tool.Run(context.Background(), fmt.Sprintf(
		`{"action":"type","state_id":%q,"ref":"e2","text":%q,"description":"Type value"}`,
		stateID, secret))
	if err != nil || result.IsError {
		t.Fatalf("verified type result=%+v err=%v", result, err)
	}
	if result.GUIOutcome == nil ||
		result.GUIOutcome.Result != agent.GUIActionResultVerified ||
		result.GUIOutcome.Phase != agent.GUIActionPhaseVerifying {
		t.Fatalf("verified type outcome=%+v", result.GUIOutcome)
	}
	if strings.Contains(result.Content, secret) || strings.Contains(result.Content, "customer") {
		t.Fatalf("verified result leaked input: %q", result.Content)
	}
}

func TestComputerUseTargetBoundTypeMapsFocusDriftToFailureWithoutPausing(t *testing.T) {
	for _, failureCode := range []string{
		"focused_window_mismatch",
		"frontmost_window_mismatch",
		"focused_element_mismatch",
	} {
		result := TargetBoundInputResultV1{
			SchemaVersion: 1, Status: "failed", Action: "type",
			InputCommitted: false, Phase: "action", FailureCode: &failureCode,
		}
		outcome := computerUseTargetBoundInputGUIOutcomeV1(result)
		if outcome.Result != agent.GUIActionResultFailed ||
			outcome.FailureCode != failureCode {
			t.Fatalf("%s outcome=%+v", failureCode, outcome)
		}
	}
}

func TestComputerUseTargetBoundInputRequiresFreshStateBeforeExecutor(t *testing.T) {
	requireComputerUseDarwin(t)
	fake := newFakeAXCaller()
	tool := newTestComputerUse(fake)
	stateID := observeNotes(t, tool, fake, focusedTreeFixture("Save"))
	fake.queue("read_tree", focusedTreeFixture("Changed"))
	executed := false
	tool.targetBoundInputExecutor = func(
		context.Context, TargetBoundInputRequestV1,
	) (TargetBoundInputResultV1, error) {
		executed = true
		return TargetBoundInputResultV1{}, nil
	}

	result, err := tool.Run(context.Background(), fmt.Sprintf(
		`{"action":"type","state_id":%q,"ref":"e2","text":"redacted","description":"Type"}`,
		stateID))
	if err != nil || !result.IsError || !strings.Contains(result.Content, "stale state") {
		t.Fatalf("stale target result=%+v err=%v", result, err)
	}
	if result.GUIOutcome == nil ||
		result.GUIOutcome.Result != agent.GUIActionResultFailed ||
		result.GUIOutcome.Phase != agent.GUIActionPhaseActing ||
		result.GUIOutcome.FailureCode != "stale_state" {
		t.Fatalf("stale precommit result lost known not-committed evidence: %+v",
			result.GUIOutcome)
	}
	if executed {
		t.Fatal("stale state reached target-bound executor")
	}
}

func TestComputerUseTargetBoundInputRequiresStateID(t *testing.T) {
	requireComputerUseDarwin(t)
	tool := newTestComputerUse(newFakeAXCaller())
	result, err := tool.Run(context.Background(),
		`{"action":"type","ref":"e2","text":"redacted","description":"Type"}`)
	if err != nil || !result.IsError || result.ErrorCategory != agent.ErrCategoryValidation ||
		!strings.Contains(result.Content, "latest state_id") {
		t.Fatalf("missing state result=%+v err=%v", result, err)
	}
}

func TestComputerUseTargetBoundKeypressEncodesNoModifiersAsExplicitArray(t *testing.T) {
	requireComputerUseDarwin(t)
	fake := newFakeAXCaller()
	tool := newTestComputerUse(fake)
	stateID := observeNotes(t, tool, fake, treeFixture("Save"))
	fake.queue("read_tree", treeFixture("Save"))
	var executed *TargetBoundInputRequestV1
	tool.targetBoundInputExecutor = func(
		_ context.Context,
		request TargetBoundInputRequestV1,
	) (TargetBoundInputResultV1, error) {
		executed = &request
		failure := "postcondition_not_declared"
		return TargetBoundInputResultV1{
			SchemaVersion: 1, Status: "completed_unverified", Action: request.Action,
			InputCommitted: true, Phase: "post_verification", FailureCode: &failure,
		}, nil
	}

	result, err := tool.Run(
		ContextWithOpenAINativeComputerActionV1(context.Background()),
		fmt.Sprintf(
			`{"action":"keypress","state_id":%q,"key_sequence":["tab"],"description":"Advance focus"}`,
			stateID,
		),
	)
	if err != nil || result.IsError || executed == nil {
		t.Fatalf("keypress result=%+v request=%+v err=%v", result, executed, err)
	}
	if executed.Modifiers == nil || *executed.Modifiers == nil ||
		len(*executed.Modifiers) != 0 {
		t.Fatalf("keypress modifiers were not an explicit empty array: %+v", executed.Modifiers)
	}
	wire, err := EncodeTargetBoundInputRPCRequestV1(TargetBoundInputRPCRequestV1{
		ID: 901, Method: "target_bound_input", Params: *executed,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), `"modifiers":[]`) ||
		strings.Contains(string(wire), `"modifiers":null`) {
		t.Fatalf("keypress wire did not preserve explicit empty modifiers: %s", wire)
	}
}

func TestComputerUseTargetBoundHelperInvalidRequestIsPrecommitFailure(t *testing.T) {
	requireComputerUseDarwin(t)
	fake := newFakeAXCaller()
	tool := newTestComputerUse(fake)
	stateID := observeNotes(t, tool, fake, treeFixture("Save"))
	fake.queue("read_tree", treeFixture("Save"))
	tool.targetBoundInputExecutor = func(
		context.Context,
		TargetBoundInputRequestV1,
	) (TargetBoundInputResultV1, error) {
		failure := "invalid_request"
		return TargetBoundInputResultV1{
			SchemaVersion: 1, Status: "failed", Action: "unknown",
			Phase: "preflight", FailureCode: &failure,
		}, nil
	}

	result, err := tool.Run(
		ContextWithOpenAINativeComputerActionV1(context.Background()),
		fmt.Sprintf(
			`{"action":"keypress","state_id":%q,"key_sequence":["tab"],"description":"Advance focus"}`,
			stateID,
		),
	)
	if err != nil || !result.IsError || result.GUIOutcome == nil ||
		result.GUIOutcome.Result != agent.GUIActionResultFailed ||
		result.GUIOutcome.Phase != agent.GUIActionPhaseActing ||
		result.GUIOutcome.FailureCode != "invalid_request" ||
		strings.Contains(result.Content, "commit status is unknown") {
		t.Fatalf("helper rejection was not a precommit failure: result=%+v err=%v", result, err)
	}
}

func TestComputerUseTargetBoundTypeRequiresFocusedRefOrCoordinateFocus(t *testing.T) {
	requireComputerUseDarwin(t)
	tool := newTestComputerUse(newFakeAXCaller())
	result, err := tool.Run(context.Background(),
		`{"action":"type","state_id":"s_state","text":"redacted","description":"Type"}`)
	if err != nil || !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness ||
		!strings.Contains(result.Content, "keyboard_target_unavailable") ||
		!strings.Contains(result.Content, "do not retry automatically") {
		t.Fatalf("missing ref result=%+v err=%v", result, err)
	}
}

func TestComputerUseTargetBoundInputAmbiguousAckNeverClaimsDefiniteFailure(t *testing.T) {
	requireComputerUseDarwin(t)
	for _, test := range []struct {
		name        string
		execute     computerUseTargetBoundInputExecutorV1
		failureCode string
	}{
		{
			name: "commit unknown",
			execute: func(context.Context, TargetBoundInputRequestV1) (TargetBoundInputResultV1, error) {
				return TargetBoundInputResultV1{}, newTargetBoundInputCommitUnknownV1(context.Canceled)
			},
			failureCode: "commit_unknown",
		},
		{
			name: "invalid helper result",
			execute: func(context.Context, TargetBoundInputRequestV1) (TargetBoundInputResultV1, error) {
				return TargetBoundInputResultV1{}, nil
			},
			failureCode: "invalid_helper_result",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeAXCaller()
			tool := newTestComputerUse(fake)
			stateID := observeNotes(t, tool, fake, treeFixture("Save"))
			fake.queue("read_tree", treeFixture("Save"))
			tool.targetBoundInputExecutor = test.execute
			result, err := tool.Run(context.Background(), fmt.Sprintf(
				`{"action":"hotkey","state_id":%q,"keys":"command+c","description":"Copy"}`,
				stateID))
			if err != nil || !result.IsError {
				t.Fatalf("ambiguous result=%+v err=%v", result, err)
			}
			if result.GUIOutcome == nil ||
				result.GUIOutcome.Result != agent.GUIActionResultCompletedUnverified ||
				result.GUIOutcome.Phase != agent.GUIActionPhaseInputCommitted ||
				result.GUIOutcome.FailureCode != test.failureCode {
				t.Fatalf("ambiguous input was reported as definite failure: %+v", result.GUIOutcome)
			}
		})
	}
}

func TestComputerUseScrollWithoutExactAuthorityFailsBeforeAnyAXRPC(t *testing.T) {
	requireComputerUseDarwin(t)
	for _, args := range []string{
		`{"action":"scroll","dx":0,"dy":1,"description":"Scroll window"}`,
		`{"action":"scroll","state_id":"state_old","ref":"e2","dx":0,"dy":1,"description":"Scroll element"}`,
	} {
		fake := newFakeAXCaller()
		tool := newTestComputerUse(fake)
		result, err := tool.Run(context.Background(), args)
		if err != nil || !result.IsError ||
			(!strings.Contains(result.Content, "requires 'ref'") && !strings.Contains(result.Content, "stale state_id")) {
			t.Fatalf("scroll result=%+v err=%v", result, err)
		}
		if len(fake.calls) != 0 {
			t.Fatalf("excluded scroll reached AX RPC: %+v", fake.calls)
		}
	}
}
