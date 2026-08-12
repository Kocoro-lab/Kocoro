package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

func TestAgentLoopOrdinaryOpenAIResponsesKeepsToolResultBeforeLoopNudge(t *testing.T) {
	// With the stable read-only threshold at 3, request 4 carries the nudge.
	// The recovery window is covered separately by the atomic-admission tests.
	responses := make([]*client.CompletionResponse, 0, 4)
	for index := 1; index <= 3; index++ {
		response := ordinaryOpenAIToolResponse(
			"openai",
			"gpt-5.6-sol",
			fmt.Sprintf("resp_tool_turn_%d", index),
		)
		callID := fmt.Sprintf("call_lookup_%d", index)
		response.ToolCalls[0].ID = callID
		response.ToolCalls[0].CallID = callID
		responses = append(responses, response)
	}
	responses = append(responses, &client.CompletionResponse{
		Provider:     "openai",
		Model:        "gpt-5.6-sol",
		OutputText:   "done",
		FinishReason: "stop",
		RequestID:    "resp_final",
	})

	llm := &budgetCaptureLLMClient{responses: responses}
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
		8,
		2000,
		200,
		nil,
		nil,
		nil,
	)
	loop.SetSkillDiscovery(false)
	loop.SetEnableStreaming(false)

	result, _, err := loop.Run(context.Background(), "repeat lookup", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "done" {
		t.Fatalf("result = %q, want done", result)
	}

	requests := llm.requests
	if len(requests) != 4 {
		t.Fatalf("completion requests = %d, want 4", len(requests))
	}
	if len(llm.responses) != 0 {
		t.Fatalf("scripted responses remaining = %d, want 0", len(llm.responses))
	}
	nudgeRequest := findLoopNudgeRequest(requests)
	if nudgeRequest == nil {
		t.Fatalf("no continuation request carried a loop nudge: %+v", requests)
	}
	if nudgeRequest.PreviousResponseID == "" {
		t.Fatal("loop nudge request lost the OpenAI Responses cursor")
	}

	assertToolResultPrecedesNudge(t, nudgeRequest)
}

func findLoopNudgeRequest(requests []client.CompletionRequest) *client.CompletionRequest {
	for index := range requests {
		if len(requests[index].Tools) == 0 {
			continue
		}
		messages := requests[index].Messages
		if len(messages) == 0 {
			continue
		}
		last := messages[len(messages)-1]
		if last.Role == "developer" && strings.Contains(last.Content.Text(), "Inspect the latest result") {
			return &requests[index]
		}
	}
	return nil
}

func assertToolResultPrecedesNudge(t *testing.T, request *client.CompletionRequest) {
	t.Helper()
	messages := request.Messages
	if len(messages) < 3 {
		t.Fatalf("nudge request has %d messages, want at least 3", len(messages))
	}
	assistant := messages[len(messages)-3]
	toolResult := messages[len(messages)-2]
	nudge := messages[len(messages)-1]
	assistantBlocks := assistant.Content.Blocks()
	resultBlocks := toolResult.Content.Blocks()
	toolUseIDs := make(map[string]struct{})
	for _, block := range assistantBlocks {
		if block.Type == "tool_use" && block.ID != "" {
			toolUseIDs[block.ID] = struct{}{}
		}
	}
	if assistant.Role != "assistant" || len(toolUseIDs) == 0 {
		t.Fatalf("assistant before nudge = %#v", assistant)
	}
	if toolResult.Role != "user" || len(resultBlocks) != 1 ||
		resultBlocks[0].Type != "tool_result" {
		t.Fatalf("paired tool result before nudge = %#v", toolResult)
	}
	if _, ok := toolUseIDs[resultBlocks[0].ToolUseID]; !ok {
		t.Fatalf("paired tool result before nudge = %#v", toolResult)
	}
	if nudge.Role != "developer" || !strings.Contains(nudge.Content.Text(), "Inspect the latest result") {
		t.Fatalf("nudge tail = %#v", nudge)
	}
}

type ordinaryContinuationMutatingClient struct {
	loop     *AgentLoop
	requests []client.CompletionRequest
	calls    int
}

func (m *ordinaryContinuationMutatingClient) Complete(
	_ context.Context,
	req client.CompletionRequest,
) (*client.CompletionResponse, error) {
	m.requests = append(m.requests, req)
	m.calls++
	if m.calls == 1 {
		m.loop.SetReasoningEffort("high")
		return ordinaryOpenAIToolResponse(
			"openai",
			"gpt-5.6-sol",
			"resp_before_request_change",
		), nil
	}
	return &client.CompletionResponse{
		Provider:     "openai",
		Model:        "gpt-5.6-sol",
		OutputText:   "BLUE",
		FinishReason: "stop",
		RequestID:    "resp_after_request_change",
	}, nil
}

func (m *ordinaryContinuationMutatingClient) CompleteStream(
	ctx context.Context,
	req client.CompletionRequest,
	_ func(client.StreamDelta),
) (*client.CompletionResponse, error) {
	return m.Complete(ctx, req)
}

func TestAgentLoopOrdinaryOpenAIResponsesFallsBackAfterRequestChange(t *testing.T) {
	llm := &ordinaryContinuationMutatingClient{}
	loop := newOrdinaryOpenAIContinuationLoop(t, llm)
	llm.loop = loop

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
	continuation := llm.requests[1]
	if continuation.PreviousResponseID != "" {
		t.Fatalf(
			"changed request reused previous_response_id %q",
			continuation.PreviousResponseID,
		)
	}
	if continuation.SpecificModel != "" {
		t.Fatalf("changed request retained provider cursor model pin %q", continuation.SpecificModel)
	}
	if continuation.ReasoningEffort != "high" {
		t.Fatalf("changed request reasoning_effort = %q, want high", continuation.ReasoningEffort)
	}
	if len(continuation.Messages) != len(llm.requests[0].Messages)+2 {
		t.Fatalf("fallback did not carry the full local transcript")
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

// compactionCursorLLMClient mints an ordinary Responses cursor on the first
// call, overflows exactly once on the request that carries it, and succeeds on
// everything else (including the small-tier compaction calls).
type compactionCursorLLMClient struct {
	requests []client.CompletionRequest
	errored  bool
}

func (m *compactionCursorLLMClient) Complete(ctx context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
	m.requests = append(m.requests, req)
	if len(m.requests) == 1 {
		return ordinaryOpenAIToolResponse("openai", "gpt-5.6-sol", "resp_tool_turn_1"), nil
	}
	if req.PreviousResponseID != "" && !m.errored {
		m.errored = true
		return nil, &client.APIError{StatusCode: 400, Body: `{"error":{"type":"invalid_request_error","message":"prompt is too long"}}`}
	}
	return &client.CompletionResponse{
		Provider:     "openai",
		Model:        "gpt-5.6-sol",
		OutputText:   "BLUE",
		FinishReason: "stop",
		RequestID:    "resp_final_turn",
	}, nil
}

func (m *compactionCursorLLMClient) CompleteStream(ctx context.Context, req client.CompletionRequest, onDelta func(client.StreamDelta)) (*client.CompletionResponse, error) {
	return m.Complete(ctx, req)
}

// A context-length overflow on an ordinary continuation request must NOT carry
// previous_response_id into the post-compaction retry: with the cursor set the
// provider rebuilds context from its own stored chain, which local compaction
// did not shrink, so the retry re-overflows and reactiveCompacted blocks a
// second attempt — the turn dies where the cursorless path recovers.
func TestReactiveCompactionDropsOrdinaryOpenAIContinuationCursor(t *testing.T) {
	llm := &compactionCursorLLMClient{}
	loop := newOrdinaryOpenAIContinuationLoop(t, llm)

	result, _, err := loop.Run(context.Background(), "look up cobalt", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "BLUE" {
		t.Fatalf("result = %q, want BLUE", result)
	}

	erroredIdx := -1
	for i, req := range llm.requests {
		if req.PreviousResponseID == "resp_tool_turn_1" {
			erroredIdx = i
			break
		}
	}
	if erroredIdx == -1 {
		t.Fatal("no request carried the ordinary continuation cursor")
	}
	if erroredIdx+1 >= len(llm.requests) {
		t.Fatal("no retry request after the context-length overflow")
	}
	for _, req := range llm.requests[erroredIdx+1:] {
		if req.PreviousResponseID != "" {
			t.Fatalf(
				"post-compaction request still pinned previous_response_id=%q — provider-side context is not compacted",
				req.PreviousResponseID,
			)
		}
	}
	// The exact model that owns the tool trajectory must survive the rebuild:
	// dropping the cursor must not hop the trajectory onto tier routing. The
	// requests between the overflow and the retry are the compaction's own
	// small-tier calls; the main retry is the final request.
	if retry := llm.requests[len(llm.requests)-1]; retry.SpecificModel != "gpt-5.6-sol" {
		t.Fatalf(
			"post-compaction retry specific_model = %q, want gpt-5.6-sol (trajectory model pin lost)",
			retry.SpecificModel,
		)
	}
}
