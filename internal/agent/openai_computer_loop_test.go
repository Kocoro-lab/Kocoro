package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

type openAIComputerLoopLLM struct {
	mu        sync.Mutex
	responses []*client.CompletionResponse
	errors    []error
	requests  []client.CompletionRequest
}

type openAIComputerSchemaProbeTool struct {
	name   string
	native bool
}

func (t *openAIComputerSchemaProbeTool) Info() ToolInfo {
	return ToolInfo{
		Name:       t.name,
		Parameters: map[string]any{"type": "object"},
	}
}

func (*openAIComputerSchemaProbeTool) RequiresApproval() bool { return false }

func (*openAIComputerSchemaProbeTool) Run(
	context.Context,
	string,
) (ToolResult, error) {
	return ToolResult{}, nil
}

func (t *openAIComputerSchemaProbeTool) NativeToolDef() *client.NativeToolDef {
	if !t.native {
		return nil
	}
	return &client.NativeToolDef{
		Type: "computer",
		Name: client.NativeComputerToolName,
	}
}

func (l *openAIComputerLoopLLM) Complete(
	_ context.Context,
	req client.CompletionRequest,
) (*client.CompletionResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.requests = append(l.requests, req)
	if len(l.errors) > 0 {
		err := l.errors[0]
		l.errors = l.errors[1:]
		if err != nil {
			return nil, err
		}
	}
	if len(l.responses) == 0 {
		return nil, errors.New("unexpected extra completion request")
	}
	response := l.responses[0]
	l.responses = l.responses[1:]
	return response, nil
}

func (l *openAIComputerLoopLLM) CompleteStream(
	ctx context.Context,
	req client.CompletionRequest,
	_ func(client.StreamDelta),
) (*client.CompletionResponse, error) {
	return l.Complete(ctx, req)
}

func (l *openAIComputerLoopLLM) capturedRequests() []client.CompletionRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]client.CompletionRequest(nil), l.requests...)
}

type openAIComputerLoopBatchExecutor struct {
	mu                    sync.Mutex
	executions            []OpenAIComputerBatchExecution
	executionErrs         []error
	calls                 []openAIComputerLoopBatchCall
	skipSafetyConsumption bool
}

type openAIComputerLoopBatchCall struct {
	ProfileID                 string
	ResponseID                string
	Payload                   json.RawMessage
	SafetyAcknowledgementSeen bool
	SafetyAcknowledgementOK   bool
}

func (e *openAIComputerLoopBatchExecutor) ExecuteOpenAIComputerBatch(
	_ context.Context,
	profile *client.ExecutionProfile,
	responseID string,
	payload json.RawMessage,
	safetyAcknowledgement *OpenAIComputerSafetyAcknowledgement,
) (OpenAIComputerBatchExecution, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	callRecord := openAIComputerLoopBatchCall{
		ProfileID:                 profile.ProfileID(),
		ResponseID:                responseID,
		Payload:                   append(json.RawMessage(nil), payload...),
		SafetyAcknowledgementSeen: safetyAcknowledgement != nil,
	}
	if safetyAcknowledgement != nil && !e.skipSafetyConsumption {
		var call client.OpenAIComputerCall
		if err := json.Unmarshal(payload, &call); err == nil {
			callRecord.SafetyAcknowledgementOK =
				safetyAcknowledgement.ConsumeForExecution(
					profile,
					responseID,
					call,
				)
		}
	}
	e.calls = append(e.calls, callRecord)
	if len(e.executionErrs) > 0 {
		err := e.executionErrs[0]
		e.executionErrs = e.executionErrs[1:]
		if err != nil {
			return OpenAIComputerBatchExecution{}, err
		}
	}
	if len(e.executions) == 0 {
		return OpenAIComputerBatchExecution{}, errors.New("unexpected extra batch execution")
	}
	execution := e.executions[0]
	e.executions = e.executions[1:]
	return execution, nil
}

func (e *openAIComputerLoopBatchExecutor) capturedCalls() []openAIComputerLoopBatchCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]openAIComputerLoopBatchCall(nil), e.calls...)
}

func openAIComputerLoopResponse(
	t *testing.T,
	profile *client.ExecutionProfile,
	responseID string,
	callJSON string,
) *client.CompletionResponse {
	t.Helper()
	var normalizedCall map[string]any
	if err := json.Unmarshal([]byte(callJSON), &normalizedCall); err != nil {
		t.Fatalf("decode test computer_call: %v", err)
	}
	normalizedCall["response_id"] = responseID
	if _, present := normalizedCall["pending_safety_checks"]; !present {
		normalizedCall["pending_safety_checks"] = []any{}
	}
	callWire, err := json.Marshal(normalizedCall)
	if err != nil {
		t.Fatalf("encode test computer_call: %v", err)
	}
	profileJSON, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	var response client.CompletionResponse
	payload := []byte(`{
		"provider":"openai",
		"model":` + mustJSONQuote(t, profile.Model()) + `,
		"request_id":` + mustJSONQuote(t, responseID) + `,
		"finish_reason":"tool_use",
		"execution_profile":` + string(profileJSON) + `,
		"content_blocks":[` + string(callWire) + `],
		"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}
	}`)
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatalf("decode OpenAI computer loop response: %v\n%s", err, payload)
	}
	call, err := response.ContentBlocks[0].NormalizedOpenAIComputerCall()
	if err != nil {
		t.Fatalf("normalize OpenAI computer loop call: %v", err)
	}
	arguments, err := json.Marshal(map[string]json.RawMessage{
		"actions":               call.Actions,
		"response_id":           json.RawMessage(mustJSONQuote(t, call.ResponseID)),
		"pending_safety_checks": mustJSONMarshal(t, call.PendingSafetyChecks),
	})
	if err != nil {
		t.Fatal(err)
	}
	alias := client.FunctionCall{
		ID:           call.CallID,
		CallID:       call.CallID,
		Name:         client.NativeComputerToolName,
		Arguments:    arguments,
		Type:         client.OpenAIComputerCallType,
		Provider:     client.OpenAIComputerProvider,
		APISurface:   client.APISurfaceOpenAIResponses,
		ToolContract: client.ToolContractOpenAIComputerV1,
		ResponseID:   call.ResponseID,
		PendingSafetyChecks: client.CloneOpenAIComputerSafetyChecks(
			call.PendingSafetyChecks,
		),
	}
	response.FunctionCall = &alias
	response.ToolCalls = []client.FunctionCall{alias}
	return &response
}

func openAIComputerLoopFinalResponse(
	t *testing.T,
	profile *client.ExecutionProfile,
	responseID string,
	text string,
) *client.CompletionResponse {
	t.Helper()
	profileJSON, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	var response client.CompletionResponse
	payload := []byte(`{
		"provider":"openai",
		"model":` + mustJSONQuote(t, profile.Model()) + `,
		"request_id":` + mustJSONQuote(t, responseID) + `,
		"finish_reason":"stop",
		"output_text":` + mustJSONQuote(t, text) + `,
		"execution_profile":` + string(profileJSON) + `,
		"content_blocks":[{"type":"text","text":` + mustJSONQuote(t, text) + `}],
		"usage":{"input_tokens":11,"output_tokens":4,"total_tokens":15}
	}`)
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatalf("decode OpenAI computer final response: %v\n%s", err, payload)
	}
	return &response
}

func mustJSONQuote(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func mustJSONMarshal(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestAgentLoopOpenAIComputerExecutesBatchAndContinuesResponsesTrajectory(t *testing.T) {
	profile := resolveTrustedOpenAIComputerProfile(t, "gpt-5.6-sol")
	llm := &openAIComputerLoopLLM{responses: []*client.CompletionResponse{
		openAIComputerLoopResponse(
			t,
			profile,
			openAIContinuationTokenPrimary,
			normalizedOpenAIComputerCallForTrajectory,
		),
		openAIComputerLoopFinalResponse(t, profile, "resp_loop_002", "done"),
	}}
	executor := &openAIComputerLoopBatchExecutor{
		executions: []OpenAIComputerBatchExecution{{
			CallID:              "call_001",
			ContinuationAllowed: true,
			Result: ToolResult{
				Content: "final observation",
				Images: []ImageBlock{{
					MediaType: "image/png",
					Data:      "c2NyZWVuc2hvdA==",
				}},
			},
		}},
	}
	loop := NewAgentLoop(
		llm,
		NewToolRegistry(),
		"medium",
		t.TempDir(),
		4,
		2000,
		200,
		nil,
		nil,
		nil,
	)
	loop.SetSkillDiscovery(false)
	loop.SetSpecificModel(profile.Model())
	loop.SetExecutionProfile(profile)
	loop.SetOpenAIComputerBatchExecutor(executor)
	loop.SetForceInitialToolUse(true)

	reply, _, err := loop.Run(context.Background(), "open the thread", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "done" {
		t.Fatalf("reply = %q, want done", reply)
	}

	batches := executor.capturedCalls()
	if len(batches) != 1 {
		t.Fatalf("batch calls = %d, want 1", len(batches))
	}
	if batches[0].ProfileID != profile.ProfileID() ||
		batches[0].ResponseID != openAIContinuationTokenPrimary {
		t.Fatalf("batch provenance = %+v", batches[0])
	}
	requireAgentJSONSemanticEqual(
		t,
		batches[0].Payload,
		[]byte(normalizedOpenAIComputerCallForTrajectory),
	)
	if !batches[0].SafetyAcknowledgementSeen ||
		!batches[0].SafetyAcknowledgementOK {
		t.Fatalf("batch safety acknowledgement = %+v", batches[0])
	}

	requests := llm.capturedRequests()
	if len(requests) != 2 {
		t.Fatalf("completion requests = %d, want 2", len(requests))
	}
	first, continuation := requests[0], requests[1]
	if first.PreviousResponseID != "" {
		t.Fatalf("initial previous_response_id = %q", first.PreviousResponseID)
	}
	if first.ExecutionProfileID != profile.ProfileID() ||
		first.ResolvedExecutionProfile != profile {
		t.Fatal("initial request lost trusted execution profile")
	}
	if first.ToolChoice != "any" {
		t.Fatalf("initial tool_choice = %#v, want any", first.ToolChoice)
	}
	if continuation.PreviousResponseID != openAIContinuationTokenPrimary {
		t.Fatalf(
			"continuation previous_response_id = %q, want %q",
			continuation.PreviousResponseID,
			openAIContinuationTokenPrimary,
		)
	}
	if continuation.ExecutionProfileID != profile.ProfileID() ||
		continuation.ResolvedExecutionProfile != profile {
		t.Fatal("continuation request lost trusted execution profile")
	}
	if continuation.ToolChoice != nil {
		t.Fatalf("continuation tool_choice = %#v, want auto", continuation.ToolChoice)
	}
	if len(continuation.Messages) != len(first.Messages)+2 {
		t.Fatalf(
			"continuation messages = %d, want initial %d + 2",
			len(continuation.Messages),
			len(first.Messages),
		)
	}
	assistant := continuation.Messages[len(continuation.Messages)-2]
	assistantBlocks := assistant.Content.Blocks()
	if assistant.Role != "assistant" || len(assistantBlocks) != 1 ||
		assistantBlocks[0].Type != client.OpenAIComputerCallType ||
		assistantBlocks[0].CallID != "call_001" {
		t.Fatalf("assistant computer_call trajectory = %#v", assistant)
	}
	output := continuation.Messages[len(continuation.Messages)-1]
	outputBlocks := output.Content.Blocks()
	if output.Role != "user" || len(outputBlocks) != 1 {
		t.Fatalf("computer_call_output message = %#v", output)
	}
	toolResult := outputBlocks[0]
	if toolResult.Type != "tool_result" ||
		toolResult.ToolUseID != "call_001" ||
		toolResult.IsError ||
		toolResult.AcknowledgedSafetyChecks != nil {
		t.Fatalf("computer_call_output block = %#v", toolResult)
	}
	nested, ok := toolResult.ToolContent.([]client.ContentBlock)
	if !ok || len(nested) != 1 ||
		nested[0].Type != "image" ||
		nested[0].Source == nil ||
		nested[0].Source.Data != "c2NyZWVuc2hvdA==" {
		t.Fatalf("computer_call_output content = %#v", toolResult.ToolContent)
	}
}

func TestAgentLoopOpenAINativeProfileExposesOnlyResponsesComputerSchema(t *testing.T) {
	profile := resolveTrustedOpenAIComputerProfile(t, "gpt-5.6-sol")
	llm := &openAIComputerLoopLLM{responses: []*client.CompletionResponse{
		openAIComputerLoopFinalResponse(t, profile, "resp_schema", "done"),
	}}
	registry := NewToolRegistry()
	registry.Register(&openAIComputerSchemaProbeTool{
		name: client.NativeComputerToolName, native: true,
	})
	registry.Register(&openAIComputerSchemaProbeTool{name: "computer_use"})
	registry.Register(&openAIComputerSchemaProbeTool{name: "bash"})
	loop := NewAgentLoop(
		llm,
		registry,
		"large",
		t.TempDir(),
		2,
		2000,
		200,
		nil,
		nil,
		nil,
	)
	loop.SetSkillDiscovery(false)
	loop.SetSpecificModel(profile.Model())
	loop.SetExecutionProfile(profile)

	reply, _, err := loop.Run(context.Background(), "inspect", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "done" {
		t.Fatalf("reply = %q", reply)
	}
	requests := llm.capturedRequests()
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(requests))
	}
	if got := requests[0].Tools; len(got) != 1 ||
		got[0].Type != "computer" ||
		got[0].Name != "" ||
		got[0].Function.Name != "" {
		t.Fatalf("OpenAI native schemas = %+v, want only {type:computer}", got)
	}
}

func TestAgentLoopOpenAIComputerRequiresExplicitSafetyConfirmation(t *testing.T) {
	profile := resolveTrustedOpenAIComputerProfile(t, "gpt-5.6-sol")
	const callWithSafety = `{"type":"computer_call","provider":"openai","api_surface":"openai_responses","tool_contract":"openai.computer.v1","response_id":"` + openAIContinuationTokenPrimary + `","call_id":"call_safe","actions":[{"type":"click","button":"left","x":10,"y":20}],"pending_safety_checks":[{"id":"check_1","code":"malicious_instructions","message":"Confirm the direct user request."}],"status":"completed"}`

	t.Run("confirmed exact checks continue", func(t *testing.T) {
		llm := &openAIComputerLoopLLM{responses: []*client.CompletionResponse{
			openAIComputerLoopResponse(
				t,
				profile,
				openAIContinuationTokenPrimary,
				callWithSafety,
			),
			openAIComputerLoopFinalResponse(
				t,
				profile,
				"resp_safe_final",
				"done",
			),
		}}
		executor := &openAIComputerLoopBatchExecutor{
			executions: []OpenAIComputerBatchExecution{{
				CallID:              "call_safe",
				ContinuationAllowed: true,
				Result: ToolResult{Images: []ImageBlock{{
					MediaType: "image/png",
					Data:      "c2NyZWVuc2hvdA==",
				}}},
			}},
		}
		handler := &mockHandler{approveResult: true}
		loop := newOpenAIComputerTestLoop(
			t,
			llm,
			profile,
			executor,
			handler,
		)

		reply, _, err := loop.Run(context.Background(), "inspect", nil, nil)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if reply != "done" || !handler.approvalRequested {
			t.Fatalf(
				"reply=%q approval_requested=%v",
				reply,
				handler.approvalRequested,
			)
		}
		requests := llm.capturedRequests()
		if len(requests) != 2 {
			t.Fatalf("completion requests = %d, want 2", len(requests))
		}
		result := requests[1].Messages[len(requests[1].Messages)-1].
			Content.Blocks()[0]
		if len(result.AcknowledgedSafetyChecks) != 1 ||
			result.AcknowledgedSafetyChecks[0].ID != "check_1" {
			t.Fatalf(
				"acknowledged safety checks = %#v",
				result.AcknowledgedSafetyChecks,
			)
		}
	})

	t.Run("denied checks never reach executor or provider continuation", func(t *testing.T) {
		llm := &openAIComputerLoopLLM{responses: []*client.CompletionResponse{
			openAIComputerLoopResponse(
				t,
				profile,
				openAIContinuationTokenPrimary,
				callWithSafety,
			),
		}}
		executor := &openAIComputerLoopBatchExecutor{}
		handler := &mockHandler{approveResult: false}
		loop := newOpenAIComputerTestLoop(
			t,
			llm,
			profile,
			executor,
			handler,
		)

		if _, _, err := loop.Run(
			context.Background(),
			"inspect",
			nil,
			nil,
		); err == nil {
			t.Fatal("denied safety checks were accepted")
		}
		if !handler.approvalRequested {
			t.Fatal("pending safety checks did not request confirmation")
		}
		if got := len(executor.capturedCalls()); got != 0 {
			t.Fatalf("denied safety checks executed %d batches", got)
		}
		if got := len(llm.capturedRequests()); got != 1 {
			t.Fatalf("denied safety checks issued %d model calls", got)
		}
	})

	t.Run("unconsumed acknowledgement cannot continue", func(t *testing.T) {
		llm := &openAIComputerLoopLLM{responses: []*client.CompletionResponse{
			openAIComputerLoopResponse(
				t,
				profile,
				openAIContinuationTokenPrimary,
				callWithSafety,
			),
		}}
		executor := &openAIComputerLoopBatchExecutor{
			skipSafetyConsumption: true,
			executions: []OpenAIComputerBatchExecution{{
				CallID:              "call_safe",
				ContinuationAllowed: true,
				Result: ToolResult{Images: []ImageBlock{{
					MediaType: "image/png",
					Data:      "c2NyZWVuc2hvdA==",
				}}},
			}},
		}
		loop := newOpenAIComputerTestLoop(
			t,
			llm,
			profile,
			executor,
			&mockHandler{approveResult: true},
		)

		if _, _, err := loop.Run(
			context.Background(),
			"inspect",
			nil,
			nil,
		); err == nil {
			t.Fatal("unconsumed safety acknowledgement continued")
		}
		if got := len(llm.capturedRequests()); got != 1 {
			t.Fatalf("unconsumed acknowledgement issued %d model calls", got)
		}
	})
}

func newOpenAIComputerTestLoop(
	t *testing.T,
	llm client.LLMClient,
	profile *client.ExecutionProfile,
	executor OpenAIComputerBatchExecutor,
	handler EventHandler,
) *AgentLoop {
	t.Helper()
	loop := NewAgentLoop(
		llm,
		NewToolRegistry(),
		"medium",
		t.TempDir(),
		4,
		2000,
		200,
		nil,
		nil,
		nil,
	)
	loop.SetSkillDiscovery(false)
	loop.SetSpecificModel(profile.Model())
	loop.SetExecutionProfile(profile)
	loop.SetOpenAIComputerBatchExecutor(executor)
	if handler != nil {
		loop.SetHandler(handler)
	}
	return loop
}

func TestAgentLoopOpenAIComputerContinuationCarriesOnlyLatestCallOutputPair(t *testing.T) {
	profile := resolveTrustedOpenAIComputerProfile(t, "gpt-5.6-sol")
	call1 := `{"type":"computer_call","provider":"openai","api_surface":"openai_responses","tool_contract":"openai.computer.v1","call_id":"call_round_1","actions":[{"type":"click","button":"left","x":1,"y":1}],"status":"completed"}`
	call2 := `{"type":"computer_call","provider":"openai","api_surface":"openai_responses","tool_contract":"openai.computer.v1","call_id":"call_round_2","actions":[{"type":"click","button":"left","x":2,"y":1}],"status":"completed"}`
	call3 := `{"type":"computer_call","provider":"openai","api_surface":"openai_responses","tool_contract":"openai.computer.v1","call_id":"call_round_3","actions":[{"type":"click","button":"left","x":3,"y":1}],"status":"completed"}`

	llm := &openAIComputerLoopLLM{responses: []*client.CompletionResponse{
		openAIComputerLoopResponse(t, profile, openAIContinuationTokenPrimary, call1),
		openAIComputerLoopResponse(t, profile, openAIContinuationTokenSecondary, call2),
		openAIComputerLoopResponse(t, profile, openAIContinuationTokenOther, call3),
		openAIComputerLoopFinalResponse(t, profile, "resp_round_4", "complete"),
	}}
	executor := &openAIComputerLoopBatchExecutor{
		executions: []OpenAIComputerBatchExecution{
			{
				CallID: "call_round_1", ContinuationAllowed: true,
				Result: ToolResult{Images: []ImageBlock{{
					MediaType: "image/png", Data: "cm91bmQx",
				}}},
			},
			{
				CallID: "call_round_2", ContinuationAllowed: true,
				Result: ToolResult{Images: []ImageBlock{{
					MediaType: "image/png", Data: "cm91bmQy",
				}}},
			},
			{
				CallID: "call_round_3", ContinuationAllowed: true,
				Result: ToolResult{Images: []ImageBlock{{
					MediaType: "image/png", Data: "cm91bmQz",
				}}},
			},
		},
	}
	loop := NewAgentLoop(
		llm,
		NewToolRegistry(),
		"medium",
		t.TempDir(),
		6,
		2000,
		200,
		nil,
		nil,
		nil,
	)
	loop.SetSkillDiscovery(false)
	loop.SetSpecificModel(profile.Model())
	loop.SetExecutionProfile(profile)
	loop.SetOpenAIComputerBatchExecutor(executor)

	reply, _, err := loop.Run(context.Background(), "perform three steps", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "complete" {
		t.Fatalf("reply = %q", reply)
	}

	requests := llm.capturedRequests()
	if len(requests) != 4 {
		t.Fatalf("completion requests = %d, want 4", len(requests))
	}
	wantCallIDs := []string{"call_round_1", "call_round_2", "call_round_3"}
	wantPreviousIDs := []string{
		openAIContinuationTokenPrimary,
		openAIContinuationTokenSecondary,
		openAIContinuationTokenOther,
	}
	for index, request := range requests[1:] {
		var callIDs []string
		var resultIDs []string
		for _, message := range request.Messages {
			for _, block := range message.Content.Blocks() {
				switch block.Type {
				case client.OpenAIComputerCallType:
					callIDs = append(callIDs, block.CallID)
				case "tool_result":
					resultIDs = append(resultIDs, block.ToolUseID)
				}
			}
		}
		if len(callIDs) != 1 || callIDs[0] != wantCallIDs[index] {
			t.Fatalf(
				"continuation %d computer_call ids = %v, want only %q",
				index+1,
				callIDs,
				wantCallIDs[index],
			)
		}
		if len(resultIDs) != 1 || resultIDs[0] != wantCallIDs[index] {
			t.Fatalf(
				"continuation %d result ids = %v, want only %q",
				index+1,
				resultIDs,
				wantCallIDs[index],
			)
		}
		if request.PreviousResponseID != wantPreviousIDs[index] {
			t.Fatalf(
				"continuation %d previous_response_id = %q, want %q",
				index+1,
				request.PreviousResponseID,
				wantPreviousIDs[index],
			)
		}
	}

	// Persistence keeps all three pairs even though each provider request is
	// trimmed to the latest pair.
	var persistedCalls int
	for _, message := range loop.RunMessages() {
		for _, block := range message.Content.Blocks() {
			if block.Type == client.OpenAIComputerCallType {
				persistedCalls++
			}
		}
	}
	if persistedCalls != 3 {
		t.Fatalf("persisted computer calls = %d, want 3", persistedCalls)
	}
}

func TestAgentLoopOpenAIComputerContinuationRetryReusesExactRequest(t *testing.T) {
	profile := resolveTrustedOpenAIComputerProfile(t, "gpt-5.6-sol")
	call := `{"type":"computer_call","provider":"openai","api_surface":"openai_responses","tool_contract":"openai.computer.v1","call_id":"call_retry_1","actions":[{"type":"click","button":"left","x":1,"y":1}],"status":"completed"}`
	llm := &openAIComputerLoopLLM{
		responses: []*client.CompletionResponse{
			openAIComputerLoopResponse(t, profile, openAIContinuationTokenPrimary, call),
			openAIComputerLoopFinalResponse(t, profile, "resp_retry_2", "complete"),
		},
		errors: []error{
			nil,
			&client.APIError{StatusCode: http.StatusInternalServerError, Body: "transient"},
			nil,
		},
	}
	executor := &openAIComputerLoopBatchExecutor{
		executions: []OpenAIComputerBatchExecution{{
			CallID: "call_retry_1", ContinuationAllowed: true,
			Result: ToolResult{Images: []ImageBlock{{
				MediaType: "image/png", Data: "cmV0cnk=",
			}}},
		}},
	}
	loop := newOpenAIComputerTestLoop(t, llm, profile, executor, nil)
	reply, _, err := loop.Run(context.Background(), "perform one step", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "complete" {
		t.Fatalf("reply=%q", reply)
	}
	requests := llm.capturedRequests()
	if len(requests) != 3 {
		t.Fatalf("completion requests=%d, want initial + failed continuation + retry", len(requests))
	}
	failed, retried := requests[1], requests[2]
	failedJSON, err := json.Marshal(failed)
	if err != nil {
		t.Fatal(err)
	}
	retriedJSON, err := json.Marshal(retried)
	if err != nil {
		t.Fatal(err)
	}
	if string(failedJSON) != string(retriedJSON) {
		t.Fatalf("continuation retry changed request:\nfailed=%s\nretried=%s",
			failedJSON, retriedJSON)
	}
	if retried.PreviousResponseID != openAIContinuationTokenPrimary {
		t.Fatalf("retry previous_response_id=%q", retried.PreviousResponseID)
	}
}

func TestAgentLoopOpenAIComputerExpiredContinuationRestartsFromVerifiedScreen(t *testing.T) {
	profile := resolveTrustedOpenAIComputerProfile(t, "gpt-5.6-sol")
	call := `{"type":"computer_call","provider":"openai","api_surface":"openai_responses","tool_contract":"openai.computer.v1","call_id":"call_expired_1","actions":[{"type":"click","button":"left","x":1,"y":1}],"status":"completed"}`
	llm := &openAIComputerLoopLLM{
		responses: []*client.CompletionResponse{
			openAIComputerLoopResponse(
				t,
				profile,
				openAIContinuationTokenPrimary,
				call,
			),
			openAIComputerLoopFinalResponse(
				t,
				profile,
				"resp_restarted",
				"continued safely",
			),
		},
		errors: []error{
			nil,
			&client.APIError{
				StatusCode: http.StatusConflict,
				Body: `{"error":{"type":"computer_continuation_expired",` +
					`"code":"invalid_request","message":"spent","status":409}}`,
			},
			nil,
		},
	}
	executor := &openAIComputerLoopBatchExecutor{
		executions: []OpenAIComputerBatchExecution{{
			CallID:              "call_expired_1",
			ContinuationAllowed: true,
			Result: ToolResult{Images: []ImageBlock{{
				MediaType: "image/png",
				Data:      "dmVyaWZpZWQ=",
			}}},
		}},
	}
	loop := newOpenAIComputerTestLoop(t, llm, profile, executor, nil)

	reply, _, err := loop.Run(
		context.Background(),
		"click once, then continue from the result",
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply != "continued safely" {
		t.Fatalf("reply=%q", reply)
	}
	if got := len(executor.capturedCalls()); got != 1 {
		t.Fatalf("expired continuation replayed %d computer batches, want 1", got)
	}

	requests := llm.capturedRequests()
	if len(requests) != 3 {
		t.Fatalf(
			"completion requests=%d, want initial + expired continuation + fresh restart",
			len(requests),
		)
	}
	if requests[1].PreviousResponseID != openAIContinuationTokenPrimary {
		t.Fatalf(
			"expired continuation previous_response_id=%q",
			requests[1].PreviousResponseID,
		)
	}
	restarted := requests[2]
	if restarted.PreviousResponseID != "" {
		t.Fatalf(
			"restarted previous_response_id=%q, want fresh trajectory",
			restarted.PreviousResponseID,
		)
	}
	var restartText string
	var restartImages []string
	for _, message := range restarted.Messages {
		for _, block := range message.Content.Blocks() {
			switch block.Type {
			case client.OpenAIComputerCallType:
				t.Fatalf("fresh restart resent expired computer_call: %#v", block)
			case "tool_result":
				if block.ToolUseID == "call_expired_1" {
					t.Fatalf("fresh restart resent expired tool_result: %#v", block)
				}
			case "text":
				restartText += block.Text
			case "image":
				if block.Source != nil {
					restartImages = append(restartImages, block.Source.Data)
				}
			}
		}
	}
	if !strings.Contains(restartText, "Do not repeat") ||
		!strings.Contains(restartText, "click once, then continue from the result") {
		t.Fatalf("restart guidance=%q", restartText)
	}
	if len(restartImages) != 1 || restartImages[0] != "dmVyaWZpZWQ=" {
		t.Fatalf("restart images=%v, want verified terminal screenshot", restartImages)
	}
}

func TestAgentLoopFreshOpenAIRunStripsPriorResponsesComputerPairsFromHistory(t *testing.T) {
	profile := resolveTrustedOpenAIComputerProfile(t, "gpt-5.6-sol")
	firstLLM := &openAIComputerLoopLLM{responses: []*client.CompletionResponse{
		openAIComputerLoopResponse(
			t,
			profile,
			openAIContinuationTokenPrimary,
			normalizedOpenAIComputerCallForTrajectory,
		),
		openAIComputerLoopFinalResponse(t, profile, "resp_prior_2", "first done"),
	}}
	firstExecutor := &openAIComputerLoopBatchExecutor{
		executions: []OpenAIComputerBatchExecution{{
			CallID:              "call_001",
			ContinuationAllowed: true,
			Result: ToolResult{Images: []ImageBlock{{
				MediaType: "image/png", Data: "cHJpb3I=",
			}}},
		}},
	}
	firstLoop := NewAgentLoop(
		firstLLM,
		NewToolRegistry(),
		"medium",
		t.TempDir(),
		4,
		2000,
		200,
		nil,
		nil,
		nil,
	)
	firstLoop.SetSkillDiscovery(false)
	firstLoop.SetSpecificModel(profile.Model())
	firstLoop.SetExecutionProfile(profile)
	firstLoop.SetOpenAIComputerBatchExecutor(firstExecutor)
	if _, _, err := firstLoop.Run(context.Background(), "first run", nil, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	history := firstLoop.RunMessages()
	var persistedPairs int
	for _, message := range history {
		for _, block := range message.Content.Blocks() {
			if block.Type == client.OpenAIComputerCallType ||
				block.Type == "tool_result" && block.ToolUseID == "call_001" {
				persistedPairs++
			}
		}
	}
	if persistedPairs != 2 {
		t.Fatalf("first-run persisted pair blocks = %d, want 2", persistedPairs)
	}

	secondLLM := &openAIComputerLoopLLM{responses: []*client.CompletionResponse{
		openAIComputerLoopFinalResponse(t, profile, "resp_fresh_1", "second done"),
	}}
	secondLoop := NewAgentLoop(
		secondLLM,
		NewToolRegistry(),
		"medium",
		t.TempDir(),
		2,
		2000,
		200,
		nil,
		nil,
		nil,
	)
	secondLoop.SetSkillDiscovery(false)
	secondLoop.SetSpecificModel(profile.Model())
	secondLoop.SetExecutionProfile(profile)
	reply, _, err := secondLoop.Run(
		context.Background(),
		"new user turn",
		nil,
		history,
	)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if reply != "second done" {
		t.Fatalf("second reply = %q", reply)
	}
	requests := secondLLM.capturedRequests()
	if len(requests) != 1 {
		t.Fatalf("fresh-run completion requests = %d, want 1", len(requests))
	}
	if requests[0].PreviousResponseID != "" {
		t.Fatalf(
			"fresh-run previous_response_id = %q",
			requests[0].PreviousResponseID,
		)
	}
	for _, message := range requests[0].Messages {
		for _, block := range message.Content.Blocks() {
			if block.Type == client.OpenAIComputerCallType ||
				block.Type == "tool_result" && block.ToolUseID == "call_001" {
				t.Fatalf("fresh run resent prior Responses pair: %#v", block)
			}
		}
	}
	// The caller-owned history remains intact; stripping is outbound-only.
	var historyPairsAfter int
	for _, message := range history {
		for _, block := range message.Content.Blocks() {
			if block.Type == client.OpenAIComputerCallType ||
				block.Type == "tool_result" && block.ToolUseID == "call_001" {
				historyPairsAfter++
			}
		}
	}
	if historyPairsAfter != 2 {
		t.Fatalf("history was mutated while stripping: pairs=%d", historyPairsAfter)
	}
}

func TestAgentLoopOpenAIComputerRejectsMismatchedFunctionCallAliasBeforeExecution(t *testing.T) {
	profile := resolveTrustedOpenAIComputerProfile(t, "gpt-5.6-sol")
	tests := map[string]func(*client.CompletionResponse){
		"call id": func(response *client.CompletionResponse) {
			response.ToolCalls[0].CallID = "call_other"
		},
		"response id": func(response *client.CompletionResponse) {
			response.ToolCalls[0].ResponseID = openAIContinuationTokenOther
		},
		"pending safety checks": func(response *client.CompletionResponse) {
			response.ToolCalls[0].PendingSafetyChecks = []client.OpenAIComputerSafetyCheck{{
				ID: "check_injected",
			}}
		},
		"arguments safety checks": func(response *client.CompletionResponse) {
			var arguments map[string]any
			if err := json.Unmarshal(
				response.ToolCalls[0].Arguments,
				&arguments,
			); err != nil {
				t.Fatal(err)
			}
			arguments["pending_safety_checks"] = []map[string]any{{
				"id": "check_injected", "code": nil, "message": nil,
			}}
			response.ToolCalls[0].Arguments = mustJSONMarshal(t, arguments)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			response := openAIComputerLoopResponse(
				t,
				profile,
				openAIContinuationTokenPrimary,
				normalizedOpenAIComputerCallForTrajectory,
			)
			mutate(response)
			llm := &openAIComputerLoopLLM{
				responses: []*client.CompletionResponse{response},
			}
			executor := &openAIComputerLoopBatchExecutor{}
			loop := newOpenAIComputerTestLoop(
				t,
				llm,
				profile,
				executor,
				nil,
			)

			if _, _, err := loop.Run(
				context.Background(),
				"inspect",
				nil,
				nil,
			); err == nil {
				t.Fatal("mismatched computer_call alias was accepted")
			}
			if calls := executor.capturedCalls(); len(calls) != 0 {
				t.Fatalf("mismatched alias reached executor: %+v", calls)
			}
		})
	}
}

func TestAgentLoopOpenAIComputerUnknownCommitContinuesFromFinalScreenshot(t *testing.T) {
	profile := resolveTrustedOpenAIComputerProfile(t, "gpt-5.6-sol")
	llm := &openAIComputerLoopLLM{responses: []*client.CompletionResponse{
		openAIComputerLoopResponse(
			t,
			profile,
			openAIContinuationTokenPrimary,
			normalizedOpenAIComputerCallForTrajectory,
		),
		{
			Model:        profile.Model(),
			Provider:     profile.Provider(),
			FinishReason: "stop",
			OutputText:   "done",
		},
	}}
	executor := &openAIComputerLoopBatchExecutor{
		executions: []OpenAIComputerBatchExecution{{
			CallID:              "call_001",
			ContinuationAllowed: true,
			Result: ToolResult{
				Content: "commit status is unknown; do not retry automatically",
				IsError: true,
				Images: []ImageBlock{{
					MediaType: "image/png",
					Data:      "ZmluYWw=",
				}},
			},
		}},
	}
	loop := NewAgentLoop(
		llm,
		NewToolRegistry(),
		"medium",
		t.TempDir(),
		4,
		2000,
		200,
		nil,
		nil,
		nil,
	)
	loop.SetSkillDiscovery(false)
	loop.SetSpecificModel(profile.Model())
	loop.SetExecutionProfile(profile)
	loop.SetOpenAIComputerBatchExecutor(executor)

	reply, _, err := loop.Run(context.Background(), "click once", nil, nil)
	if err != nil || reply != "done" {
		t.Fatalf("unknown commit recovery = %q / %v", reply, err)
	}
	requests := llm.capturedRequests()
	if got := len(requests); got != 2 {
		t.Fatalf("unknown commit issued %d completion requests, want 2", got)
	}
	if got := len(executor.capturedCalls()); got != 1 {
		t.Fatalf("unknown commit executed %d batches, want 1", got)
	}

	continuation := requests[1]
	if continuation.PreviousResponseID != openAIContinuationTokenPrimary {
		t.Fatalf("continuation response id = %q", continuation.PreviousResponseID)
	}
	if len(continuation.Messages) < 2 {
		t.Fatalf("continuation messages = %#v", continuation.Messages)
	}
	output := continuation.Messages[len(continuation.Messages)-1]
	blocks := output.Content.Blocks()
	if output.Role != "user" || len(blocks) != 1 ||
		blocks[0].ToolUseID != "call_001" || !blocks[0].IsError {
		t.Fatalf("recovery computer_call_output = %#v", output)
	}
	nested, ok := blocks[0].ToolContent.([]client.ContentBlock)
	if !ok || len(nested) != 1 || nested[0].Source == nil ||
		nested[0].Source.Data != "ZmluYWw=" {
		t.Fatalf("recovery final screenshot = %#v", blocks[0].ToolContent)
	}
}

func TestAgentLoopOpenAIComputerStopsAfterRepeatedFailedBatches(t *testing.T) {
	profile := resolveTrustedOpenAIComputerProfile(t, "gpt-5.6-sol")
	llm := &openAIComputerLoopLLM{responses: []*client.CompletionResponse{
		openAIComputerLoopResponse(
			t,
			profile,
			openAIContinuationTokenPrimary,
			normalizedOpenAIComputerCallForTrajectory,
		),
		openAIComputerLoopResponse(
			t,
			profile,
			openAIContinuationTokenSecondary,
			normalizedOpenAIComputerCallForTrajectory,
		),
		openAIComputerLoopResponse(
			t,
			profile,
			openAIContinuationTokenOther,
			normalizedOpenAIComputerCallForTrajectory,
		),
	}}
	failedBatch := func(detail string) OpenAIComputerBatchExecution {
		return OpenAIComputerBatchExecution{
			CallID:              "call_001",
			ContinuationAllowed: true,
			Result: ToolResult{
				Content: detail,
				IsError: true,
				Images: []ImageBlock{{
					MediaType: "image/png",
					Data:      "ZmluYWw=",
				}},
			},
		}
	}
	executor := &openAIComputerLoopBatchExecutor{
		executions: []OpenAIComputerBatchExecution{
			failedBatch("OpenAI computer action 1 of 2 did not complete: click target vanished"),
			failedBatch("OpenAI computer action 1 of 2 did not complete: click target vanished"),
			failedBatch("OpenAI computer action 2 of 2 did not complete: typed text was not accepted"),
		},
	}
	loop := NewAgentLoop(
		llm,
		NewToolRegistry(),
		"medium",
		t.TempDir(),
		10,
		2000,
		200,
		nil,
		nil,
		nil,
	)
	loop.SetSkillDiscovery(false)
	loop.SetSpecificModel(profile.Model())
	loop.SetExecutionProfile(profile)
	loop.SetOpenAIComputerBatchExecutor(executor)

	_, _, err := loop.Run(context.Background(), "click once", nil, nil)
	if err == nil {
		t.Fatal("repeated failed batches did not terminate the run")
	}
	for _, want := range []string{
		"stopped after 3 failed action batches",
		"typed text was not accepted",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("terminal error %q missing %q", err.Error(), want)
		}
	}
	if got := len(executor.capturedCalls()); got != 3 {
		t.Fatalf("failed batches executed = %d, want exactly 3", got)
	}
	if got := len(llm.capturedRequests()); got != 3 {
		t.Fatalf(
			"completion requests = %d, want 3 (no provider call after the recovery cap)",
			got,
		)
	}
}

func TestAgentLoopOpenAIComputerTerminalErrorsPreserveExecutorDetail(t *testing.T) {
	profile := resolveTrustedOpenAIComputerProfile(t, "gpt-5.6-sol")
	cases := []struct {
		name       string
		execution  OpenAIComputerBatchExecution
		executeErr error
		want       []string
	}{
		{
			name: "payload rejected before execution surfaces the decode reason",
			execution: OpenAIComputerBatchExecution{
				Result: ToolResult{
					Content: `validation error: OpenAI computer action 1 has an unsupported type "triple_click"`,
					IsError: true,
				},
			},
			want: []string{
				"rejected before execution",
				`unsupported type "triple_click"`,
			},
		},
		{
			name:       "executor error is wrapped not replaced",
			executeErr: errors.New("focus target vanished mid-action"),
			want: []string{
				"execution failed",
				"focus target vanished mid-action",
			},
		},
		{
			name: "missing continuation state carries the executor reason",
			execution: OpenAIComputerBatchExecution{
				CallID: "call_001",
				Result: ToolResult{
					Content: "OpenAI computer batch finished, but its required final exact screenshot is unavailable",
					IsError: true,
				},
			},
			want: []string{
				"no verified state for continuation",
				"final exact screenshot is unavailable",
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			llm := &openAIComputerLoopLLM{responses: []*client.CompletionResponse{
				openAIComputerLoopResponse(
					t,
					profile,
					openAIContinuationTokenPrimary,
					normalizedOpenAIComputerCallForTrajectory,
				),
			}}
			executor := &openAIComputerLoopBatchExecutor{
				executions:    []OpenAIComputerBatchExecution{testCase.execution},
				executionErrs: []error{testCase.executeErr},
			}
			loop := NewAgentLoop(
				llm,
				NewToolRegistry(),
				"medium",
				t.TempDir(),
				4,
				2000,
				200,
				nil,
				nil,
				nil,
			)
			loop.SetSkillDiscovery(false)
			loop.SetSpecificModel(profile.Model())
			loop.SetExecutionProfile(profile)
			loop.SetOpenAIComputerBatchExecutor(executor)

			_, _, err := loop.Run(context.Background(), "click once", nil, nil)
			if err == nil {
				t.Fatal("invalid batch execution did not terminate the run")
			}
			for _, want := range testCase.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("terminal error %q missing %q", err.Error(), want)
				}
			}
		})
	}
}

func TestAgentLoopGenericComputerProfileNeverCallsOpenAINativeExecutor(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join(
		"..",
		"..",
		"docs",
		"desktop-wire-fixtures",
		"execution-profiles-v1",
		"profile.openai-generic-ax-only.json",
	))
	if err != nil {
		t.Fatal(err)
	}
	profile := resolveTrustedExecutionProfileFromJSON(t, fixture)
	llm := &openAIComputerLoopLLM{responses: []*client.CompletionResponse{
		openAIComputerLoopResponse(
			t,
			profile,
			openAIContinuationTokenPrimary,
			normalizedOpenAIComputerCallForTrajectory,
		),
	}}
	executor := &openAIComputerLoopBatchExecutor{}
	loop := NewAgentLoop(
		llm,
		NewToolRegistry(),
		"medium",
		t.TempDir(),
		2,
		2000,
		200,
		nil,
		nil,
		nil,
	)
	loop.SetSkillDiscovery(false)
	loop.SetSpecificModel(profile.Model())
	loop.SetExecutionProfile(profile)
	loop.SetOpenAIComputerBatchExecutor(executor)

	if _, _, err := loop.Run(context.Background(), "inspect", nil, nil); err == nil {
		t.Fatal("generic profile accepted a native computer_call")
	}
	if calls := executor.capturedCalls(); len(calls) != 0 {
		t.Fatalf("generic profile reached native executor: %+v", calls)
	}
}

func resolveTrustedExecutionProfileFromJSON(
	t *testing.T,
	fixture []byte,
) *client.ExecutionProfile {
	t.Helper()
	var expected struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(fixture, &expected); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()
	profile, err := client.NewGatewayClient(server.URL, "").ResolveExecutionProfile(
		context.Background(),
		client.ResolveExecutionProfileRequest{
			SchemaVersion: client.ExecutionProfileSchemaVersion,
			SpecificModel: expected.Model,
			Capability:    client.ExecutionProfileCapabilityComputer,
		},
	)
	if err != nil {
		t.Fatalf("resolve trusted profile: %v", err)
	}
	return profile
}
