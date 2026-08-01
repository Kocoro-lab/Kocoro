package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	ExecutionProfileSchemaVersion      = 1
	ExecutionProfileContractRevision   = 1
	ExecutionProfileCapabilityComputer = "computer_use"

	NativeComputerProviderAnthropic       = "anthropic"
	APISurfaceAnthropicMessages           = "anthropic_messages"
	APISurfaceOpenAIResponses             = "openai_responses"
	APISurfaceOpenAIChatCompletions       = "openai_chat_completions"
	APISurfaceGoogleGenerateContent       = "google_generate_content"
	ExecutionModeNativeComputer           = "native_computer"
	ExecutionModeFunctionComputerUse      = "function_computer_use"
	ExecutionModeUnavailable              = "unavailable"
	ToolContractAnthropicComputer20251124 = "anthropic.computer_20251124"
	ToolContractOpenAIComputerV1          = "openai.computer.v1"
	ToolContractKocoroComputerUseV1       = "kocoro.computer_use.v1"
	AnthropicComputerBetaContract         = "computer-use-2025-11-24"

	// A resolved profile is currently under 1 KiB. This workload-specific
	// 64 KiB ceiling leaves ample room for contract revisions while preventing
	// a misconfigured or hostile resolve endpoint from forcing an unbounded
	// allocation. Completion bodies have different workloads and limits.
	maxExecutionProfileResolveResponseBytes = 64 * 1024
)

// ResolveExecutionProfileRequest asks Cloud to resolve one exact provider,
// model, API surface, and computer-use contract before Kocoro constructs its
// run-local registry. A model tier is routing intent, not an identity pin;
// Cloud always returns an exact model in the resolved profile.
type ResolveExecutionProfileRequest struct {
	SchemaVersion        int    `json:"schema_version"`
	ModelTier            string `json:"model_tier,omitempty"`
	SpecificModel        string `json:"specific_model,omitempty"`
	Capability           string `json:"capability"`
	RequiredToolContract string `json:"required_tool_contract,omitempty"`
	AllowModelFallback   bool   `json:"allow_model_fallback"`
}

// executionProfileWire is the canonical Cloud-owned profile. profile_id is
// ep1_ + SHA-256(compact sorted-key JSON of all other fields). Keep every
// field non-omitempty: null beta_contract participates in the fingerprint.
type executionProfileWire struct {
	SchemaVersion            int     `json:"schema_version"`
	ContractRevision         int     `json:"contract_revision"`
	ProfileID                string  `json:"profile_id"`
	Provider                 string  `json:"provider"`
	Model                    string  `json:"model"`
	APISurface               string  `json:"api_surface"`
	ExecutionMode            string  `json:"execution_mode"`
	ToolContract             string  `json:"tool_contract"`
	BetaContract             *string `json:"beta_contract"`
	SupportsImageInput       bool    `json:"supports_image_input"`
	SupportsToolResultImages bool    `json:"supports_tool_result_images"`
	SupportsFunctionTools    bool    `json:"supports_function_tools"`
	SupportsBatchedActions   bool    `json:"supports_batched_actions"`
}

// ExecutionProfile is deliberately sealed. JSON received on a completion
// response can be decoded into this type for comparison, but only
// GatewayClient.ResolveExecutionProfile can attach the trustedResolution seal
// required to select a provider-native run-local adapter.
type ExecutionProfile struct {
	wire              executionProfileWire
	trustedResolution *executionProfileResolutionSeal
}

type executionProfileResolutionSeal struct {
	marker byte
}

var trustedExecutionProfileResolutionSeal = &executionProfileResolutionSeal{marker: 1}

func (p ExecutionProfile) MarshalJSON() ([]byte, error) {
	return json.Marshal(p.wire)
}

func (p *ExecutionProfile) UnmarshalJSON(data []byte) error {
	if p == nil {
		return fmt.Errorf("cannot unmarshal execution profile into nil receiver")
	}
	if err := rejectDuplicateJSONMembers(data); err != nil {
		return executionProfileError(
			ExecutionProfileInvalid,
			"decode execution profile: %v",
			err,
		)
	}
	var wire executionProfileWire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	if err := validateExecutionProfileWire(wire); err != nil {
		return err
	}
	p.wire = wire
	p.trustedResolution = nil
	return nil
}

// rejectDuplicateJSONMembers scans one complete JSON value before typed
// decoding. encoding/json intentionally accepts duplicate object members, but
// execution profiles are versioned authorization records: allowing the last
// spelling to win makes their signed-looking canonical identity ambiguous.
//
// Token() returns decoded object keys, so escaped-equivalent spellings such as
// "provider" and "pro\u0076ider" compare equal. Recursing through every object
// keeps this guard valid if a later profile revision adds nested structures.
// decodeBoundedResolveResponse reads a /v1/completions/resolve response body
// with the same hardening as ResolveExecutionProfile: the 64 KiB ceiling,
// duplicate-member rejection, and unknown-field rejection. One endpoint, one
// codec — a misconfigured or hostile resolve endpoint must not be able to
// force an unbounded allocation or smuggle ambiguous duplicate members past
// any of the resolver entry points.
func decodeBoundedResolveResponse(body io.Reader, target any) error {
	raw, err := io.ReadAll(io.LimitReader(body, maxExecutionProfileResolveResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read resolve response: %w", err)
	}
	if len(raw) > maxExecutionProfileResolveResponseBytes {
		return fmt.Errorf("resolve response exceeds %d byte limit", maxExecutionProfileResolveResponseBytes)
	}
	if err := rejectDuplicateJSONMembers(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func rejectDuplicateJSONMembers(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanJSONValueForDuplicateMembers(decoder); err != nil {
		return err
	}
	if token, err := decoder.Token(); err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("unexpected trailing JSON value beginning with %v", token)
	}
	return nil
}

func scanJSONValueForDuplicateMembers(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, structured := token.(json.Delim)
	if !structured {
		return nil
	}

	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("JSON object member name is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON member %q", key)
			}
			seen[key] = struct{}{}
			if err := scanJSONValueForDuplicateMembers(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return fmt.Errorf("JSON object ended with %v", end)
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValueForDuplicateMembers(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return fmt.Errorf("JSON array ended with %v", end)
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
	return nil
}

func (p *ExecutionProfile) ProfileID() string {
	if p == nil {
		return ""
	}
	return p.wire.ProfileID
}

func (p *ExecutionProfile) Provider() string {
	if p == nil {
		return ""
	}
	return p.wire.Provider
}

func (p *ExecutionProfile) Model() string {
	if p == nil {
		return ""
	}
	return p.wire.Model
}

func (p *ExecutionProfile) APISurface() string {
	if p == nil {
		return ""
	}
	return p.wire.APISurface
}

func (p *ExecutionProfile) ExecutionMode() string {
	if p == nil {
		return ""
	}
	return p.wire.ExecutionMode
}

func (p *ExecutionProfile) ToolContract() string {
	if p == nil {
		return ""
	}
	return p.wire.ToolContract
}

func (p *ExecutionProfile) BetaContract() string {
	if p == nil || p.wire.BetaContract == nil {
		return ""
	}
	return *p.wire.BetaContract
}

func (p *ExecutionProfile) SupportsImageInput() bool {
	return p != nil && p.wire.SupportsImageInput
}

func (p *ExecutionProfile) SupportsToolResultImages() bool {
	return p != nil && p.wire.SupportsToolResultImages
}

func (p *ExecutionProfile) SupportsFunctionTools() bool {
	return p != nil && p.wire.SupportsFunctionTools
}

func (p *ExecutionProfile) SupportsBatchedActions() bool {
	return p != nil && p.wire.SupportsBatchedActions
}

func (p *ExecutionProfile) IsTrustedResolution() bool {
	return p != nil && p.trustedResolution == trustedExecutionProfileResolutionSeal
}

// MatchesExact reports whether two profiles have the same canonical Cloud
// wire contract. The trusted seal is intentionally excluded: completion
// echoes are decoded as untrusted and must match the separately resolved,
// trusted profile before any native call is admitted.
func (p *ExecutionProfile) MatchesExact(other *ExecutionProfile) bool {
	return p != nil && other != nil && equalExecutionProfileWire(p.wire, other.wire)
}

func admitResolvedExecutionProfile(wire executionProfileWire) (*ExecutionProfile, error) {
	if err := validateExecutionProfileWire(wire); err != nil {
		return nil, err
	}
	return &ExecutionProfile{
		wire:              wire,
		trustedResolution: trustedExecutionProfileResolutionSeal,
	}, nil
}

type ExecutionProfileErrorCode string

const (
	ExecutionProfileInvalid          ExecutionProfileErrorCode = "execution_profile_invalid"
	ExecutionProfileIDMismatch       ExecutionProfileErrorCode = "execution_profile_id_mismatch"
	ExecutionProfileUntrusted        ExecutionProfileErrorCode = "execution_profile_untrusted"
	ExecutionProfileRequired         ExecutionProfileErrorCode = "execution_profile_required"
	ExecutionProfileRequestMismatch  ExecutionProfileErrorCode = "execution_profile_request_mismatch"
	ExecutionProfileResponseMissing  ExecutionProfileErrorCode = "execution_profile_response_missing"
	ExecutionProfileResponseMismatch ExecutionProfileErrorCode = "execution_profile_response_mismatch"
)

type ExecutionProfileError struct {
	Code   ExecutionProfileErrorCode
	Detail string
}

func (e *ExecutionProfileError) Error() string {
	if e == nil {
		return "execution profile rejected"
	}
	if e.Detail == "" {
		return fmt.Sprintf("execution profile rejected: %s", e.Code)
	}
	return fmt.Sprintf("execution profile rejected: %s: %s", e.Code, e.Detail)
}

func executionProfileError(code ExecutionProfileErrorCode, format string, args ...any) error {
	return &ExecutionProfileError{
		Code:   code,
		Detail: fmt.Sprintf(format, args...),
	}
}

func validateResolveExecutionProfileRequest(req ResolveExecutionProfileRequest) error {
	if req.SchemaVersion != ExecutionProfileSchemaVersion {
		return executionProfileError(
			ExecutionProfileInvalid,
			"resolve schema_version %d is unsupported",
			req.SchemaVersion,
		)
	}
	if req.Capability != ExecutionProfileCapabilityComputer {
		return executionProfileError(
			ExecutionProfileInvalid,
			"resolve capability %q is unsupported",
			req.Capability,
		)
	}
	if req.RequiredToolContract != "" &&
		req.RequiredToolContract != ToolContractOpenAIComputerV1 {
		return executionProfileError(
			ExecutionProfileInvalid,
			"resolve required_tool_contract %q is unsupported",
			req.RequiredToolContract,
		)
	}
	if strings.TrimSpace(req.ModelTier) == "" && strings.TrimSpace(req.SpecificModel) == "" {
		return executionProfileError(
			ExecutionProfileInvalid,
			"resolve requires model_tier or specific_model",
		)
	}
	return nil
}

// ResolveExecutionProfile obtains the authenticated Cloud-owned contract that
// Kocoro must pin before selecting a run-local computer-use adapter.
func (c *GatewayClient) ResolveExecutionProfile(
	ctx context.Context,
	req ResolveExecutionProfileRequest,
) (*ExecutionProfile, error) {
	if err := validateResolveExecutionProfileRequest(req); err != nil {
		return nil, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal execution profile resolve request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.baseURL+"/v1/completions/resolve",
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("create execution profile resolve request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if key := c.getAPIKey(); key != "" {
		httpReq.Header.Set("X-API-Key", key)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("execution profile resolve failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{StatusCode: resp.StatusCode, Body: readResponseBody(resp)}
	}

	responseBody, err := io.ReadAll(io.LimitReader(
		resp.Body,
		maxExecutionProfileResolveResponseBytes+1,
	))
	if err != nil {
		return nil, executionProfileError(
			ExecutionProfileInvalid,
			"read resolve response: %v",
			err,
		)
	}
	if len(responseBody) > maxExecutionProfileResolveResponseBytes {
		return nil, executionProfileError(
			ExecutionProfileInvalid,
			"resolve response exceeds %d byte limit",
			maxExecutionProfileResolveResponseBytes,
		)
	}
	if err := rejectDuplicateJSONMembers(responseBody); err != nil {
		return nil, executionProfileError(
			ExecutionProfileInvalid,
			"decode resolve response: %v",
			err,
		)
	}
	var wire executionProfileWire
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return nil, executionProfileError(
			ExecutionProfileInvalid,
			"decode resolve response: %v",
			err,
		)
	}
	profile, err := admitResolvedExecutionProfile(wire)
	if err != nil {
		return nil, err
	}
	if specific := strings.TrimSpace(req.SpecificModel); specific != "" && profile.Model() != specific {
		return nil, executionProfileError(
			ExecutionProfileRequestMismatch,
			"resolved model %q does not match requested specific_model %q",
			profile.Model(),
			specific,
		)
	}
	return profile, nil
}

func validateExecutionProfileWire(wire executionProfileWire) error {
	if wire.SchemaVersion != ExecutionProfileSchemaVersion {
		return executionProfileError(
			ExecutionProfileInvalid,
			"schema_version %d is unsupported",
			wire.SchemaVersion,
		)
	}
	if wire.ContractRevision != ExecutionProfileContractRevision {
		return executionProfileError(
			ExecutionProfileInvalid,
			"contract_revision %d is unsupported",
			wire.ContractRevision,
		)
	}
	for name, value := range map[string]string{
		"profile_id":     wire.ProfileID,
		"provider":       wire.Provider,
		"model":          wire.Model,
		"api_surface":    wire.APISurface,
		"execution_mode": wire.ExecutionMode,
		"tool_contract":  wire.ToolContract,
	} {
		if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
			return executionProfileError(ExecutionProfileInvalid, "%s is empty or not canonical", name)
		}
	}

	switch wire.APISurface {
	case APISurfaceAnthropicMessages,
		APISurfaceOpenAIResponses,
		APISurfaceOpenAIChatCompletions,
		APISurfaceGoogleGenerateContent:
	default:
		return executionProfileError(
			ExecutionProfileInvalid,
			"api_surface %q is unsupported",
			wire.APISurface,
		)
	}

	switch wire.ExecutionMode {
	case ExecutionModeNativeComputer:
		switch wire.ToolContract {
		case ToolContractAnthropicComputer20251124:
			if wire.Provider != NativeComputerProviderAnthropic ||
				wire.APISurface != APISurfaceAnthropicMessages ||
				wire.BetaContract == nil ||
				*wire.BetaContract != AnthropicComputerBetaContract ||
				wire.SupportsBatchedActions ||
				!wire.SupportsFunctionTools {
				return executionProfileError(
					ExecutionProfileInvalid,
					"Anthropic native computer contract fields are inconsistent",
				)
			}
		case ToolContractOpenAIComputerV1:
			if wire.Provider != "openai" ||
				wire.APISurface != APISurfaceOpenAIResponses ||
				wire.BetaContract != nil ||
				!wire.SupportsBatchedActions ||
				wire.SupportsFunctionTools {
				return executionProfileError(
					ExecutionProfileInvalid,
					"OpenAI native computer contract fields are inconsistent",
				)
			}
		default:
			return executionProfileError(
				ExecutionProfileInvalid,
				"native tool_contract %q is unsupported",
				wire.ToolContract,
			)
		}
		if !wire.SupportsImageInput || !wire.SupportsToolResultImages {
			return executionProfileError(
				ExecutionProfileInvalid,
				"native computer profile is missing required capabilities",
			)
		}
	case ExecutionModeFunctionComputerUse:
		if wire.ToolContract != ToolContractKocoroComputerUseV1 ||
			wire.BetaContract != nil ||
			!wire.SupportsFunctionTools {
			return executionProfileError(
				ExecutionProfileInvalid,
				"function computer_use contract fields are inconsistent",
			)
		}
		if wire.SupportsToolResultImages && !wire.SupportsImageInput {
			return executionProfileError(
				ExecutionProfileInvalid,
				"tool-result images require image input support",
			)
		}
		if wire.SupportsBatchedActions {
			return executionProfileError(
				ExecutionProfileInvalid,
				"function computer_use does not use provider-native batched actions",
			)
		}
	case ExecutionModeUnavailable:
		if wire.SupportsBatchedActions {
			return executionProfileError(
				ExecutionProfileInvalid,
				"unavailable profile cannot support batched actions",
			)
		}
	default:
		return executionProfileError(
			ExecutionProfileInvalid,
			"execution_mode %q is unsupported",
			wire.ExecutionMode,
		)
	}

	expectedID, err := canonicalExecutionProfileID(wire)
	if err != nil {
		return executionProfileError(
			ExecutionProfileInvalid,
			"canonicalize profile: %v",
			err,
		)
	}
	if wire.ProfileID != expectedID {
		return executionProfileError(
			ExecutionProfileIDMismatch,
			"profile_id %q does not match canonical id %q",
			wire.ProfileID,
			expectedID,
		)
	}
	return nil
}

func canonicalExecutionProfileID(wire executionProfileWire) (string, error) {
	canonical := map[string]any{
		"schema_version":              wire.SchemaVersion,
		"contract_revision":           wire.ContractRevision,
		"provider":                    wire.Provider,
		"model":                       wire.Model,
		"api_surface":                 wire.APISurface,
		"execution_mode":              wire.ExecutionMode,
		"tool_contract":               wire.ToolContract,
		"beta_contract":               wire.BetaContract,
		"supports_image_input":        wire.SupportsImageInput,
		"supports_tool_result_images": wire.SupportsToolResultImages,
		"supports_function_tools":     wire.SupportsFunctionTools,
		"supports_batched_actions":    wire.SupportsBatchedActions,
	}
	data, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "ep1_" + hex.EncodeToString(sum[:]), nil
}

func requestNativeToolType(req CompletionRequest) (string, bool) {
	for _, tool := range req.Tools {
		if tool.Type == NativeComputerToolType ||
			tool.Type == OpenAINativeComputerToolType {
			return tool.Type, true
		}
	}
	return "", false
}

func requestHasFunctionComputerUse(req CompletionRequest) bool {
	for _, tool := range req.Tools {
		if tool.Type == "function" && tool.Function.Name == "computer_use" {
			return true
		}
	}
	return false
}

// validateProviderNativeExecution is the shared Complete / CompleteStream
// preflight. The name is retained because both call sites already use it, but
// the contract is now the Cloud canonical execution profile for native and
// generic computer-use modes.
func validateProviderNativeExecution(req CompletionRequest) error {
	nativeType, hasNative := requestNativeToolType(req)
	hasProfileID := strings.TrimSpace(req.ExecutionProfileID) != ""
	hasResolved := req.ResolvedExecutionProfile != nil

	if !hasNative && !hasProfileID && !hasResolved {
		return nil
	}
	if !hasProfileID || !hasResolved {
		return executionProfileError(
			ExecutionProfileRequired,
			"computer-use execution requires both execution_profile_id and trusted resolved profile",
		)
	}
	profile := req.ResolvedExecutionProfile
	if !profile.IsTrustedResolution() {
		return executionProfileError(
			ExecutionProfileUntrusted,
			"execution profile was not minted by authenticated resolve",
		)
	}
	if err := validateExecutionProfileWire(profile.wire); err != nil {
		return err
	}
	if req.ExecutionProfileID != profile.ProfileID() {
		return executionProfileError(
			ExecutionProfileRequestMismatch,
			"request profile id %q does not match resolved profile id %q",
			req.ExecutionProfileID,
			profile.ProfileID(),
		)
	}
	if strings.TrimSpace(req.SpecificModel) == "" || req.SpecificModel != profile.Model() {
		return executionProfileError(
			ExecutionProfileRequestMismatch,
			"specific_model %q does not match resolved model %q",
			req.SpecificModel,
			profile.Model(),
		)
	}

	switch profile.ExecutionMode() {
	case ExecutionModeNativeComputer:
		if requestHasFunctionComputerUse(req) {
			return executionProfileError(
				ExecutionProfileRequestMismatch,
				"native profile cannot accompany the generic computer_use function schema",
			)
		}
		switch profile.ToolContract() {
		case ToolContractAnthropicComputer20251124:
			if !hasNative || nativeType != NativeComputerToolType ||
				len(req.Tools) == 0 {
				return executionProfileError(
					ExecutionProfileRequestMismatch,
					"Anthropic native profile requires its computer schema",
				)
			}
			for _, tool := range req.Tools {
				if tool.Type == NativeComputerToolType && tool.Name != NativeComputerToolName {
					return executionProfileError(
						ExecutionProfileRequestMismatch,
						"native tool name %q does not match %q",
						tool.Name,
						NativeComputerToolName,
					)
				}
			}
		case ToolContractOpenAIComputerV1:
			if len(req.Tools) != 1 ||
				req.Tools[0].Type != OpenAINativeComputerToolType {
				return executionProfileError(
					ExecutionProfileRequestMismatch,
					"OpenAI native profile requires exactly one Responses computer schema",
				)
			}
		default:
			return executionProfileError(
				ExecutionProfileRequestMismatch,
				"native tool contract %q has no Kocoro executor in this build",
				profile.ToolContract(),
			)
		}
	case ExecutionModeFunctionComputerUse:
		if hasNative {
			return executionProfileError(
				ExecutionProfileRequestMismatch,
				"function profile forbids provider-native computer schema",
			)
		}
	case ExecutionModeUnavailable:
		return executionProfileError(
			ExecutionProfileRequestMismatch,
			"unavailable execution profile cannot authorize computer use",
		)
	}
	return nil
}

// validateProviderNativeResponse withholds the response unless Cloud echoes
// the exact canonical profile resolved before this run. This executes in both
// Complete and CompleteStream before any returned tool call can reach the
// agent executor.
func validateProviderNativeResponse(req CompletionRequest, resp *CompletionResponse) error {
	if strings.TrimSpace(req.ExecutionProfileID) == "" && req.ResolvedExecutionProfile == nil {
		return nil
	}
	if req.ResolvedExecutionProfile == nil {
		return executionProfileError(
			ExecutionProfileRequired,
			"response validation has no trusted resolved profile",
		)
	}
	if resp == nil || resp.ExecutionProfile == nil {
		return executionProfileError(
			ExecutionProfileResponseMissing,
			"completion response did not echo execution_profile",
		)
	}
	if err := validateExecutionProfileWire(resp.ExecutionProfile.wire); err != nil {
		return err
	}
	if !equalExecutionProfileWire(req.ResolvedExecutionProfile.wire, resp.ExecutionProfile.wire) {
		return executionProfileError(
			ExecutionProfileResponseMismatch,
			"completion execution_profile does not match resolved profile %q",
			req.ExecutionProfileID,
		)
	}
	if resp.Provider != resp.ExecutionProfile.Provider() || resp.Model != resp.ExecutionProfile.Model() {
		return executionProfileError(
			ExecutionProfileResponseMismatch,
			"response provider/model %q/%q does not match echoed profile %q/%q",
			resp.Provider,
			resp.Model,
			resp.ExecutionProfile.Provider(),
			resp.ExecutionProfile.Model(),
		)
	}
	return nil
}

func equalExecutionProfileWire(left, right executionProfileWire) bool {
	if left.SchemaVersion != right.SchemaVersion ||
		left.ContractRevision != right.ContractRevision ||
		left.ProfileID != right.ProfileID ||
		left.Provider != right.Provider ||
		left.Model != right.Model ||
		left.APISurface != right.APISurface ||
		left.ExecutionMode != right.ExecutionMode ||
		left.ToolContract != right.ToolContract ||
		left.SupportsImageInput != right.SupportsImageInput ||
		left.SupportsToolResultImages != right.SupportsToolResultImages ||
		left.SupportsFunctionTools != right.SupportsFunctionTools ||
		left.SupportsBatchedActions != right.SupportsBatchedActions {
		return false
	}
	if left.BetaContract == nil || right.BetaContract == nil {
		return left.BetaContract == nil && right.BetaContract == nil
	}
	return *left.BetaContract == *right.BetaContract
}
