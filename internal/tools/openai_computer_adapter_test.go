package tools

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

const openAIComputerContinuationTokenForTest = "shct_pOIBMOn2gmZdU7TJZm93xdhEM1SNRTRle-n9A0mz76g"

type openAIComputerExecutorProbe struct {
	authorityCalls int
	authority      OpenAIComputerBatchAuthorityV1
	authorityErr   error
	authorities    []OpenAIComputerBatchAuthorityV1
	scopes         []OpenAIComputerActionScopeV1
	actions        []OpenAIComputerActionV1
	executions     []OpenAIComputerActionExecutionV1
	executionErrs  []error
	finalCalls     int
	finalResult    agent.ToolResult
	finalErr       error
	afterAction    func(index int)
}

func (p *openAIComputerExecutorProbe) AcquireOpenAIComputerBatchAuthorityV1(
	_ context.Context,
	call OpenAIComputerCallV1,
) (OpenAIComputerBatchAuthorityV1, error) {
	p.authorityCalls++
	authority := p.authority
	if authority.LeaseID == "" {
		authority = OpenAIComputerBatchAuthorityV1{
			LeaseID: "cul_batch", ResponseID: call.ResponseID, CallID: call.CallID,
			Provider: call.Provider, APISurface: call.APISurface,
			ToolContract: call.ToolContract,
		}
	}
	return authority, p.authorityErr
}

func (p *openAIComputerExecutorProbe) ExecuteAuthorizedOpenAIComputerActionV1(
	_ context.Context,
	authority OpenAIComputerBatchAuthorityV1,
	scope OpenAIComputerActionScopeV1,
	action OpenAIComputerActionV1,
) (OpenAIComputerActionExecutionV1, error) {
	p.authorities = append(p.authorities, authority)
	p.scopes = append(p.scopes, scope)
	p.actions = append(p.actions, action)
	index := len(p.actions) - 1
	var execution OpenAIComputerActionExecutionV1
	if index < len(p.executions) {
		execution = p.executions[index]
	}
	var err error
	if index < len(p.executionErrs) {
		err = p.executionErrs[index]
	}
	if p.afterAction != nil {
		p.afterAction(index)
	}
	return execution, err
}

func (p *openAIComputerExecutorProbe) CaptureFinalOpenAIComputerObservationV1(
	_ context.Context,
	authority OpenAIComputerBatchAuthorityV1,
	_ OpenAIComputerCallV1,
) (agent.ToolResult, error) {
	p.authorities = append(p.authorities, authority)
	p.finalCalls++
	return p.finalResult, p.finalErr
}

func validOpenAIComputerCallPayload() string {
	return `{
		"type":"computer_call",
		"provider":"openai",
		"api_surface":"openai_responses",
		"tool_contract":"openai.computer.v1",
		"response_id":"` + openAIComputerContinuationTokenForTest + `",
		"call_id":"call_002",
		"actions":[
			{"type":"click","button":"left","x":405,"y":157},
			{"type":"type","text":"penguin"}
		],
		"pending_safety_checks":[
			{"id":"sc_confirm","code":"confirm_external_side_effect","message":"Confirm this action."},
			{"id":"sc_nullable","code":null,"message":null}
		],
		"status":"completed"
	}`
}

func openAIComputerCallWithActions(actions string) string {
	return `{
		"type":"computer_call",
		"provider":"openai",
		"api_surface":"openai_responses",
		"tool_contract":"openai.computer.v1",
		"response_id":"` + openAIComputerContinuationTokenForTest + `",
		"call_id":"call_shapes",
		"actions":[` + actions + `],
		"pending_safety_checks":[],
		"status":"completed"
	}`
}

func finalOpenAIComputerObservation() agent.ToolResult {
	return agent.ToolResult{
		Content: "final exact observation",
		Images: []agent.ImageBlock{{
			MediaType: "image/png",
			Data:      "final-image",
		}},
	}
}

func TestDecodeOpenAIComputerCallV1PreservesBatchProvenance(t *testing.T) {
	call, err := DecodeOpenAIComputerCallV1([]byte(validOpenAIComputerCallPayload()))
	if err != nil {
		t.Fatalf("DecodeOpenAIComputerCallV1: %v", err)
	}
	if call.Type != OpenAIComputerCallTypeV1 ||
		call.Provider != OpenAIComputerProviderV1 ||
		call.APISurface != client.APISurfaceOpenAIResponses ||
		call.ToolContract != client.ToolContractOpenAIComputerV1 ||
		call.ResponseID != openAIComputerContinuationTokenForTest ||
		call.CallID != "call_002" ||
		call.Status != OpenAIComputerCallStatusCompletedV1 {
		t.Fatalf("provenance = %#v", call)
	}
	if len(call.Actions) != 2 ||
		call.Actions[0].Type != OpenAIComputerActionClickV1 ||
		call.Actions[1].Type != OpenAIComputerActionTypeTextV1 {
		t.Fatalf("actions = %#v", call.Actions)
	}
	if len(call.PendingSafetyChecks) != 2 ||
		call.PendingSafetyChecks[0].ID != "sc_confirm" ||
		call.PendingSafetyChecks[0].Code == nil ||
		*call.PendingSafetyChecks[0].Code != "confirm_external_side_effect" ||
		call.PendingSafetyChecks[1].Code != nil ||
		call.PendingSafetyChecks[1].Message != nil {
		t.Fatalf("pending safety checks = %#v", call.PendingSafetyChecks)
	}
}

func TestOpenAIComputerCallV1CanonicalNormalizedEnvelopeRoundTrip(t *testing.T) {
	const normalized = `{"type":"computer_call","provider":"openai","api_surface":"openai_responses","tool_contract":"openai.computer.v1","response_id":"` + openAIComputerContinuationTokenForTest + `","call_id":"call_002","actions":[{"type":"click","button":"left","x":405,"y":157},{"type":"type","text":"penguin"}],"pending_safety_checks":[{"id":"sc_confirm","code":"confirm_external_side_effect","message":"Confirm this action."},{"id":"sc_nullable","code":null,"message":null}],"status":"completed"}`
	call, err := DecodeOpenAIComputerCallV1([]byte(normalized))
	if err != nil {
		t.Fatalf("DecodeOpenAIComputerCallV1: %v", err)
	}
	encoded, err := json.Marshal(call)
	if err != nil {
		t.Fatalf("marshal normalized envelope: %v", err)
	}
	if string(encoded) != normalized {
		t.Fatalf("normalized envelope drifted:\n got: %s\nwant: %s", encoded, normalized)
	}
}

func TestDecodeOpenAIComputerCallV1RejectsUnknownBeforeAnyExecution(t *testing.T) {
	payloads := []string{
		strings.Replace(validOpenAIComputerCallPayload(), `"status":"completed"`, `"status":"completed","extra":true`, 1),
		strings.Replace(validOpenAIComputerCallPayload(), `"api_surface":"openai_responses"`, `"api_surface":"anthropic_messages"`, 1),
		strings.Replace(validOpenAIComputerCallPayload(), `"tool_contract":"openai.computer.v1"`, `"tool_contract":"kocoro.computer_use.v1"`, 1),
		strings.Replace(
			validOpenAIComputerCallPayload(),
			`"response_id":"`+openAIComputerContinuationTokenForTest+`",`,
			"",
			1,
		),
		strings.Replace(
			validOpenAIComputerCallPayload(),
			openAIComputerContinuationTokenForTest,
			"resp_2",
			1,
		),
		strings.Replace(validOpenAIComputerCallPayload(), `"pending_safety_checks":[`, `"pending_safety_checks":null,"ignored":[`, 1),
		strings.Replace(validOpenAIComputerCallPayload(), `"sc_nullable"`, `"sc_confirm"`, 1),
		strings.Replace(validOpenAIComputerCallPayload(), `"message":null`, `"message":null,"extra":true`, 1),
		strings.Replace(validOpenAIComputerCallPayload(), `{"type":"type","text":"penguin"}`, `{"type":"future_action","value":1}`, 1),
		strings.Replace(validOpenAIComputerCallPayload(), `"actions":[`, `"actions":[],"ignored":[`, 1),
	}
	for _, payload := range payloads {
		if _, err := DecodeOpenAIComputerCallV1([]byte(payload)); err == nil {
			t.Fatalf("payload unexpectedly accepted: %s", payload)
		}
	}
}

func TestDecodeOpenAIComputerCallV1RejectsDuplicateMembersRecursively(t *testing.T) {
	payloads := []string{
		strings.Replace(
			validOpenAIComputerCallPayload(),
			`"tool_contract":"openai.computer.v1"`,
			`"tool_contract":"openai.computer.v1","tool_contract":"openai.computer.v1"`,
			1,
		),
		strings.Replace(
			validOpenAIComputerCallPayload(),
			`"x":405,"y":157`,
			`"x":405,"x":406,"y":157`,
			1,
		),
		strings.Replace(
			validOpenAIComputerCallPayload(),
			`"x":405,"y":157`,
			`"x":405,"\u0078":406,"y":157`,
			1,
		),
	}
	for _, payload := range payloads {
		if _, err := DecodeOpenAIComputerCallV1([]byte(payload)); err == nil ||
			!strings.Contains(err.Error(), "duplicate JSON") {
			t.Errorf("duplicate member was not rejected recursively: err=%v payload=%s", err, payload)
		}
	}
}

func TestDecodeOpenAIComputerCallV1StrictActionUnion(t *testing.T) {
	valid := []string{
		`{"type":"click","button":"right","x":0,"y":1,"keys":["SHIFT"]}`,
		`{"type":"click","button":"wheel","x":0,"y":1}`,
		`{"type":"click","button":"back","x":0,"y":1}`,
		`{"type":"click","button":"forward","x":0,"y":1}`,
		`{"type":"double_click","x":2,"y":3,"keys":["CTRL"]}`,
		`{"type":"drag","path":[{"x":1,"y":2},{"x":3,"y":4}],"keys":["ALT"]}`,
		`{"type":"keypress","keys":["META","A","B","B","RETURN","F4"]}`,
		`{"type":"move","x":5,"y":6,"keys":["COMMAND"]}`,
		`{"type":"screenshot"}`,
		`{"type":"scroll","x":7,"y":8,"scroll_x":0,"scroll_y":-618,"keys":["SHIFT"]}`,
		`{"type":"type","text":"hello\nworld"}`,
		`{"type":"wait"}`,
	}
	for _, action := range valid {
		call, err := DecodeOpenAIComputerCallV1(
			[]byte(openAIComputerCallWithActions(action)),
		)
		if err != nil {
			t.Errorf("valid action rejected: %s: %v", action, err)
			continue
		}
		if len(call.Actions) != 1 {
			t.Errorf("decoded %d actions, want 1: %s", len(call.Actions), action)
		}
	}

	invalid := []string{
		`{"type":"click","button":"middle","x":0,"y":1}`,
		`{"type":"click","button":"left","x":-1,"y":1}`,
		`{"type":"click","button":"left","x":1,"y":1,"keys":["A"]}`,
		`{"type":"click","button":"left","x":1,"y":1,"keys":["SHIFT","SHIFT"]}`,
		`{"type":"double_click","x":2}`,
		`{"type":"drag","path":[{"x":1,"y":2}]}`,
		`{"type":"drag","path":[{"x":1,"y":2},{"x":1,"y":2}]}`,
		`{"type":"keypress","keys":[]}`,
		`{"type":"keypress","keys":["SHIFT","SHIFT","A"]}`,
		`{"type":"keypress","keys":["CAPSLOCK","A"]}`,
		`{"type":"move","x":5,"y":6,"keys":["A"]}`,
		`{"type":"screenshot","x":0}`,
		`{"type":"scroll","x":7,"y":8,"scroll_x":0,"scroll_y":0}`,
		`{"type":"scroll","x":7,"y":8,"scroll_x":-2147483648,"scroll_y":1}`,
		`{"type":"type","text":""}`,
		`{"type":"wait","duration":2}`,
	}
	for _, action := range invalid {
		if _, err := DecodeOpenAIComputerCallV1(
			[]byte(openAIComputerCallWithActions(action)),
		); err == nil {
			t.Errorf("invalid action accepted: %s", action)
		}
	}
}

func TestOpenAIComputerKeypressAdmitsDriverFunctionKeys(t *testing.T) {
	keys := []string{
		"F1", "F2", "F3", "F4", "F5", "F6",
		"F7", "F8", "F9", "F10", "F11", "F12",
	}
	action, err := decodeOpenAIComputerActionV1(json.RawMessage(
		`{"type":"keypress","keys":` + mustJSONValueForOpenAIComputerTest(t, keys) + `}`,
	))
	if err != nil {
		t.Fatalf("function-key action: %v", err)
	}
	want := []string{
		"f1", "f2", "f3", "f4", "f5", "f6",
		"f7", "f8", "f9", "f10", "f11", "f12",
	}
	if !reflect.DeepEqual(action.Keys, want) {
		t.Fatalf("normalized function keys = %v, want %v", action.Keys, want)
	}
}

func mustJSONValueForOpenAIComputerTest(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestDecodeOpenAIComputerCallV1CanonicalizesOfficialKeyAliases(t *testing.T) {
	call, err := DecodeOpenAIComputerCallV1([]byte(openAIComputerCallWithActions(
		`{"type":"keypress","keys":["META","CTRL","ALT","SHIFT","ENTER","ESC","TAB","DEL","BACKSPACE","HOME","END","PAGEUP","PAGEDOWN","ARROWUP","ARROWDOWN","ARROWLEFT","ARROWRIGHT","SPACE","A"]},` +
			`{"type":"click","button":"left","x":1,"y":2,"keys":["CMD","OPTION"]}`,
	)))
	if err != nil {
		t.Fatalf("DecodeOpenAIComputerCallV1: %v", err)
	}
	wantKeypress := []string{
		"command", "control", "option", "shift", "return", "escape", "tab",
		"delete", "backspace", "home", "end", "pageup", "pagedown",
		"up", "down", "left", "right", "space", "a",
	}
	if !reflect.DeepEqual(call.Actions[0].Keys, wantKeypress) {
		t.Fatalf("keypress keys = %#v, want %#v", call.Actions[0].Keys, wantKeypress)
	}
	if want := []string{"command", "option"}; !reflect.DeepEqual(call.Actions[1].Keys, want) {
		t.Fatalf("click modifiers = %#v, want %#v", call.Actions[1].Keys, want)
	}
	if err := ValidateOpenAIComputerCallV1(call); err != nil {
		t.Fatalf("normalized typed representation rejected: %v", err)
	}
}

func TestOpenAIComputerAdapterRejectsUnsafeLaterPixelScrollBeforeEarlierMutation(t *testing.T) {
	executor := &openAIComputerExecutorProbe{finalResult: finalOpenAIComputerObservation()}
	adapter := newOpenAIComputerAdapterV1(executor)
	result, err := adapter.ExecuteBatchV1(
		context.Background(),
		[]byte(openAIComputerCallWithActions(
			`{"type":"click","button":"left","x":1,"y":2},`+
				`{"type":"scroll","x":7,"y":8,"scroll_x":-2147483648,"scroll_y":1}`,
		)),
	)
	if err != nil {
		t.Fatalf("ExecuteBatchV1: %v", err)
	}
	if !result.ToolResult.IsError ||
		result.ToolResult.ErrorCategory != agent.ErrCategoryValidation {
		t.Fatalf("tool result = %#v", result.ToolResult)
	}
	if executor.authorityCalls != 0 || len(executor.actions) != 0 ||
		executor.finalCalls != 0 {
		t.Fatalf("unsafe later scroll reached execution: %+v", executor)
	}
}

func TestDecodeOpenAIComputerCallV1RejectsDragBeyondStrictHelperWaypointBudget(t *testing.T) {
	points := make([]string, 0, coordinateDragMaximumWaypointsV1+1)
	for index := 0; index <= coordinateDragMaximumWaypointsV1; index++ {
		points = append(points, `{"x":`+strconv.Itoa(index)+`,"y":1}`)
	}
	action := `{"type":"drag","path":[` + strings.Join(points, ",") + `]}`
	if _, err := DecodeOpenAIComputerCallV1(
		[]byte(openAIComputerCallWithActions(action)),
	); err == nil {
		t.Fatal("provider drag exceeding strict helper event budget was accepted")
	}
}

func TestOpenAIComputerAdapterExecutesOrderedBatchAsOneResult(t *testing.T) {
	executor := &openAIComputerExecutorProbe{
		executions: []OpenAIComputerActionExecutionV1{
			{CommitState: OpenAIComputerCommitVerifiedV1},
			{CommitState: OpenAIComputerCommitVerifiedV1},
		},
		finalResult: finalOpenAIComputerObservation(),
	}
	adapter := newOpenAIComputerAdapterV1(executor)
	result, err := adapter.ExecuteBatchV1(
		context.Background(),
		[]byte(validOpenAIComputerCallPayload()),
	)
	if err != nil {
		t.Fatalf("ExecuteBatchV1: %v", err)
	}
	if result.CallID != "call_002" ||
		result.Provider != OpenAIComputerProviderV1 ||
		result.APISurface != client.APISurfaceOpenAIResponses ||
		result.ToolContract != client.ToolContractOpenAIComputerV1 {
		t.Fatalf("result provenance = %#v", result)
	}
	if result.ToolResult.IsError || len(result.ToolResult.Images) != 1 {
		t.Fatalf("tool result = %#v", result.ToolResult)
	}
	if len(executor.actions) != 2 ||
		executor.actions[0].Type != OpenAIComputerActionClickV1 ||
		executor.actions[1].Type != OpenAIComputerActionTypeTextV1 {
		t.Fatalf("execution order = %#v", executor.actions)
	}
	if executor.finalCalls != 1 {
		t.Fatalf("final capture calls = %d, want 1", executor.finalCalls)
	}
	if executor.authorityCalls != 1 {
		t.Fatalf("batch authority calls = %d, want 1", executor.authorityCalls)
	}
	for index, authority := range executor.authorities {
		if authority.LeaseID != "cul_batch" ||
			authority.CallID != "call_002" ||
			authority.APISurface != client.APISurfaceOpenAIResponses ||
			authority.ToolContract != client.ToolContractOpenAIComputerV1 {
			t.Fatalf("authority[%d] = %#v", index, authority)
		}
	}
	wantActionIDs := []string{"call_002/action/1", "call_002/action/2"}
	for index, scope := range executor.scopes {
		if scope.CallID != "call_002" ||
			scope.Provider != OpenAIComputerProviderV1 ||
			scope.APISurface != client.APISurfaceOpenAIResponses ||
			scope.ToolContract != client.ToolContractOpenAIComputerV1 ||
			scope.ActionIndex != index ||
			scope.ActionCount != 2 ||
			scope.ActionID != wantActionIDs[index] {
			t.Fatalf("scope[%d] = %#v", index, scope)
		}
	}
}

func TestOpenAIComputerAdapterRejectsWholeCallBeforeAuthorityOnUnknownAction(t *testing.T) {
	executor := &openAIComputerExecutorProbe{finalResult: finalOpenAIComputerObservation()}
	adapter := newOpenAIComputerAdapterV1(executor)
	result, err := adapter.ExecuteBatchV1(
		context.Background(),
		[]byte(strings.Replace(
			validOpenAIComputerCallPayload(),
			`{"type":"type","text":"penguin"}`,
			`{"type":"future_action","text":"penguin"}`,
			1,
		)),
	)
	if err != nil {
		t.Fatalf("ExecuteBatchV1: %v", err)
	}
	if !result.ToolResult.IsError ||
		result.ToolResult.ErrorCategory != agent.ErrCategoryValidation {
		t.Fatalf("tool result = %#v", result.ToolResult)
	}
	if executor.authorityCalls != 0 || len(executor.actions) != 0 || executor.finalCalls != 0 {
		t.Fatalf(
			"invalid call reached authority/execution/final capture: authority=%d actions=%d final=%d",
			executor.authorityCalls,
			len(executor.actions),
			executor.finalCalls,
		)
	}
}

func TestOpenAIComputerAdapterRejectsMismatchedBatchAuthorityBeforeAction(t *testing.T) {
	executor := &openAIComputerExecutorProbe{
		authority: OpenAIComputerBatchAuthorityV1{
			LeaseID: "cul_other", CallID: "call_other",
			Provider:     OpenAIComputerProviderV1,
			APISurface:   client.APISurfaceOpenAIResponses,
			ToolContract: client.ToolContractOpenAIComputerV1,
		},
		finalResult: finalOpenAIComputerObservation(),
	}
	adapter := newOpenAIComputerAdapterV1(executor)
	result, err := adapter.ExecuteBatchV1(
		context.Background(),
		[]byte(validOpenAIComputerCallPayload()),
	)
	if err != nil {
		t.Fatalf("ExecuteBatchV1: %v", err)
	}
	if !result.ToolResult.IsError ||
		!strings.Contains(result.ToolResult.Content, "batch authority") {
		t.Fatalf("tool result = %#v", result.ToolResult)
	}
	if len(executor.actions) != 0 || executor.finalCalls != 0 {
		t.Fatalf("mismatched authority reached execution: actions=%d final=%d",
			len(executor.actions), executor.finalCalls)
	}
}

func TestOpenAIComputerAdapterShortCircuitsAfterActionFailure(t *testing.T) {
	executor := &openAIComputerExecutorProbe{
		executions: []OpenAIComputerActionExecutionV1{
			{CommitState: OpenAIComputerCommitVerifiedV1},
			{CommitState: OpenAIComputerNotCommittedV1},
		},
		executionErrs: []error{nil, errors.New("action backend rejected input")},
		finalResult:   finalOpenAIComputerObservation(),
	}
	adapter := newOpenAIComputerAdapterV1(executor)
	result, err := adapter.ExecuteBatchV1(
		context.Background(),
		[]byte(strings.Replace(
			validOpenAIComputerCallPayload(),
			`{"type":"type","text":"penguin"}`,
			`{"type":"type","text":"penguin"},{"type":"keypress","keys":["ENTER"]}`,
			1,
		)),
	)
	if err != nil {
		t.Fatalf("ExecuteBatchV1: %v", err)
	}
	if !result.ToolResult.IsError ||
		!strings.Contains(result.ToolResult.Content, "action 2 of 3 (type)") {
		t.Fatalf("tool result = %#v", result.ToolResult)
	}
	if len(executor.actions) != 2 {
		t.Fatalf("executed %d actions, want 2", len(executor.actions))
	}
	if executor.finalCalls != 1 || len(result.ToolResult.Images) != 1 {
		t.Fatalf("final observation was not returned exactly once: calls=%d result=%#v",
			executor.finalCalls, result.ToolResult)
	}
}

func TestOpenAIComputerAdapterCancellationStopsAtActionBoundary(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	executor := &openAIComputerExecutorProbe{
		executions: []OpenAIComputerActionExecutionV1{
			{CommitState: OpenAIComputerCommitVerifiedV1},
		},
		finalResult: finalOpenAIComputerObservation(),
		afterAction: func(index int) {
			if index == 0 {
				cancel()
			}
		},
	}
	adapter := newOpenAIComputerAdapterV1(executor)
	result, err := adapter.ExecuteBatchV1(ctx, []byte(validOpenAIComputerCallPayload()))
	if err != nil {
		t.Fatalf("ExecuteBatchV1: %v", err)
	}
	if !result.ToolResult.IsError ||
		result.ToolResult.ErrorCategory != agent.ErrCategoryBusiness ||
		!strings.Contains(result.ToolResult.Content, "cancelled") {
		t.Fatalf("tool result = %#v", result.ToolResult)
	}
	if len(executor.actions) != 1 {
		t.Fatalf("executed %d actions after cancellation, want 1", len(executor.actions))
	}
	if executor.finalCalls != 0 {
		t.Fatalf("capture ignored cancelled context: %d calls", executor.finalCalls)
	}
}

func TestOpenAIComputerAdapterUnknownCommitNeverContinuesOrInvitesRetry(t *testing.T) {
	executor := &openAIComputerExecutorProbe{
		executions: []OpenAIComputerActionExecutionV1{{
			CommitState: OpenAIComputerCommitUnknownV1,
		}},
		executionErrs: []error{errors.New("helper connection lost")},
		finalResult:   finalOpenAIComputerObservation(),
	}
	adapter := newOpenAIComputerAdapterV1(executor)
	result, err := adapter.ExecuteBatchV1(
		context.Background(),
		[]byte(validOpenAIComputerCallPayload()),
	)
	if err != nil {
		t.Fatalf("ExecuteBatchV1: %v", err)
	}
	if !result.ToolResult.IsError ||
		!strings.Contains(result.ToolResult.Content, "commit status is unknown") ||
		!strings.Contains(result.ToolResult.Content, "helper connection lost") ||
		!strings.Contains(result.ToolResult.Content, "do not retry automatically") {
		t.Fatalf("tool result = %#v", result.ToolResult)
	}
	if len(executor.actions) != 1 {
		t.Fatalf("executed %d actions after unknown commit, want 1", len(executor.actions))
	}
	if executor.finalCalls != 1 || len(result.ToolResult.Images) != 1 {
		t.Fatalf("final observation = calls=%d result=%#v", executor.finalCalls, result.ToolResult)
	}
}

func TestOpenAIComputerAdapterContinuesFullyCommittedScrollAndDrag(t *testing.T) {
	for _, test := range []struct {
		name        string
		firstAction string
		failureCode string
	}{
		{
			name: "scroll",
			firstAction: `{"type":"scroll","x":7,"y":8,` +
				`"scroll_x":0,"scroll_y":-618}`,
			failureCode: "scroll_postcondition_not_declared",
		},
		{
			name: "drag",
			firstAction: `{"type":"drag","path":[` +
				`{"x":1,"y":2},{"x":3,"y":4}]}`,
			failureCode: "drop_postcondition_not_declared",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor := &openAIComputerExecutorProbe{
				executions: []OpenAIComputerActionExecutionV1{
					{
						CommitState: OpenAIComputerCommitUnverifiedV1,
						Result: agent.ToolResult{GUIOutcome: &agent.GUIActionOutcome{
							Result:      agent.GUIActionResultCompletedUnverified,
							Phase:       agent.GUIActionPhaseVerifying,
							FailureCode: test.failureCode,
						}},
					},
					{CommitState: OpenAIComputerCommitVerifiedV1},
				},
				finalResult: finalOpenAIComputerObservation(),
			}
			result, err := newOpenAIComputerAdapterV1(executor).ExecuteBatchV1(
				context.Background(),
				[]byte(openAIComputerCallWithActions(
					test.firstAction+
						`,{"type":"click","button":"left","x":9,"y":10}`,
				)),
			)
			if err != nil || result.ToolResult.IsError ||
				len(executor.actions) != 2 || executor.finalCalls != 1 {
				t.Fatalf(
					"result=%+v actions=%d final=%d err=%v",
					result.ToolResult,
					len(executor.actions),
					executor.finalCalls,
					err,
				)
			}
		})
	}
}

func TestOpenAIComputerAdapterContinuesKeypressWhenOnlyPhysicalMonitoringWasLost(
	t *testing.T,
) {
	executor := &openAIComputerExecutorProbe{
		executions: []OpenAIComputerActionExecutionV1{
			{
				CommitState: OpenAIComputerCommitUnverifiedV1,
				Result: agent.ToolResult{GUIOutcome: &agent.GUIActionOutcome{
					Result:      agent.GUIActionResultCompletedUnverified,
					Phase:       agent.GUIActionPhaseVerifying,
					FailureCode: "interference_detection_unavailable",
				}},
			},
			{CommitState: OpenAIComputerCommitVerifiedV1},
		},
		finalResult: finalOpenAIComputerObservation(),
	}
	result, err := newOpenAIComputerAdapterV1(executor).ExecuteBatchV1(
		context.Background(),
		[]byte(openAIComputerCallWithActions(
			`{"type":"keypress","keys":["META","TAB"]},`+
				`{"type":"keypress","keys":["9"]}`,
		)),
	)
	if err != nil || result.ToolResult.IsError ||
		len(executor.actions) != 2 || executor.finalCalls != 1 {
		t.Fatalf(
			"result=%+v actions=%d final=%d err=%v",
			result.ToolResult,
			len(executor.actions),
			executor.finalCalls,
			err,
		)
	}
}

func TestOpenAIComputerAdapterObservesButDoesNotReplayDragAfterMonitoringLoss(
	t *testing.T,
) {
	executor := &openAIComputerExecutorProbe{
		executions: []OpenAIComputerActionExecutionV1{
			{
				CommitState: OpenAIComputerCommitUnverifiedV1,
				Result: agent.ToolResult{GUIOutcome: &agent.GUIActionOutcome{
					Result:      agent.GUIActionResultCompletedUnverified,
					Phase:       agent.GUIActionPhaseVerifying,
					FailureCode: "interference_detection_unavailable",
				}},
			},
			{CommitState: OpenAIComputerCommitVerifiedV1},
		},
		finalResult: finalOpenAIComputerObservation(),
	}
	result, err := newOpenAIComputerAdapterV1(executor).ExecuteBatchV1(
		context.Background(),
		[]byte(openAIComputerCallWithActions(
			`{"type":"drag","path":[{"x":1,"y":2},{"x":3,"y":4}]},`+
				`{"type":"wait"}`,
		)),
	)
	if err != nil || !result.ToolResult.IsError ||
		len(executor.actions) != 1 || executor.finalCalls != 1 ||
		len(result.ToolResult.Images) != 1 {
		t.Fatalf(
			"result=%+v actions=%d final=%d err=%v",
			result.ToolResult,
			len(executor.actions),
			executor.finalCalls,
			err,
		)
	}
}

func TestOpenAIComputerAdapterStopsAfterCommittedActionWhenTargetRefreshFails(
	t *testing.T,
) {
	executor := &openAIComputerExecutorProbe{
		executions: []OpenAIComputerActionExecutionV1{
			{
				CommitState: OpenAIComputerCommitUnverifiedV1,
				Result: agent.ToolResult{GUIOutcome: &agent.GUIActionOutcome{
					Result:      agent.GUIActionResultCompletedUnverified,
					Phase:       agent.GUIActionPhaseVerifying,
					FailureCode: "postcondition_not_declared",
				}},
			},
			{CommitState: OpenAIComputerCommitVerifiedV1},
		},
		executionErrs: []error{
			errors.New("target refresh failed after the committed keypress"),
		},
		finalResult: finalOpenAIComputerObservation(),
	}
	result, err := newOpenAIComputerAdapterV1(executor).ExecuteBatchV1(
		context.Background(),
		[]byte(openAIComputerCallWithActions(
			`{"type":"keypress","keys":["META","TAB"]},`+
				`{"type":"keypress","keys":["9"]}`,
		)),
	)
	if err != nil || !result.ToolResult.IsError ||
		!strings.Contains(result.ToolResult.Content, "target refresh failed") ||
		len(executor.actions) != 1 || executor.finalCalls != 1 {
		t.Fatalf(
			"result=%+v actions=%d final=%d err=%v",
			result.ToolResult,
			len(executor.actions),
			executor.finalCalls,
			err,
		)
	}
}

func TestOpenAIComputerAdapterStopsPartiallyCommittedScroll(t *testing.T) {
	executor := &openAIComputerExecutorProbe{
		executions: []OpenAIComputerActionExecutionV1{{
			CommitState: OpenAIComputerCommitUnverifiedV1,
			Result: agent.ToolResult{GUIOutcome: &agent.GUIActionOutcome{
				Result:      agent.GUIActionResultCompletedUnverified,
				Phase:       agent.GUIActionPhaseInputCommitted,
				FailureCode: "scroll_not_committed",
			}},
		}},
		finalResult: finalOpenAIComputerObservation(),
	}
	result, err := newOpenAIComputerAdapterV1(executor).ExecuteBatchV1(
		context.Background(),
		[]byte(openAIComputerCallWithActions(
			`{"type":"scroll","x":7,"y":8,"scroll_x":0,"scroll_y":-618},`+
				`{"type":"click","button":"left","x":9,"y":10}`,
		)),
	)
	if err != nil || !result.ToolResult.IsError ||
		len(executor.actions) != 1 || executor.finalCalls != 1 {
		t.Fatalf(
			"result=%+v actions=%d final=%d err=%v",
			result.ToolResult,
			len(executor.actions),
			executor.finalCalls,
			err,
		)
	}
}

func TestOpenAIComputerAdapterRejectsPerActionImagesAndInvalidAcknowledgement(t *testing.T) {
	executor := &openAIComputerExecutorProbe{
		executions: []OpenAIComputerActionExecutionV1{{
			CommitState: OpenAIComputerCommitVerifiedV1,
			Result: agent.ToolResult{
				Images: []agent.ImageBlock{{MediaType: "image/png", Data: "intermediate"}},
			},
		}},
		finalResult: finalOpenAIComputerObservation(),
	}
	adapter := newOpenAIComputerAdapterV1(executor)
	result, err := adapter.ExecuteBatchV1(
		context.Background(),
		[]byte(validOpenAIComputerCallPayload()),
	)
	if err != nil {
		t.Fatalf("ExecuteBatchV1: %v", err)
	}
	if !result.ToolResult.IsError ||
		!strings.Contains(result.ToolResult.Content, "invalid per-action acknowledgement") {
		t.Fatalf("tool result = %#v", result.ToolResult)
	}
	if len(executor.actions) != 1 || executor.finalCalls != 1 {
		t.Fatalf("actions=%d final_calls=%d", len(executor.actions), executor.finalCalls)
	}
}

func TestOpenAINativeComputerSelectionIsAvailableAfterDaemonAuthorityCoverage(t *testing.T) {
	if !OpenAINativeComputerBatchExecutionAvailable() {
		t.Fatal("OpenAI native profile remained disabled after daemon per-action authority coverage")
	}
}
