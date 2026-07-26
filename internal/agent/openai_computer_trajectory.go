package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// OpenAIComputerBatchExecution is the narrow result returned to AgentLoop.
// Result must contain exactly one final screenshot. ContinuationAllowed means
// the provider may inspect that fresh state before choosing the next step,
// including recovery after a partial or uncertain batch.
type OpenAIComputerBatchExecution struct {
	CallID              string
	ContinuationAllowed bool
	Result              ToolResult
}

// OpenAIComputerBatchExecutor is installed by the daemon only after it owns a
// real daemonGUIWorkflow. The profile and Cloud-issued opaque continuation
// token let the implementation mint process-local provenance before decoding
// and prevalidating the entire action list. Generic function-tool profiles
// never satisfy AgentLoop's trusted OpenAI trajectory admission and therefore
// cannot reach this seam.
type OpenAIComputerBatchExecutor interface {
	ExecuteOpenAIComputerBatch(
		context.Context,
		*client.ExecutionProfile,
		string,
		json.RawMessage,
		*OpenAIComputerSafetyAcknowledgement,
	) (OpenAIComputerBatchExecution, error)
}

// openAIComputerBatchEnvelope is the internal handoff to the daemon-private
// OpenAI adapter. AdapterPayload deliberately contains only Cloud's normalized
// computer_call object; ResponseID remains trajectory metadata used to
// construct the next Responses API request.
type openAIComputerBatchEnvelope struct {
	ResponseID string
	Call       client.OpenAIComputerCall
}

func (envelope openAIComputerBatchEnvelope) AdapterPayload() (json.RawMessage, error) {
	payload, err := json.Marshal(envelope.Call)
	if err != nil {
		return nil, fmt.Errorf("marshal OpenAI computer batch envelope: %w", err)
	}
	return json.RawMessage(payload), nil
}

// openAIComputerTrajectory binds one trusted profile, its exact Cloud echo,
// the opaque continuation token, and the assistant computer_call turn.
// AgentLoop consumes it through a daemon-installed callback; the tools-layer
// readiness gate admits only the fully guarded daemon batch path.
type openAIComputerTrajectory struct {
	profile    *client.ExecutionProfile
	responseID string
	call       client.OpenAIComputerCall
	assistant  client.Message
}

func newOpenAIComputerTrajectory(
	profile *client.ExecutionProfile,
	response *client.CompletionResponse,
) (*openAIComputerTrajectory, error) {
	if err := validateTrustedOpenAIComputerProfile(profile); err != nil {
		return nil, err
	}
	if response == nil {
		return nil, fmt.Errorf("OpenAI computer trajectory requires a completion response")
	}
	if response.ExecutionProfile == nil {
		return nil, fmt.Errorf("OpenAI computer trajectory response is missing execution_profile")
	}
	if !profile.MatchesExact(response.ExecutionProfile) {
		return nil, fmt.Errorf("OpenAI computer trajectory execution_profile echo mismatch")
	}
	if response.Provider != profile.Provider() || response.Model != profile.Model() {
		return nil, fmt.Errorf(
			"OpenAI computer trajectory response provider/model %q/%q does not match profile %q/%q",
			response.Provider,
			response.Model,
			profile.Provider(),
			profile.Model(),
		)
	}
	if !validOpenAIResponseID(response.RequestID) {
		return nil, fmt.Errorf("OpenAI computer trajectory response id is missing or invalid")
	}

	var call client.OpenAIComputerCall
	callCount := 0
	for _, block := range response.ContentBlocks {
		if block.Type != client.OpenAIComputerCallType {
			continue
		}
		decoded, err := block.NormalizedOpenAIComputerCall()
		if err != nil {
			return nil, fmt.Errorf("OpenAI computer trajectory call: %w", err)
		}
		call = decoded
		callCount++
	}
	if callCount != 1 {
		return nil, fmt.Errorf(
			"OpenAI computer trajectory requires exactly one computer_call, got %d",
			callCount,
		)
	}
	if call.ResponseID != response.RequestID {
		return nil, fmt.Errorf(
			"OpenAI computer trajectory normalized response_id mismatch",
		)
	}

	assistant, err := cloneOpenAIComputerTrajectoryMessage(
		buildAssistantMessage(response, response.OutputText),
	)
	if err != nil {
		return nil, err
	}
	return &openAIComputerTrajectory{
		profile:    profile,
		responseID: response.RequestID,
		call:       call,
		assistant:  assistant,
	}, nil
}

func (trajectory *openAIComputerTrajectory) BatchEnvelope() openAIComputerBatchEnvelope {
	if trajectory == nil {
		return openAIComputerBatchEnvelope{}
	}
	call := trajectory.call
	call.Actions = append(json.RawMessage(nil), trajectory.call.Actions...)
	call.PendingSafetyChecks = client.CloneOpenAIComputerSafetyChecks(
		trajectory.call.PendingSafetyChecks,
	)
	return openAIComputerBatchEnvelope{
		ResponseID: trajectory.responseID,
		Call:       call,
	}
}

func (trajectory *openAIComputerTrajectory) BuildNextRequest(
	base client.CompletionRequest,
	screenshot client.ContentBlock,
	acknowledgement *OpenAIComputerSafetyAcknowledgement,
) (client.CompletionRequest, error) {
	return trajectory.buildNextRequest(base, screenshot, false, acknowledgement)
}

func (trajectory *openAIComputerTrajectory) buildNextRequest(
	base client.CompletionRequest,
	screenshot client.ContentBlock,
	isError bool,
	acknowledgement *OpenAIComputerSafetyAcknowledgement,
) (client.CompletionRequest, error) {
	if trajectory == nil || trajectory.profile == nil {
		return client.CompletionRequest{}, fmt.Errorf("OpenAI computer trajectory is unavailable")
	}
	if !trajectory.profile.IsTrustedResolution() {
		return client.CompletionRequest{}, fmt.Errorf("OpenAI computer trajectory profile is untrusted")
	}
	if base.ResolvedExecutionProfile == nil ||
		!base.ResolvedExecutionProfile.IsTrustedResolution() ||
		!trajectory.profile.MatchesExact(base.ResolvedExecutionProfile) {
		return client.CompletionRequest{}, fmt.Errorf(
			"OpenAI computer continuation requires the same trusted execution profile",
		)
	}
	if base.ExecutionProfileID != trajectory.profile.ProfileID() {
		return client.CompletionRequest{}, fmt.Errorf(
			"OpenAI computer continuation execution_profile_id mismatch",
		)
	}
	if base.SpecificModel != trajectory.profile.Model() {
		return client.CompletionRequest{}, fmt.Errorf(
			"OpenAI computer continuation specific_model mismatch",
		)
	}
	if base.PreviousResponseID != "" && base.PreviousResponseID != trajectory.responseID {
		return client.CompletionRequest{}, fmt.Errorf(
			"OpenAI computer continuation previous_response_id mismatch",
		)
	}
	if err := validateOpenAIComputerScreenshot(screenshot); err != nil {
		return client.CompletionRequest{}, err
	}
	acknowledgedChecks, err := acknowledgement.takeForContinuation(
		trajectory.profile,
		trajectory.responseID,
		trajectory.call,
	)
	if err != nil {
		return client.CompletionRequest{}, err
	}

	assistant, err := cloneOpenAIComputerTrajectoryMessage(trajectory.assistant)
	if err != nil {
		return client.CompletionRequest{}, err
	}
	screenshotCopy := screenshot
	sourceCopy := *screenshot.Source
	screenshotCopy.Source = &sourceCopy

	next := base
	next.Messages = make([]client.Message, 0, len(base.Messages)+2)
	next.Messages = append(next.Messages, base.Messages...)
	next.Messages = append(next.Messages, assistant)
	resultBlock := client.NewToolResultBlockWithBlocks(
		trajectory.call.CallID,
		[]client.ContentBlock{screenshotCopy},
		isError,
	)
	if len(acknowledgedChecks) > 0 {
		resultBlock.AcknowledgedSafetyChecks =
			client.CloneOpenAIComputerSafetyChecks(acknowledgedChecks)
	}
	next.Messages = append(next.Messages, client.Message{
		Role: "user",
		Content: client.NewBlockContent([]client.ContentBlock{
			resultBlock,
		}),
	})
	// Only the first private-computer request is forced to use the native tool.
	// A continuation must be allowed to finish with text once the requested end
	// state is reached; carrying "any" forward would force another unnecessary
	// computer_call forever.
	next.ToolChoice = nil
	next.PreviousResponseID = trajectory.responseID
	return next, nil
}

func responseHasOpenAIComputerCall(response *client.CompletionResponse) bool {
	if response == nil {
		return false
	}
	for _, block := range response.ContentBlocks {
		if block.Type == client.OpenAIComputerCallType {
			return true
		}
	}
	return false
}

func validateOpenAIComputerResponseAliases(
	response *client.CompletionResponse,
	call client.OpenAIComputerCall,
) error {
	if response == nil {
		return fmt.Errorf("OpenAI computer response is unavailable")
	}
	aliases := make([]client.FunctionCall, 0, len(response.ToolCalls)+1)
	if response.FunctionCall != nil {
		aliases = append(aliases, *response.FunctionCall)
	}
	aliases = append(aliases, response.ToolCalls...)
	for _, alias := range aliases {
		if err := validateOpenAIComputerResponseAlias(alias, call); err != nil {
			return err
		}
	}
	return nil
}

func validateOpenAIComputerResponseAlias(
	alias client.FunctionCall,
	call client.OpenAIComputerCall,
) error {
	if alias.Type != client.OpenAIComputerCallType ||
		alias.Provider != client.OpenAIComputerProvider ||
		alias.APISurface != client.APISurfaceOpenAIResponses ||
		alias.ToolContract != client.ToolContractOpenAIComputerV1 ||
		alias.Name != client.NativeComputerToolName ||
		alias.ResponseID != call.ResponseID ||
		!reflect.DeepEqual(
			alias.PendingSafetyChecks,
			call.PendingSafetyChecks,
		) {
		return fmt.Errorf("OpenAI computer response contains an incompatible tool-call alias")
	}
	if alias.ID != "" && alias.ID != call.CallID ||
		alias.CallID != "" && alias.CallID != call.CallID ||
		alias.ID == "" && alias.CallID == "" {
		return fmt.Errorf("OpenAI computer response alias call_id mismatch")
	}
	var arguments struct {
		Actions             json.RawMessage                    `json:"actions"`
		ResponseID          string                             `json:"response_id"`
		PendingSafetyChecks []client.OpenAIComputerSafetyCheck `json:"pending_safety_checks"`
	}
	decoder := json.NewDecoder(strings.NewReader(alias.ArgumentsString()))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(&arguments); err != nil {
		return fmt.Errorf("OpenAI computer response alias arguments are invalid")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("OpenAI computer response alias arguments are invalid")
	}
	if !equalOpenAIComputerJSON(arguments.Actions, call.Actions) {
		return fmt.Errorf("OpenAI computer response alias actions mismatch")
	}
	if arguments.ResponseID != call.ResponseID ||
		!reflect.DeepEqual(
			arguments.PendingSafetyChecks,
			call.PendingSafetyChecks,
		) {
		return fmt.Errorf("OpenAI computer response alias safety provenance mismatch")
	}
	return nil
}

func equalOpenAIComputerJSON(left, right []byte) bool {
	decode := func(value []byte) (any, bool) {
		decoder := json.NewDecoder(bytes.NewReader(value))
		decoder.UseNumber()
		var decoded any
		if err := decoder.Decode(&decoded); err != nil {
			return nil, false
		}
		if _, err := decoder.Token(); err != io.EOF {
			return nil, false
		}
		return decoded, true
	}
	leftValue, leftOK := decode(left)
	rightValue, rightOK := decode(right)
	return leftOK && rightOK && reflect.DeepEqual(leftValue, rightValue)
}

func stripPriorOpenAIComputerPairs(messages []client.Message) []client.Message {
	callIDs := make(map[string]struct{})
	for _, message := range messages {
		for _, block := range message.Content.Blocks() {
			if block.Type == client.OpenAIComputerCallType && block.CallID != "" {
				callIDs[block.CallID] = struct{}{}
			}
		}
	}
	if len(callIDs) == 0 {
		return messages
	}

	stripped := make([]client.Message, 0, len(messages))
	for _, message := range messages {
		if !message.Content.HasBlocks() {
			stripped = append(stripped, message)
			continue
		}
		blocks := message.Content.Blocks()
		kept := make([]client.ContentBlock, 0, len(blocks))
		for _, block := range blocks {
			if block.Type == client.OpenAIComputerCallType {
				continue
			}
			if block.Type == "tool_result" {
				if _, paired := callIDs[block.ToolUseID]; paired {
					continue
				}
			}
			kept = append(kept, block)
		}
		if len(kept) == 0 {
			continue
		}
		copy := message
		copy.Content = client.NewBlockContent(kept)
		stripped = append(stripped, copy)
	}
	return stripped
}

func cloneOpenAIComputerBaseRequest(
	request client.CompletionRequest,
) client.CompletionRequest {
	cloned := request
	cloned.Messages = cloneMessages(request.Messages)
	cloned.Tools = append([]client.Tool(nil), request.Tools...)
	cloned.PreviousResponseID = ""
	return cloned
}

func validateOpenAIComputerExecution(
	trajectory *openAIComputerTrajectory,
	execution OpenAIComputerBatchExecution,
	executeErr error,
) (client.ContentBlock, error) {
	if executeErr != nil {
		return client.ContentBlock{}, fmt.Errorf("OpenAI computer batch execution failed")
	}
	if trajectory == nil || execution.CallID == "" ||
		execution.CallID != trajectory.call.CallID {
		return client.ContentBlock{}, fmt.Errorf("OpenAI computer batch call_id mismatch")
	}
	if !execution.ContinuationAllowed {
		return client.ContentBlock{}, fmt.Errorf(
			"OpenAI computer batch has no verified state for continuation",
		)
	}
	if len(execution.Result.Images) != 1 {
		return client.ContentBlock{}, fmt.Errorf(
			"OpenAI computer batch requires exactly one final screenshot",
		)
	}
	image := execution.Result.Images[0]
	screenshot := client.ContentBlock{
		Type: "image",
		Source: &client.ImageSource{
			Type:      "base64",
			MediaType: image.MediaType,
			Data:      image.Data,
		},
	}
	if err := validateOpenAIComputerScreenshot(screenshot); err != nil {
		return client.ContentBlock{}, err
	}
	return screenshot, nil
}

func validateTrustedOpenAIComputerProfile(profile *client.ExecutionProfile) error {
	if profile == nil || !profile.IsTrustedResolution() {
		return fmt.Errorf("OpenAI computer trajectory requires a trusted resolved profile")
	}
	if profile.Provider() != client.OpenAIComputerProvider ||
		profile.APISurface() != client.APISurfaceOpenAIResponses ||
		profile.ExecutionMode() != client.ExecutionModeNativeComputer ||
		profile.ToolContract() != client.ToolContractOpenAIComputerV1 ||
		!profile.SupportsImageInput() ||
		!profile.SupportsToolResultImages() ||
		profile.SupportsFunctionTools() ||
		!profile.SupportsBatchedActions() {
		return fmt.Errorf("OpenAI computer trajectory profile contract is unsupported")
	}
	return nil
}

func validOpenAIResponseID(value string) bool {
	return client.ValidOpenAIComputerContinuationToken(value)
}

func validateOpenAIComputerScreenshot(screenshot client.ContentBlock) error {
	if screenshot.Type != "image" || screenshot.Source == nil {
		return fmt.Errorf("OpenAI computer continuation requires one screenshot image block")
	}
	if screenshot.Source.Type != "base64" ||
		!strings.HasPrefix(screenshot.Source.MediaType, "image/") ||
		strings.TrimSpace(screenshot.Source.Data) == "" {
		return fmt.Errorf("OpenAI computer continuation screenshot source is invalid")
	}
	if len(screenshot.Source.Data) > client.MaxInlineImageBase64Bytes {
		return fmt.Errorf("OpenAI computer continuation screenshot exceeds inline image limit")
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(screenshot.Source.Data)
	if err != nil ||
		base64.StdEncoding.EncodeToString(decoded) != screenshot.Source.Data {
		return fmt.Errorf("OpenAI computer continuation screenshot data is not canonical base64")
	}
	return nil
}

func cloneOpenAIComputerTrajectoryMessage(message client.Message) (client.Message, error) {
	wire, err := json.Marshal(message)
	if err != nil {
		return client.Message{}, fmt.Errorf("marshal OpenAI computer trajectory message: %w", err)
	}
	var clone client.Message
	if err := json.Unmarshal(wire, &clone); err != nil {
		return client.Message{}, fmt.Errorf("clone OpenAI computer trajectory message: %w", err)
	}
	return clone, nil
}
