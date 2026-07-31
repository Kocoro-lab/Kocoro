package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func ordinaryOpenAIToolResponse(provider, model, requestID string) *client.CompletionResponse {
	return &client.CompletionResponse{
		Provider:     provider,
		Model:        model,
		FinishReason: "tool_use",
		ToolCalls: []client.FunctionCall{{
			ID:         "call_lookup",
			CallID:     "call_lookup",
			Name:       "lookup",
			Arguments:  json.RawMessage(`{"key":"cobalt"}`),
			Type:       "function",
			Provider:   provider,
			APISurface: client.APISurfaceOpenAIResponses,
		}},
		RequestID: requestID,
	}
}

func newOrdinaryOpenAIContinuationLoop(
	t *testing.T,
	llm client.LLMClient,
) *AgentLoop {
	t.Helper()
	registry := NewToolRegistry()
	registry.Register(&mockSimpleTool{
		name:   "lookup",
		result: ToolResult{Content: "BLUE"},
	})
	loop := NewAgentLoop(
		llm,
		registry,
		"large",
		t.TempDir(),
		4,
		2000,
		200,
		nil,
		nil,
		nil,
	)
	loop.SetSkillDiscovery(false)
	loop.SetEnableStreaming(false)
	return loop
}

func TestAgentLoopOrdinaryOpenAIResponsesContinuesToolResult(t *testing.T) {
	llm := &budgetCaptureLLMClient{responses: []*client.CompletionResponse{
		ordinaryOpenAIToolResponse(
			"openai",
			"gpt-5.6-sol",
			"resp_tool_turn_1",
		),
		{
			Provider:     "openai",
			Model:        "gpt-5.6-sol",
			OutputText:   "BLUE",
			FinishReason: "stop",
			RequestID:    "resp_final_turn_2",
		},
		{
			Provider:     "openai",
			Model:        "gpt-5.6-sol",
			OutputText:   "fresh",
			FinishReason: "stop",
			RequestID:    "resp_fresh_run",
		},
	}}
	loop := newOrdinaryOpenAIContinuationLoop(t, llm)

	result, _, err := loop.Run(context.Background(), "look up cobalt", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "BLUE" {
		t.Fatalf("result = %q, want BLUE", result)
	}
	if len(llm.requests) != 2 {
		t.Fatalf("completion requests = %d, want 2", len(llm.requests))
	}
	first, continuation := llm.requests[0], llm.requests[1]
	if first.PreviousResponseID != "" || first.SpecificModel != "" {
		t.Fatalf("initial tier request was unexpectedly pinned: %+v", first)
	}
	if continuation.PreviousResponseID != "resp_tool_turn_1" {
		t.Fatalf(
			"continuation previous_response_id = %q, want resp_tool_turn_1",
			continuation.PreviousResponseID,
		)
	}
	if continuation.SpecificModel != "gpt-5.6-sol" {
		t.Fatalf(
			"continuation specific_model = %q, want gpt-5.6-sol",
			continuation.SpecificModel,
		)
	}
	if len(continuation.Messages) != len(first.Messages)+2 {
		t.Fatalf(
			"continuation messages = %d, want full transcript %d + 2",
			len(continuation.Messages),
			len(first.Messages),
		)
	}
	assistant := continuation.Messages[len(continuation.Messages)-2]
	assistantBlocks := assistant.Content.Blocks()
	if assistant.Role != "assistant" || len(assistantBlocks) != 1 ||
		assistantBlocks[0].Type != "tool_use" ||
		assistantBlocks[0].ID != "call_lookup" {
		t.Fatalf("assistant tool trajectory = %#v", assistant)
	}
	output := continuation.Messages[len(continuation.Messages)-1]
	outputBlocks := output.Content.Blocks()
	if output.Role != "user" || len(outputBlocks) != 1 ||
		outputBlocks[0].Type != "tool_result" ||
		outputBlocks[0].ToolUseID != "call_lookup" {
		t.Fatalf("tool-result trajectory = %#v", output)
	}
	forkSnapshot, ok := loop.LastSentRequest()
	if !ok {
		t.Fatal("post-Run fork snapshot is unavailable")
	}
	if forkSnapshot.PreviousResponseID != "" {
		t.Fatalf(
			"post-Run fork snapshot retained consumed cursor %q",
			forkSnapshot.PreviousResponseID,
		)
	}
	if forkSnapshot.SpecificModel != "gpt-5.6-sol" {
		t.Fatalf(
			"post-Run fork snapshot lost actual model pin %q",
			forkSnapshot.SpecificModel,
		)
	}
	if fork := BuildForkedSuggestionRequest(forkSnapshot); fork.PreviousResponseID != "" {
		t.Fatalf("suggestion fork retained consumed cursor %q", fork.PreviousResponseID)
	}

	// The cursor/model pin belongs only to the immediately following
	// tool-result request. A final text response ends the trajectory, so a new
	// Run starts from the loop's original tier configuration.
	result, _, err = loop.Run(context.Background(), "start fresh", nil, nil)
	if err != nil {
		t.Fatalf("fresh Run: %v", err)
	}
	if result != "fresh" {
		t.Fatalf("fresh result = %q, want fresh", result)
	}
	if len(llm.requests) != 3 {
		t.Fatalf("completion requests after fresh Run = %d, want 3", len(llm.requests))
	}
	fresh := llm.requests[2]
	if fresh.PreviousResponseID != "" || fresh.SpecificModel != "" {
		t.Fatalf("ordinary Responses continuation leaked into fresh Run: %+v", fresh)
	}
}

func TestAgentLoopOrdinaryOpenAIResponsesRetryKeepsContinuation(t *testing.T) {
	llm := &openAIComputerLoopLLM{
		responses: []*client.CompletionResponse{
			ordinaryOpenAIToolResponse(
				"openai",
				"gpt-5.6-sol",
				"resp_tool_retry",
			),
			{
				Provider:     "openai",
				Model:        "gpt-5.6-sol",
				OutputText:   "done",
				FinishReason: "stop",
			},
		},
		errors: []error{
			nil,
			&client.APIError{StatusCode: 503, Body: "temporary"},
			nil,
		},
	}
	loop := newOrdinaryOpenAIContinuationLoop(t, llm)
	if _, _, err := loop.Run(context.Background(), "lookup", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	requests := llm.capturedRequests()
	if len(requests) != 3 {
		t.Fatalf("completion requests = %d, want initial + 2 continuation attempts", len(requests))
	}
	for index, request := range requests[1:] {
		if request.PreviousResponseID != "resp_tool_retry" ||
			request.SpecificModel != "gpt-5.6-sol" {
			t.Fatalf("continuation attempt %d drifted: %+v", index+1, request)
		}
	}
	forkSnapshot, ok := loop.LastSentRequest()
	if !ok || forkSnapshot.PreviousResponseID != "" ||
		forkSnapshot.SpecificModel != "gpt-5.6-sol" {
		t.Fatalf("post-retry fork snapshot = (%+v, %t)", forkSnapshot, ok)
	}
}

func TestAgentLoopOrdinaryOpenAIResponsesRejectsUntrustedContinuation(t *testing.T) {
	tests := []struct {
		name      string
		provider  string
		model     string
		requestID string
		koeFast   bool
		wantModel string
	}{
		{
			name:      "non response id",
			provider:  "openai",
			model:     "gpt-5.6-sol",
			requestID: "chatcmpl_tool_turn",
			wantModel: "claude-sonnet-5",
		},
		{
			name:      "malformed response id",
			provider:  "openai",
			model:     "gpt-5.6-sol",
			requestID: "resp_bad/value",
			wantModel: "claude-sonnet-5",
		},
		{
			name:      "non OpenAI provider",
			provider:  "anthropic",
			model:     "claude-sonnet-5",
			requestID: "resp_looks_openai",
			wantModel: "claude-sonnet-5",
		},
		{
			name:      "missing response model",
			provider:  "openai",
			requestID: "resp_missing_model",
			wantModel: "claude-sonnet-5",
		},
		{
			name:      "Koe Fast Chat",
			provider:  "openai",
			model:     "gpt-5.6-terra",
			requestID: "resp_must_not_escape_fast",
			koeFast:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llm := &budgetCaptureLLMClient{responses: []*client.CompletionResponse{
				ordinaryOpenAIToolResponse(
					tt.provider,
					tt.model,
					tt.requestID,
				),
				{
					Provider:     tt.provider,
					Model:        tt.model,
					OutputText:   "done",
					FinishReason: "stop",
				},
			}}
			loop := newOrdinaryOpenAIContinuationLoop(t, llm)
			loop.SetSpecificModel("claude-sonnet-5")
			if tt.koeFast {
				loop.SetKoeExecutionProfile(fastProfileForAgentTest())
			}

			if _, _, err := loop.Run(context.Background(), "lookup", nil, nil); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(llm.requests) != 2 {
				t.Fatalf("completion requests = %d, want 2", len(llm.requests))
			}
			continuation := llm.requests[1]
			if continuation.PreviousResponseID != "" {
				t.Fatalf(
					"untrusted cursor escaped: previous_response_id=%q",
					continuation.PreviousResponseID,
				)
			}
			if continuation.SpecificModel != tt.wantModel {
				t.Fatalf(
					"specific_model = %q, want unchanged %q",
					continuation.SpecificModel,
					tt.wantModel,
				)
			}
		})
	}
}

func TestAgentLoopOrdinaryOpenAIResponsesNoToolDoesNotStartContinuation(t *testing.T) {
	llm := &budgetCaptureLLMClient{responses: []*client.CompletionResponse{
		{
			Provider:     "openai",
			Model:        "gpt-5.6-sol",
			OutputText:   "first",
			FinishReason: "stop",
			RequestID:    "resp_text_only",
		},
		{
			Provider:     "openai",
			Model:        "gpt-5.6-sol",
			OutputText:   "second",
			FinishReason: "stop",
			RequestID:    "resp_second_text_only",
		},
	}}
	loop := newOrdinaryOpenAIContinuationLoop(t, llm)
	if _, _, err := loop.Run(context.Background(), "first", nil, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if _, _, err := loop.Run(context.Background(), "second", nil, nil); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(llm.requests) != 2 {
		t.Fatalf("completion requests = %d, want 2", len(llm.requests))
	}
	if llm.requests[1].PreviousResponseID != "" ||
		llm.requests[1].SpecificModel != "" {
		t.Fatalf("text-only response started a continuation: %+v", llm.requests[1])
	}
}

func TestAgentLoopOrdinaryOpenAIResponsesDoesNotOverrideNativeComputer(t *testing.T) {
	profile := resolveTrustedOpenAIComputerProfile(t, "gpt-5.6-sol")
	computerResponse := openAIComputerLoopResponse(
		t,
		profile,
		openAIContinuationTokenPrimary,
		normalizedOpenAIComputerCallForTrajectory,
	)
	// Even if a transport-level request ID looks like an ordinary Responses
	// cursor, the computer_call block must keep it outside the ordinary path.
	rawTransport := *computerResponse
	rawTransport.RequestID = "resp_native_transport_only"
	if responseID, model := ordinaryOpenAIResponsesContinuation(&rawTransport); responseID != "" ||
		model != "" {
		t.Fatalf(
			"native computer response entered ordinary continuation: id=%q model=%q",
			responseID,
			model,
		)
	}
	aliasOnly := rawTransport
	aliasOnly.ContentBlocks = nil
	if responseID, model := ordinaryOpenAIResponsesContinuation(&aliasOnly); responseID != "" ||
		model != "" {
		t.Fatalf(
			"native computer alias entered ordinary continuation: id=%q model=%q",
			responseID,
			model,
		)
	}
	llm := &openAIComputerLoopLLM{responses: []*client.CompletionResponse{
		computerResponse,
		openAIComputerLoopFinalResponse(t, profile, "resp_native_final", "done"),
	}}
	executor := &openAIComputerLoopBatchExecutor{
		executions: []OpenAIComputerBatchExecution{{
			CallID:              "call_001",
			ContinuationAllowed: true,
			Result: ToolResult{
				Content: "observation",
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
		"large",
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

	if _, _, err := loop.Run(context.Background(), "inspect", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	requests := llm.capturedRequests()
	if len(requests) != 2 {
		t.Fatalf("completion requests = %d, want 2", len(requests))
	}
	if requests[1].PreviousResponseID != openAIContinuationTokenPrimary {
		t.Fatalf(
			"native continuation cursor = %q, want sealed token %q",
			requests[1].PreviousResponseID,
			openAIContinuationTokenPrimary,
		)
	}
	forkSnapshot, ok := loop.LastSentRequest()
	if !ok || forkSnapshot.PreviousResponseID != openAIContinuationTokenPrimary {
		t.Fatalf(
			"native fork snapshot cursor = %q, want sealed token %q",
			forkSnapshot.PreviousResponseID,
			openAIContinuationTokenPrimary,
		)
	}
}
