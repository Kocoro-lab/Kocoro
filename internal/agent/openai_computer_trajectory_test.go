package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

const (
	openAIContinuationTokenPrimary   = "shct_pOIBMOn2gmZdU7TJZm93xdhEM1SNRTRle-n9A0mz76g"
	openAIContinuationTokenSecondary = "shct_WYl9Jlo4RkeE9sVHGA7GZlqr8_ZVa19Mg_gFbemKI_E"
	openAIContinuationTokenOther     = "shct_jB_YlgNBKJVPEO6R3u4QKwyG9xsvHyOP1ruR3hmqq-E"

	normalizedOpenAIComputerCallForTrajectory = `{"type":"computer_call","provider":"openai","api_surface":"openai_responses","tool_contract":"openai.computer.v1","response_id":"` + openAIContinuationTokenPrimary + `","call_id":"call_001","actions":[{"type":"click","button":"left","x":405,"y":157},{"type":"type","text":"penguin"}],"pending_safety_checks":[],"status":"completed"}`
)

func resolveTrustedOpenAIComputerProfile(t *testing.T, model string) *client.ExecutionProfile {
	t.Helper()
	canonical := map[string]any{
		"schema_version":              1,
		"contract_revision":           1,
		"provider":                    "openai",
		"model":                       model,
		"api_surface":                 "openai_responses",
		"execution_mode":              "native_computer",
		"tool_contract":               "openai.computer.v1",
		"beta_contract":               nil,
		"supports_image_input":        true,
		"supports_tool_result_images": true,
		"supports_function_tools":     false,
		"supports_batched_actions":    true,
	}
	canonicalJSON, err := json.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonicalJSON)
	canonical["profile_id"] = "ep1_" + hex.EncodeToString(sum[:])
	profileJSON, err := json.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(profileJSON)
	}))
	defer server.Close()

	profile, err := client.NewGatewayClient(server.URL, "").ResolveExecutionProfile(
		context.Background(),
		client.ResolveExecutionProfileRequest{
			SchemaVersion: 1,
			SpecificModel: model,
			Capability:    client.ExecutionProfileCapabilityComputer,
		},
	)
	if err != nil {
		t.Fatalf("resolve trusted OpenAI profile: %v", err)
	}
	if !profile.IsTrustedResolution() {
		t.Fatal("resolved OpenAI profile is not trusted")
	}
	return profile
}

func openAIComputerCompletionResponse(
	t *testing.T,
	profile *client.ExecutionProfile,
) *client.CompletionResponse {
	t.Helper()
	profileJSON, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	var response client.CompletionResponse
	payload := fmt.Sprintf(
		`{"provider":"openai","model":%q,"request_id":%q,"execution_profile":%s,"content_blocks":[%s]}`,
		profile.Model(),
		openAIContinuationTokenPrimary,
		profileJSON,
		normalizedOpenAIComputerCallForTrajectory,
	)
	if err := json.Unmarshal([]byte(payload), &response); err != nil {
		t.Fatalf("decode OpenAI computer response: %v", err)
	}
	if response.ExecutionProfile == nil || response.ExecutionProfile.IsTrustedResolution() {
		t.Fatal("response echo should be present but untrusted")
	}
	return &response
}

func TestOpenAIComputerTrajectoryBuildsBatchAndNextScreenshotResult(t *testing.T) {
	profile := resolveTrustedOpenAIComputerProfile(t, "gpt-5.6-sol")
	response := openAIComputerCompletionResponse(t, profile)

	trajectory, err := newOpenAIComputerTrajectory(profile, response)
	if err != nil {
		t.Fatalf("newOpenAIComputerTrajectory: %v", err)
	}
	envelope := trajectory.BatchEnvelope()
	if envelope.ResponseID != openAIContinuationTokenPrimary ||
		envelope.Call.CallID != "call_001" {
		t.Fatalf("batch envelope = %#v", envelope)
	}
	payload, err := envelope.AdapterPayload()
	if err != nil {
		t.Fatalf("AdapterPayload: %v", err)
	}
	requireAgentJSONSemanticEqual(t, payload, []byte(normalizedOpenAIComputerCallForTrajectory))

	base := client.CompletionRequest{
		Messages: []client.Message{{
			Role:    "user",
			Content: client.NewTextContent("open the thread"),
		}},
		SpecificModel:            profile.Model(),
		ExecutionProfileID:       profile.ProfileID(),
		ResolvedExecutionProfile: profile,
	}
	screenshot := client.ContentBlock{
		Type: "image",
		Source: &client.ImageSource{
			Type:      "base64",
			MediaType: "image/png",
			Data:      "c2NyZWVuc2hvdA==",
		},
	}
	acknowledgement, err := trajectory.newSafetyAcknowledgement(false)
	if err != nil {
		t.Fatalf("newSafetyAcknowledgement: %v", err)
	}
	if !acknowledgement.ConsumeForExecution(
		profile,
		trajectory.responseID,
		trajectory.call,
	) {
		t.Fatal("consume no-check safety acknowledgement")
	}
	next, err := trajectory.BuildNextRequest(base, screenshot, acknowledgement)
	if err != nil {
		t.Fatalf("BuildNextRequest: %v", err)
	}
	if next.PreviousResponseID != openAIContinuationTokenPrimary {
		t.Fatalf(
			"previous_response_id = %q, want %q",
			next.PreviousResponseID,
			openAIContinuationTokenPrimary,
		)
	}
	if len(next.Messages) != len(base.Messages)+2 {
		t.Fatalf("next message count = %d, want %d", len(next.Messages), len(base.Messages)+2)
	}

	assistant := next.Messages[len(base.Messages)]
	if assistant.Role != "assistant" || len(assistant.Content.Blocks()) != 1 {
		t.Fatalf("assistant trajectory message = %#v", assistant)
	}
	assistantWire, err := json.Marshal(assistant.Content.Blocks()[0])
	if err != nil {
		t.Fatal(err)
	}
	requireAgentJSONSemanticEqual(t, assistantWire, []byte(normalizedOpenAIComputerCallForTrajectory))

	resultMessage := next.Messages[len(base.Messages)+1]
	resultBlocks := resultMessage.Content.Blocks()
	if resultMessage.Role != "user" || len(resultBlocks) != 1 {
		t.Fatalf("result message = %#v", resultMessage)
	}
	result := resultBlocks[0]
	if result.Type != "tool_result" || result.ToolUseID != "call_001" || result.IsError {
		t.Fatalf("tool result = %#v", result)
	}
	nested, ok := result.ToolContent.([]client.ContentBlock)
	if !ok || len(nested) != 1 || !reflect.DeepEqual(nested[0], screenshot) {
		t.Fatalf("tool result content = %#v, want one screenshot", result.ToolContent)
	}

	wire, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	var requestWire map[string]json.RawMessage
	if err := json.Unmarshal(wire, &requestWire); err != nil {
		t.Fatal(err)
	}
	var previousID string
	if err := json.Unmarshal(requestWire["previous_response_id"], &previousID); err != nil {
		t.Fatalf("request previous_response_id: %v; wire=%s", err, wire)
	}
	if previousID != openAIContinuationTokenPrimary {
		t.Fatalf("wire previous_response_id = %q", previousID)
	}

	if base.PreviousResponseID != "" || len(base.Messages) != 1 {
		t.Fatalf("base request mutated: %#v", base)
	}
}

func TestOpenAIComputerSafetyAcknowledgementIsExplicitBoundAndOneShot(t *testing.T) {
	profile := resolveTrustedOpenAIComputerProfile(t, "gpt-5.6-sol")
	const callWithSafety = `{"type":"computer_call","provider":"openai","api_surface":"openai_responses","tool_contract":"openai.computer.v1","response_id":"` + openAIContinuationTokenSecondary + `","call_id":"call_safe","actions":[{"type":"click","button":"left","x":405,"y":157}],"pending_safety_checks":[{"id":"check_1","code":"malicious_instructions","message":"Confirm the direct user request."},{"id":"check_2","code":null,"message":null}],"status":"completed"}`
	trajectory, err := newOpenAIComputerTrajectory(
		profile,
		openAIComputerLoopResponse(
			t,
			profile,
			openAIContinuationTokenSecondary,
			callWithSafety,
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trajectory.newSafetyAcknowledgement(false); err == nil {
		t.Fatal("pending safety checks were acknowledged without confirmation")
	}
	confirmationArgs, required, err := trajectory.safetyConfirmationArguments()
	if err != nil || !required {
		t.Fatalf(
			"safetyConfirmationArguments = (%q, %v, %v)",
			confirmationArgs,
			required,
			err,
		)
	}
	var confirmation struct {
		Action              string                             `json:"action"`
		ResponseID          string                             `json:"response_id"`
		CallID              string                             `json:"call_id"`
		PendingSafetyChecks []client.OpenAIComputerSafetyCheck `json:"pending_safety_checks"`
	}
	if err := json.Unmarshal([]byte(confirmationArgs), &confirmation); err != nil {
		t.Fatalf("decode safety confirmation args: %v", err)
	}
	if confirmation.Action != "acknowledge_provider_safety_checks" ||
		confirmation.ResponseID != trajectory.responseID ||
		confirmation.CallID != trajectory.call.CallID ||
		!reflect.DeepEqual(
			confirmation.PendingSafetyChecks,
			trajectory.call.PendingSafetyChecks,
		) {
		t.Fatalf("safety confirmation args = %#v", confirmation)
	}
	acknowledgement, err := trajectory.newSafetyAcknowledgement(true)
	if err != nil {
		t.Fatalf("newSafetyAcknowledgement: %v", err)
	}

	tampered := trajectory.call
	tampered.PendingSafetyChecks = client.CloneOpenAIComputerSafetyChecks(
		trajectory.call.PendingSafetyChecks,
	)
	tampered.PendingSafetyChecks[0].Message = stringPointer(
		"different safety message",
	)
	if acknowledgement.ConsumeForExecution(
		profile,
		trajectory.responseID,
		tampered,
	) {
		t.Fatal("tampered safety check consumed acknowledgement")
	}
	if acknowledgement.ConsumeForExecution(
		profile,
		openAIContinuationTokenOther,
		trajectory.call,
	) {
		t.Fatal("cross-response safety acknowledgement was accepted")
	}
	if !acknowledgement.ConsumeForExecution(
		profile,
		trajectory.responseID,
		trajectory.call,
	) {
		t.Fatal("exact safety acknowledgement was rejected")
	}
	if acknowledgement.ConsumeForExecution(
		profile,
		trajectory.responseID,
		trajectory.call,
	) {
		t.Fatal("safety acknowledgement replay was accepted")
	}

	base := client.CompletionRequest{
		SpecificModel:            profile.Model(),
		ExecutionProfileID:       profile.ProfileID(),
		ResolvedExecutionProfile: profile,
	}
	screenshot := client.ContentBlock{
		Type: "image",
		Source: &client.ImageSource{
			Type: "base64", MediaType: "image/png", Data: "aW1hZ2U=",
		},
	}
	next, err := trajectory.BuildNextRequest(base, screenshot, acknowledgement)
	if err != nil {
		t.Fatalf("BuildNextRequest: %v", err)
	}
	result := next.Messages[len(next.Messages)-1].Content.Blocks()[0]
	if !reflect.DeepEqual(
		result.AcknowledgedSafetyChecks,
		trajectory.call.PendingSafetyChecks,
	) {
		t.Fatalf(
			"acknowledged checks = %#v, want %#v",
			result.AcknowledgedSafetyChecks,
			trajectory.call.PendingSafetyChecks,
		)
	}
	if _, err := trajectory.BuildNextRequest(
		base,
		screenshot,
		acknowledgement,
	); err == nil {
		t.Fatal("safety acknowledgement continuation replay was accepted")
	}
}

func stringPointer(value string) *string {
	return &value
}

func TestOpenAIComputerTrajectoryRejectsUntrustedOrMismatchedCallBeforeEnvelope(t *testing.T) {
	profile := resolveTrustedOpenAIComputerProfile(t, "gpt-5.6-sol")
	otherProfile := resolveTrustedOpenAIComputerProfile(t, "gpt-5.6-terra")
	tests := map[string]func(*client.CompletionResponse) *client.ExecutionProfile{
		"untrusted resolved profile": func(response *client.CompletionResponse) *client.ExecutionProfile {
			return response.ExecutionProfile
		},
		"missing echoed profile": func(response *client.CompletionResponse) *client.ExecutionProfile {
			response.ExecutionProfile = nil
			return profile
		},
		"mismatched echoed profile": func(response *client.CompletionResponse) *client.ExecutionProfile {
			response.ExecutionProfile = otherProfile
			response.Model = otherProfile.Model()
			return profile
		},
		"mismatched response provider": func(response *client.CompletionResponse) *client.ExecutionProfile {
			response.Provider = "anthropic"
			return profile
		},
		"missing response id": func(response *client.CompletionResponse) *client.ExecutionProfile {
			response.RequestID = ""
			return profile
		},
		"mismatched normalized response id": func(response *client.CompletionResponse) *client.ExecutionProfile {
			response.ContentBlocks[0].ResponseID = openAIContinuationTokenOther
			return profile
		},
		"missing provider provenance": func(response *client.CompletionResponse) *client.ExecutionProfile {
			response.ContentBlocks[0].Provider = ""
			return profile
		},
		"mismatched api surface": func(response *client.CompletionResponse) *client.ExecutionProfile {
			response.ContentBlocks[0].APISurface = client.APISurfaceOpenAIChatCompletions
			return profile
		},
		"mismatched tool contract": func(response *client.CompletionResponse) *client.ExecutionProfile {
			response.ContentBlocks[0].ToolContract = client.ToolContractKocoroComputerUseV1
			return profile
		},
		"missing call id": func(response *client.CompletionResponse) *client.ExecutionProfile {
			response.ContentBlocks[0].CallID = ""
			return profile
		},
		"missing actions": func(response *client.CompletionResponse) *client.ExecutionProfile {
			response.ContentBlocks[0].Actions = nil
			return profile
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			response := openAIComputerCompletionResponse(t, profile)
			runProfile := mutate(response)
			trajectory, err := newOpenAIComputerTrajectory(runProfile, response)
			if err == nil || trajectory != nil {
				t.Fatalf("invalid trajectory accepted: trajectory=%#v err=%v", trajectory, err)
			}
		})
	}
}

func TestOpenAIComputerTrajectoryNextRequestFailsClosedOnWrongContinuation(t *testing.T) {
	profile := resolveTrustedOpenAIComputerProfile(t, "gpt-5.6-sol")
	trajectory, err := newOpenAIComputerTrajectory(
		profile,
		openAIComputerCompletionResponse(t, profile),
	)
	if err != nil {
		t.Fatal(err)
	}
	validScreenshot := client.ContentBlock{
		Type: "image",
		Source: &client.ImageSource{
			Type: "base64", MediaType: "image/png", Data: "aW1hZ2U=",
		},
	}
	base := client.CompletionRequest{
		SpecificModel:            profile.Model(),
		ExecutionProfileID:       profile.ProfileID(),
		ResolvedExecutionProfile: profile,
	}

	t.Run("wrong previous response", func(t *testing.T) {
		request := base
		request.PreviousResponseID = openAIContinuationTokenOther
		acknowledgement, ackErr := trajectory.newSafetyAcknowledgement(false)
		if ackErr != nil || !acknowledgement.ConsumeForExecution(
			profile,
			trajectory.responseID,
			trajectory.call,
		) {
			t.Fatal("prepare safety acknowledgement")
		}
		if _, err := trajectory.BuildNextRequest(
			request,
			validScreenshot,
			acknowledgement,
		); err == nil {
			t.Fatal("mismatched previous_response_id accepted")
		}
	})
	t.Run("not an image", func(t *testing.T) {
		acknowledgement, ackErr := trajectory.newSafetyAcknowledgement(false)
		if ackErr != nil || !acknowledgement.ConsumeForExecution(
			profile,
			trajectory.responseID,
			trajectory.call,
		) {
			t.Fatal("prepare safety acknowledgement")
		}
		if _, err := trajectory.BuildNextRequest(
			base,
			client.ContentBlock{Type: "text", Text: "not a screenshot"},
			acknowledgement,
		); err == nil {
			t.Fatal("non-image tool result accepted")
		}
	})
	t.Run("invalid base64 screenshot", func(t *testing.T) {
		screenshot := validScreenshot
		source := *validScreenshot.Source
		source.Data = "not-base64%%%"
		screenshot.Source = &source
		acknowledgement, ackErr := trajectory.newSafetyAcknowledgement(false)
		if ackErr != nil || !acknowledgement.ConsumeForExecution(
			profile,
			trajectory.responseID,
			trajectory.call,
		) {
			t.Fatal("prepare safety acknowledgement")
		}
		if _, err := trajectory.BuildNextRequest(
			base,
			screenshot,
			acknowledgement,
		); err == nil {
			t.Fatal("invalid base64 screenshot accepted")
		}
	})
	t.Run("wrong execution profile", func(t *testing.T) {
		request := base
		request.ExecutionProfileID = "ep1_wrong"
		acknowledgement, ackErr := trajectory.newSafetyAcknowledgement(false)
		if ackErr != nil || !acknowledgement.ConsumeForExecution(
			profile,
			trajectory.responseID,
			trajectory.call,
		) {
			t.Fatal("prepare safety acknowledgement")
		}
		if _, err := trajectory.BuildNextRequest(
			request,
			validScreenshot,
			acknowledgement,
		); err == nil {
			t.Fatal("mismatched continuation execution profile accepted")
		}
	})
}

func requireAgentJSONSemanticEqual(t *testing.T, got, want []byte) {
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
