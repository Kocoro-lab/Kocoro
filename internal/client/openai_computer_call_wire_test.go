package client

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const (
	openAIContinuationTokenPrimary   = "shct_pOIBMOn2gmZdU7TJZm93xdhEM1SNRTRle-n9A0mz76g"
	openAIContinuationTokenSecondary = "shct_WYl9Jlo4RkeE9sVHGA7GZlqr8_ZVa19Mg_gFbemKI_E"

	normalizedOpenAIComputerCallFixture = `{"type":"computer_call","provider":"openai","api_surface":"openai_responses","tool_contract":"openai.computer.v1","response_id":"` + openAIContinuationTokenPrimary + `","call_id":"call_001","actions":[{"type":"click","button":"left","x":405,"y":157},{"type":"type","text":"penguin"}],"pending_safety_checks":[],"status":"completed"}`
)

func requireJSONSemanticEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var gotValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("decode got JSON: %v\n%s", err, got)
	}
	var wantValue any
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("decode want JSON: %v\n%s", err, want)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON semantic drift:\n got: %s\nwant: %s", got, want)
	}
}

func TestValidOpenAIComputerContinuationTokenRequiresExactOpaqueFormat(t *testing.T) {
	for _, value := range []string{
		openAIContinuationTokenPrimary,
		openAIContinuationTokenSecondary,
	} {
		if !ValidOpenAIComputerContinuationToken(value) {
			t.Errorf("valid continuation token rejected: %q", value)
		}
	}
	for _, value := range []string{
		"",
		"resp_001",
		"shct_",
		openAIContinuationTokenPrimary + "a",
		openAIContinuationTokenPrimary[:len(openAIContinuationTokenPrimary)-1],
		"shct_pOIBMOn2gmZdU7TJZm93xdhEM1SNRTRle-n9A0mz76=",
	} {
		if ValidOpenAIComputerContinuationToken(value) {
			t.Errorf("invalid continuation token accepted: %q", value)
		}
	}
}

func TestContentBlockPreservesNormalizedOpenAIComputerCallRoundTrip(t *testing.T) {
	var block ContentBlock
	if err := json.Unmarshal([]byte(normalizedOpenAIComputerCallFixture), &block); err != nil {
		t.Fatalf("decode normalized computer_call: %v", err)
	}
	call, err := block.NormalizedOpenAIComputerCall()
	if err != nil {
		t.Fatalf("extract normalized computer_call: %v", err)
	}
	if call.CallID != "call_001" ||
		string(call.Actions) != `[{"type":"click","button":"left","x":405,"y":157},{"type":"type","text":"penguin"}]` {
		t.Fatalf("normalized computer_call = %#v", call)
	}
	encoded, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("encode normalized computer_call: %v", err)
	}
	requireJSONSemanticEqual(t, encoded, []byte(normalizedOpenAIComputerCallFixture))
}

func TestContentBlockPreservesOpenAIComputerSafetyChecksExactly(t *testing.T) {
	const fixture = `{"type":"computer_call","provider":"openai","api_surface":"openai_responses","tool_contract":"openai.computer.v1","response_id":"` + openAIContinuationTokenSecondary + `","call_id":"call_safe","actions":[{"type":"click","button":"left","x":405,"y":157}],"pending_safety_checks":[{"id":"check_1","code":"malicious_instructions","message":"Confirm the direct user request."},{"id":"check_2","code":null,"message":null}],"status":"completed"}`
	var block ContentBlock
	if err := json.Unmarshal([]byte(fixture), &block); err != nil {
		t.Fatalf("decode normalized computer_call safety checks: %v", err)
	}
	call, err := block.NormalizedOpenAIComputerCall()
	if err != nil {
		t.Fatalf("extract normalized computer_call safety checks: %v", err)
	}
	if len(call.PendingSafetyChecks) != 2 ||
		call.PendingSafetyChecks[0].ID != "check_1" ||
		call.PendingSafetyChecks[0].Code == nil ||
		*call.PendingSafetyChecks[0].Code != "malicious_instructions" ||
		call.PendingSafetyChecks[1].Code != nil ||
		call.PendingSafetyChecks[1].Message != nil {
		t.Fatalf("pending safety checks = %#v", call.PendingSafetyChecks)
	}
	encoded, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("encode normalized computer_call safety checks: %v", err)
	}
	requireJSONSemanticEqual(t, encoded, []byte(fixture))
}

func TestToolResultPreservesAcknowledgedOpenAIComputerSafetyChecks(t *testing.T) {
	const fixture = `{"type":"tool_result","tool_use_id":"call_safe","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"image"}}],"acknowledged_safety_checks":[{"id":"check_1","code":"malicious_instructions","message":"Confirm the direct user request."},{"id":"check_2","code":null,"message":null}]}`
	var block ContentBlock
	if err := json.Unmarshal([]byte(fixture), &block); err != nil {
		t.Fatalf("decode acknowledged safety checks: %v", err)
	}
	if len(block.AcknowledgedSafetyChecks) != 2 ||
		block.AcknowledgedSafetyChecks[0].ID != "check_1" ||
		block.AcknowledgedSafetyChecks[1].Code != nil {
		t.Fatalf(
			"acknowledged safety checks = %#v",
			block.AcknowledgedSafetyChecks,
		)
	}
	encoded, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("encode acknowledged safety checks: %v", err)
	}
	requireJSONSemanticEqual(t, encoded, []byte(fixture))
}

func TestContentBlockRejectsIncompleteOrMismatchedOpenAIComputerCall(t *testing.T) {
	tests := map[string]string{
		"missing provider": strings.Replace(
			normalizedOpenAIComputerCallFixture,
			`"provider":"openai",`,
			"",
			1,
		),
		"wrong provider": strings.Replace(
			normalizedOpenAIComputerCallFixture,
			`"provider":"openai"`,
			`"provider":"anthropic"`,
			1,
		),
		"wrong api surface": strings.Replace(
			normalizedOpenAIComputerCallFixture,
			`"api_surface":"openai_responses"`,
			`"api_surface":"openai_chat_completions"`,
			1,
		),
		"wrong tool contract": strings.Replace(
			normalizedOpenAIComputerCallFixture,
			`"tool_contract":"openai.computer.v1"`,
			`"tool_contract":"kocoro.computer_use.v1"`,
			1,
		),
		"missing response id": strings.Replace(
			normalizedOpenAIComputerCallFixture,
			`"response_id":"`+openAIContinuationTokenPrimary+`",`,
			"",
			1,
		),
		"raw upstream response id": strings.Replace(
			normalizedOpenAIComputerCallFixture,
			`"response_id":"`+openAIContinuationTokenPrimary+`"`,
			`"response_id":"resp_001"`,
			1,
		),
		"missing call id": strings.Replace(
			normalizedOpenAIComputerCallFixture,
			`"call_id":"call_001",`,
			"",
			1,
		),
		"malformed call id": strings.Replace(
			normalizedOpenAIComputerCallFixture,
			`"call_id":"call_001"`,
			`"call_id":"tool_001"`,
			1,
		),
		"nonterminal status": strings.Replace(
			normalizedOpenAIComputerCallFixture,
			`"status":"completed"`,
			`"status":"in_progress"`,
			1,
		),
		"empty actions": strings.Replace(
			normalizedOpenAIComputerCallFixture,
			`[{"type":"click","button":"left","x":405,"y":157},{"type":"type","text":"penguin"}]`,
			`[]`,
			1,
		),
		"missing pending safety checks": strings.Replace(
			normalizedOpenAIComputerCallFixture,
			`"pending_safety_checks":[],`,
			"",
			1,
		),
		"duplicate pending safety check id": strings.Replace(
			normalizedOpenAIComputerCallFixture,
			`"pending_safety_checks":[]`,
			`"pending_safety_checks":[{"id":"check_1","code":null,"message":null},{"id":"check_1","code":null,"message":null}]`,
			1,
		),
		"missing safety code member": strings.Replace(
			normalizedOpenAIComputerCallFixture,
			`"pending_safety_checks":[]`,
			`"pending_safety_checks":[{"id":"check_1","message":null}]`,
			1,
		),
		"unknown safety member": strings.Replace(
			normalizedOpenAIComputerCallFixture,
			`"pending_safety_checks":[]`,
			`"pending_safety_checks":[{"id":"check_1","code":null,"message":null,"extra":true}]`,
			1,
		),
		"unknown member": strings.Replace(
			normalizedOpenAIComputerCallFixture,
			`"status":"completed"`,
			`"status":"completed","extra":true`,
			1,
		),
		"duplicate call id": strings.Replace(
			normalizedOpenAIComputerCallFixture,
			`"call_id":"call_001"`,
			`"call_id":"call_001","call_id":"call_002"`,
			1,
		),
	}

	for name, fixture := range tests {
		t.Run(name, func(t *testing.T) {
			var block ContentBlock
			if err := json.Unmarshal([]byte(fixture), &block); err == nil {
				t.Fatalf("invalid normalized computer_call was accepted: %s", fixture)
			}
		})
	}
}

func TestOrdinaryAndAnthropicContentBlocksKeepExistingWireShape(t *testing.T) {
	const fixture = `[
		{"type":"text","text":"hello"},
		{"type":"thinking","thinking":"private","signature":"sig"},
		{"type":"tool_use","id":"tool_1","name":"computer","input":{"action":"screenshot"}},
		{"type":"tool_result","tool_use_id":"tool_1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"image"}}]}
	]`
	var blocks []ContentBlock
	if err := json.Unmarshal([]byte(fixture), &blocks); err != nil {
		t.Fatalf("decode ordinary/Anthropic blocks: %v", err)
	}
	encoded, err := json.Marshal(blocks)
	if err != nil {
		t.Fatalf("encode ordinary/Anthropic blocks: %v", err)
	}
	requireJSONSemanticEqual(t, encoded, []byte(fixture))
}

func TestFunctionCallPreservesOpenAIComputerCallProvenance(t *testing.T) {
	const fixture = `{"id":"call_001","call_id":"call_001","name":"computer","arguments":{"actions":[{"type":"click","button":"left","x":405,"y":157}],"response_id":"` + openAIContinuationTokenPrimary + `","pending_safety_checks":[]},"type":"computer_call","provider":"openai","api_surface":"openai_responses","tool_contract":"openai.computer.v1","response_id":"` + openAIContinuationTokenPrimary + `","pending_safety_checks":[]}`
	var call FunctionCall
	if err := json.Unmarshal([]byte(fixture), &call); err != nil {
		t.Fatalf("decode normalized function call: %v", err)
	}
	encoded, err := json.Marshal(call)
	if err != nil {
		t.Fatalf("encode normalized function call: %v", err)
	}
	requireJSONSemanticEqual(t, encoded, []byte(fixture))
}

func TestCompletionRequestPreservesPreviousResponseIDOnWire(t *testing.T) {
	var request CompletionRequest
	if err := json.Unmarshal(
		[]byte(`{"messages":[{"role":"user","content":"continue"}],"previous_response_id":"`+openAIContinuationTokenPrimary+`"}`),
		&request,
	); err != nil {
		t.Fatalf("decode completion request fixture: %v", err)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal completion request: %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatalf("decode completion request: %v", err)
	}
	var got string
	if err := json.Unmarshal(wire["previous_response_id"], &got); err != nil {
		t.Fatalf("decode previous_response_id: %v; body=%s", err, encoded)
	}
	if got != openAIContinuationTokenPrimary {
		t.Fatalf(
			"previous_response_id = %q, want %q",
			got,
			openAIContinuationTokenPrimary,
		)
	}
}
