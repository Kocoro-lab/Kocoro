package executionprofile

import (
	"fmt"
	"strings"
)

// Mode is the provider-neutral execution intent carried across the Koe wire.
// Only fast is opt-in. Missing, malformed, and future values fail closed to
// full so an older daemon never silently routes work onto a weaker profile.
type Mode string

const (
	ModeFast Mode = "fast"
	ModeFull Mode = "full"
)

func NormalizeMode(value string) Mode {
	if strings.EqualFold(strings.TrimSpace(value), string(ModeFast)) {
		return ModeFast
	}
	return ModeFull
}

type ResponseCachePolicy string

const (
	ResponseCacheOff ResponseCachePolicy = "off"
)

const (
	FastCapability     = "koe_fast"
	FastSchemaVersion  = 1
	FastProfileVersion = 2
	FastProfileName    = "koe-fast-v1"
	FastToolContract   = "kocoro.function_tools.v1"

	ComputerCapability             = "computer_use"
	ComputerSchemaVersion          = 1
	ComputerContractRevision       = 1
	ComputerExecutionModeNative    = "native_computer"
	ComputerExecutionModeFunction  = "function_computer_use"
	AnthropicComputerToolContract  = "anthropic.computer_20251124"
	AnthropicComputerBetaContract  = "computer-use-2025-11-24"
	GenericComputerUseToolContract = "kocoro.computer_use.v1"
	AnthropicMessagesAPISurface    = "anthropic_messages"
)

// Profile is the immutable, provider-neutral execution contract for one run.
// Provider-native response ids and reasoning payloads must never be persisted
// here; only the Cloud-resolved routing decision is retained.
type Profile struct {
	RequestedMode        Mode                `json:"requested_mode"`
	EffectiveMode        Mode                `json:"effective_mode"`
	SchemaVersion        int                 `json:"schema_version,omitempty"`
	ContractRevision     int                 `json:"contract_revision,omitempty"`
	ProfileName          string              `json:"profile_name,omitempty"`
	ProfileVersion       int                 `json:"profile_version,omitempty"`
	ProfileID            string              `json:"profile_id,omitempty"`
	Provider             string              `json:"provider,omitempty"`
	Model                string              `json:"model,omitempty"`
	APISurface           string              `json:"api_surface,omitempty"`
	ExecutionMode        string              `json:"execution_mode,omitempty"`
	ToolContract         string              `json:"tool_contract,omitempty"`
	BetaContract         string              `json:"beta_contract,omitempty"`
	SupportsImageInput   bool                `json:"supports_image_input,omitempty"`
	SupportsToolImages   bool                `json:"supports_tool_result_images,omitempty"`
	SupportsFunctions    bool                `json:"supports_function_tools,omitempty"`
	SupportsBatchActions bool                `json:"supports_batched_actions,omitempty"`
	ReasoningEffort      string              `json:"reasoning_effort,omitempty"`
	ServiceTier          string              `json:"service_tier,omitempty"`
	ParallelToolCalls    bool                `json:"parallel_tool_calls,omitempty"`
	ResponseCachePolicy  ResponseCachePolicy `json:"response_cache_policy,omitempty"`
	ResolutionReason     string              `json:"resolution_reason"`
}

// ToolOutcomeEvidence is the durable, provider-neutral record of one tool
// outcome. It intentionally stores only hashes and decisions, never provider
// response ids, hidden reasoning, raw arguments, or raw tool output.
type ToolOutcomeEvidence struct {
	ToolCallID         string `json:"tool_call_id"`
	ToolName           string `json:"tool_name"`
	Validated          bool   `json:"validated"`
	Outcome            string `json:"outcome"`
	PermissionDecision string `json:"permission_decision,omitempty"`
	PermissionApproved bool   `json:"permission_approved,omitempty"`
	SideEffect         bool   `json:"side_effect,omitempty"`
	ArgumentsDigest    string `json:"arguments_digest,omitempty"`
	ResultDigest       string `json:"result_digest,omitempty"`
}

type DeliverableEvidence struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
	MIME     string `json:"mime,omitempty"`
	ByteSize int64  `json:"byte_size,omitempty"`
}

type Evidence struct {
	ToolOutcomes []ToolOutcomeEvidence `json:"tool_outcomes,omitempty"`
	Deliverables []DeliverableEvidence `json:"deliverables,omitempty"`
}

// ComputerActivation is the durable, provider-neutral overlay created only
// after tool_search selects computer use. It contains no provider response id,
// hidden reasoning, raw tool arguments, or tool output.
type ComputerActivation struct {
	Profile            Profile `json:"profile"`
	ToolName           string  `json:"tool_name"`
	ToolsetFingerprint string  `json:"toolset_fingerprint"`
}

// Run is the stable logical-task execution generation persisted at checkpoints.
// Profile fields are immutable after the run begins.
type Run struct {
	LogicalTaskID      string              `json:"logical_task_id,omitempty"`
	RunID              string              `json:"run_id,omitempty"`
	ParentRunID        string              `json:"parent_run_id,omitempty"`
	Profile            Profile             `json:"profile"`
	ComputerActivation *ComputerActivation `json:"computer_activation,omitempty"`
	Evidence           Evidence            `json:"evidence,omitempty"`
}

func FullProfile(requested Mode, reason string) Profile {
	return Profile{
		RequestedMode:    NormalizeMode(string(requested)),
		EffectiveMode:    ModeFull,
		ResolutionReason: reason,
	}
}

func (p Profile) IsFast() bool {
	return p.EffectiveMode == ModeFast
}

// ValidateFast verifies the exact public Cloud contract while keeping the
// profile id opaque. ShanClaw never manufactures or interprets a kfp1 id.
func (p Profile) ValidateFast() error {
	switch {
	case p.EffectiveMode != ModeFast:
		return fmt.Errorf("effective mode %q is not fast", p.EffectiveMode)
	case p.SchemaVersion != FastSchemaVersion:
		return fmt.Errorf("schema_version %d is not %d", p.SchemaVersion, FastSchemaVersion)
	case p.ProfileName != FastProfileName:
		return fmt.Errorf("profile_name %q is not %q", p.ProfileName, FastProfileName)
	case p.ProfileVersion != FastProfileVersion:
		return fmt.Errorf("profile_version %d is not %d", p.ProfileVersion, FastProfileVersion)
	case !strings.HasPrefix(p.ProfileID, "kfp1_") || len(p.ProfileID) <= len("kfp1_"):
		return fmt.Errorf("profile_id must be an opaque kfp1 id")
	case p.Provider != "openai":
		return fmt.Errorf("provider %q is not openai", p.Provider)
	case p.Model != "gpt-5.6-luna":
		return fmt.Errorf("model %q is not gpt-5.6-luna", p.Model)
	case p.APISurface != "openai_responses":
		return fmt.Errorf("api_surface %q is not openai_responses", p.APISurface)
	case p.ToolContract != FastToolContract:
		return fmt.Errorf("tool_contract %q is not %q", p.ToolContract, FastToolContract)
	case p.ReasoningEffort != "medium":
		return fmt.Errorf("reasoning_effort %q is not medium", p.ReasoningEffort)
	case p.ServiceTier != "fast":
		return fmt.Errorf("service_tier %q is not fast", p.ServiceTier)
	case !p.ParallelToolCalls:
		return fmt.Errorf("parallel_tool_calls must be true")
	case p.ResponseCachePolicy != ResponseCacheOff:
		return fmt.Errorf("response_cache_policy %q is not off", p.ResponseCachePolicy)
	default:
		return nil
	}
}

// ValidateComputer verifies the Cloud-minted, exact-route computer contract.
// ShanClaw currently implements Anthropic's native computer wire and the
// provider-neutral function-tool wire. OpenAI's native Responses continuation
// contract stays rejected until the agent loop has a complete adapter for it.
func (p Profile) ValidateComputer(requiredToolContract string) error {
	switch {
	case p.SchemaVersion != ComputerSchemaVersion:
		return fmt.Errorf("schema_version %d is not %d", p.SchemaVersion, ComputerSchemaVersion)
	case p.ContractRevision != ComputerContractRevision:
		return fmt.Errorf("contract_revision %d is not %d", p.ContractRevision, ComputerContractRevision)
	case !strings.HasPrefix(p.ProfileID, "ep1_") || len(p.ProfileID) <= len("ep1_"):
		return fmt.Errorf("profile_id must be an opaque ep1 id")
	case strings.TrimSpace(p.Provider) == "":
		return fmt.Errorf("provider is required")
	case p.Provider != strings.ToLower(strings.TrimSpace(p.Provider)):
		return fmt.Errorf("provider %q is not canonical", p.Provider)
	case strings.TrimSpace(p.Model) == "":
		return fmt.Errorf("model is required")
	case p.Model != strings.TrimSpace(p.Model):
		return fmt.Errorf("model %q is not canonical", p.Model)
	case strings.TrimSpace(p.APISurface) == "":
		return fmt.Errorf("api_surface is required")
	case requiredToolContract != "" && p.ToolContract != requiredToolContract:
		return fmt.Errorf("tool_contract %q is not %q", p.ToolContract, requiredToolContract)
	}

	switch p.ToolContract {
	case AnthropicComputerToolContract:
		switch {
		case p.Provider != "anthropic":
			return fmt.Errorf("provider %q is not anthropic", p.Provider)
		case p.APISurface != AnthropicMessagesAPISurface:
			return fmt.Errorf("api_surface %q is not %q", p.APISurface, AnthropicMessagesAPISurface)
		case p.ExecutionMode != ComputerExecutionModeNative:
			return fmt.Errorf("execution_mode %q is not %q", p.ExecutionMode, ComputerExecutionModeNative)
		case p.BetaContract != AnthropicComputerBetaContract:
			return fmt.Errorf("beta_contract %q is not %q", p.BetaContract, AnthropicComputerBetaContract)
		case !p.SupportsImageInput:
			return fmt.Errorf("supports_image_input must be true")
		case !p.SupportsToolImages:
			return fmt.Errorf("supports_tool_result_images must be true")
		case !p.SupportsFunctions:
			return fmt.Errorf("supports_function_tools must be true")
		case p.SupportsBatchActions:
			return fmt.Errorf("supports_batched_actions must be false")
		}
	case GenericComputerUseToolContract:
		switch {
		case p.ExecutionMode != ComputerExecutionModeFunction:
			return fmt.Errorf("execution_mode %q is not %q", p.ExecutionMode, ComputerExecutionModeFunction)
		case !p.SupportsFunctions:
			return fmt.Errorf("supports_function_tools must be true")
		case p.SupportsBatchActions:
			return fmt.Errorf("supports_batched_actions must be false")
		}
	default:
		return fmt.Errorf("unsupported computer tool_contract %q", p.ToolContract)
	}
	return nil
}

type ResolutionInput struct {
	RequestedMode Mode
	FastEnabled   bool
	CloudProfile  *Profile
	CloudError    error
}

// Resolve is deliberately pure. It is the single fail-closed policy for
// setting x requested mode x Cloud support. Full returns no model overrides,
// preserving the complete global/per-agent configuration already on the loop.
func Resolve(in ResolutionInput) Profile {
	requested := NormalizeMode(string(in.RequestedMode))
	if requested != ModeFast {
		return FullProfile(requested, "requested_full")
	}
	if !in.FastEnabled {
		return FullProfile(requested, "fast_setting_disabled")
	}
	if in.CloudError != nil {
		return FullProfile(requested, "cloud_resolver_failed")
	}
	if in.CloudProfile == nil {
		return FullProfile(requested, "cloud_profile_missing")
	}
	profile := *in.CloudProfile
	profile.RequestedMode = requested
	if err := profile.ValidateFast(); err != nil {
		return FullProfile(requested, "cloud_profile_invalid")
	}
	profile.ResolutionReason = "cloud_profile_resolved"
	return profile
}
