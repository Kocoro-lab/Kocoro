package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestOpenAIComputerProfileFixtureUsesAuthenticatedResolveEntry(t *testing.T) {
	profileBytes := loadExecutionProfileFixture(t, "profile.openai-native.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/completions/resolve" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(profileBytes)
	}))
	defer server.Close()

	profile, err := NewGatewayClient(server.URL, "").ResolveExecutionProfile(
		context.Background(),
		ResolveExecutionProfileRequest{
			SchemaVersion: 1,
			SpecificModel: "gpt-5.6-sol",
			Capability:    ExecutionProfileCapabilityComputer,
		},
	)
	if err != nil {
		t.Fatalf("ResolveExecutionProfile fixture: %v", err)
	}
	if !profile.IsTrustedResolution() ||
		profile.Provider() != OpenAIComputerProvider ||
		profile.APISurface() != APISurfaceOpenAIResponses ||
		profile.ToolContract() != ToolContractOpenAIComputerV1 ||
		profile.SupportsFunctionTools() ||
		!profile.SupportsBatchedActions() {
		t.Fatalf("resolved OpenAI fixture profile = %#v", profile)
	}
}

func TestOpenAIComputerCompletionAndContinuationFixturesUseProductionTypes(t *testing.T) {
	var response CompletionResponse
	if err := json.Unmarshal(
		loadExecutionProfileFixture(t, "completion.openai-native-computer-call.json"),
		&response,
	); err != nil {
		t.Fatalf("decode completion fixture: %v", err)
	}
	if response.RequestID != openAIContinuationTokenPrimary ||
		response.ExecutionProfile == nil ||
		len(response.ContentBlocks) != 1 {
		t.Fatalf("completion fixture = %#v", response)
	}
	call, err := response.ContentBlocks[0].NormalizedOpenAIComputerCall()
	if err != nil {
		t.Fatalf("normalized completion call: %v", err)
	}
	if call.CallID != "call_001" || call.ToolContract != ToolContractOpenAIComputerV1 {
		t.Fatalf("normalized completion call = %#v", call)
	}
	if call.ResponseID != response.RequestID ||
		len(call.PendingSafetyChecks) != 2 {
		t.Fatalf("normalized completion safety provenance = %#v", call)
	}

	continuationBytes := loadExecutionProfileFixture(
		t,
		"completion-request.openai-native-continuation.json",
	)
	var continuation CompletionRequest
	if err := json.Unmarshal(continuationBytes, &continuation); err != nil {
		t.Fatalf("decode continuation fixture: %v", err)
	}
	if continuation.PreviousResponseID != openAIContinuationTokenSecondary ||
		continuation.ExecutionProfileID != response.ExecutionProfile.ProfileID() ||
		len(continuation.Messages) != 3 {
		t.Fatalf("continuation fixture = %#v", continuation)
	}
	assistant := continuation.Messages[1].Content.Blocks()
	if len(assistant) != 1 {
		t.Fatalf("assistant continuation blocks = %#v", assistant)
	}
	continuedCall, err := assistant[0].NormalizedOpenAIComputerCall()
	if err != nil {
		t.Fatalf("normalized continuation call: %v", err)
	}
	if continuedCall.ResponseID != continuation.PreviousResponseID {
		t.Fatalf(
			"continued response_id = %q, previous_response_id = %q",
			continuedCall.ResponseID,
			continuation.PreviousResponseID,
		)
	}
	comparableCall := call
	comparableCall.ResponseID = continuedCall.ResponseID
	if !reflect.DeepEqual(continuedCall, comparableCall) {
		t.Fatalf("continued call = %#v, completion call = %#v", continuedCall, call)
	}
	result := continuation.Messages[2].Content.Blocks()
	if len(result) != 1 ||
		!reflect.DeepEqual(
			result[0].AcknowledgedSafetyChecks,
			continuedCall.PendingSafetyChecks,
		) {
		t.Fatalf("continuation safety acknowledgement = %#v", result)
	}
}

func TestOpenAIComputerStreamFixturePreservesContinuationResponseID(t *testing.T) {
	data := loadExecutionProfileFixture(t, "stream.openai-native-computer-call.sse")
	scanner := bufio.NewScanner(bytes.NewReader(data))
	var result *CompletionResponse
	for scanner.Scan() {
		stop, parsed := processSSEData(scanner.Text(), func(StreamDelta) {})
		if parsed != nil {
			result = parsed
		}
		if stop {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if result == nil ||
		result.RequestID != openAIContinuationTokenSecondary ||
		len(result.ContentBlocks) != 1 {
		t.Fatalf("stream fixture result = %#v", result)
	}
	if _, err := result.ContentBlocks[0].NormalizedOpenAIComputerCall(); err != nil {
		t.Fatalf("stream fixture normalized call: %v", err)
	}
}
