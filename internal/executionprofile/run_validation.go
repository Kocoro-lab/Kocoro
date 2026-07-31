package executionprofile

import (
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidPersistedRun marks an execution generation that is unsafe to
// restore. Callers must fail closed instead of resolving a replacement profile:
// a replacement could replay checkpointed side effects on another model.
var ErrInvalidPersistedRun = errors.New("invalid persisted execution run")

// ValidateFull verifies that a full profile is the provider-neutral no-override
// shape minted by FullProfile. The resolution reason remains opaque so a newer
// daemon can add a reason without making otherwise safe checkpoints unreadable.
func (p Profile) ValidateFull() error {
	if p.RequestedMode != ModeFast && p.RequestedMode != ModeFull {
		return fmt.Errorf("requested mode %q is not canonical", p.RequestedMode)
	}
	if p.EffectiveMode != ModeFull {
		return fmt.Errorf("effective mode %q is not full", p.EffectiveMode)
	}
	if p.ResolutionReason == "" || p.ResolutionReason != strings.TrimSpace(p.ResolutionReason) {
		return fmt.Errorf("resolution_reason must be a non-empty canonical value")
	}
	if p != FullProfile(p.RequestedMode, p.ResolutionReason) {
		return fmt.Errorf("full profile contains execution overrides")
	}
	return nil
}

// ValidatePersisted verifies the complete provider-neutral contract that may be
// restored from a Koe checkpoint. It deliberately validates the run as a unit:
// individually valid kfp1 and ep1 profiles are still an invalid combination.
func (r Run) ValidatePersisted() error {
	if r.RunID == "" || r.RunID != strings.TrimSpace(r.RunID) {
		return invalidPersistedRunf("run_id must be a non-empty canonical value")
	}
	if r.LogicalTaskID != strings.TrimSpace(r.LogicalTaskID) {
		return invalidPersistedRunf("logical_task_id is not canonical")
	}
	if r.ParentRunID != strings.TrimSpace(r.ParentRunID) {
		return invalidPersistedRunf("parent_run_id is not canonical")
	}
	if r.ParentRunID != "" && r.ParentRunID == r.RunID {
		return invalidPersistedRunf("parent_run_id must differ from run_id")
	}

	switch r.Profile.EffectiveMode {
	case ModeFast:
		if r.Profile.RequestedMode != ModeFast {
			return invalidPersistedRunf(
				"fast profile requested mode %q is not fast",
				r.Profile.RequestedMode,
			)
		}
		if err := r.Profile.ValidateFast(); err != nil {
			return invalidPersistedRunf("invalid fast profile: %v", err)
		}
		if r.Profile.ResolutionReason != "cloud_profile_resolved" {
			return invalidPersistedRunf(
				"fast profile resolution_reason %q is not cloud_profile_resolved",
				r.Profile.ResolutionReason,
			)
		}
		if r.ComputerActivation != nil {
			return invalidPersistedRunf("fast run cannot contain a computer activation")
		}
	case ModeFull:
		if err := r.Profile.ValidateFull(); err != nil {
			return invalidPersistedRunf("invalid full profile: %v", err)
		}
		if r.ComputerActivation != nil {
			if err := r.ComputerActivation.validatePersisted(); err != nil {
				return invalidPersistedRunf("invalid computer activation: %v", err)
			}
		}
	default:
		return invalidPersistedRunf(
			"effective mode %q is neither fast nor full",
			r.Profile.EffectiveMode,
		)
	}
	for _, outcome := range r.Evidence.ToolOutcomes {
		if outcome.SideEffect && outcome.Validated &&
			outcome.Outcome == "succeeded" &&
			!isLowerSHA256Hex(outcome.ArgumentsDigest) {
			return invalidPersistedRunf(
				"successful side-effect outcome %q lacks a canonical arguments digest",
				outcome.ToolCallID,
			)
		}
	}
	return nil
}

func (a ComputerActivation) validatePersisted() error {
	if a.Profile.RequestedMode != ModeFull || a.Profile.EffectiveMode != ModeFull {
		return fmt.Errorf(
			"computer profile modes must both be full, got requested=%q effective=%q",
			a.Profile.RequestedMode,
			a.Profile.EffectiveMode,
		)
	}
	if a.Profile.ResolutionReason != "cloud_computer_profile_resolved" {
		return fmt.Errorf(
			"computer profile resolution_reason %q is not cloud_computer_profile_resolved",
			a.Profile.ResolutionReason,
		)
	}
	if a.Profile.ProfileName != "" ||
		a.Profile.ProfileVersion != 0 ||
		a.Profile.ReasoningEffort != "" ||
		a.Profile.ServiceTier != "" ||
		a.Profile.ParallelToolCalls ||
		a.Profile.ResponseCachePolicy != "" {
		return fmt.Errorf("computer profile contains fast/full execution overrides")
	}
	if err := a.Profile.ValidateComputer(a.Profile.ToolContract); err != nil {
		return fmt.Errorf("invalid computer profile: %w", err)
	}

	var expectedToolName string
	switch a.Profile.ToolContract {
	case AnthropicComputerToolContract:
		expectedToolName = "computer"
	case GenericComputerUseToolContract:
		expectedToolName = "computer_use"
	default:
		// ValidateComputer already rejects this. Keep the branch defensive so a
		// future contract cannot accidentally restore under an empty tool name.
		return fmt.Errorf("unsupported computer tool_contract %q", a.Profile.ToolContract)
	}
	if a.ToolName != expectedToolName {
		return fmt.Errorf(
			"computer tool %q does not match contract %q (want %q)",
			a.ToolName,
			a.Profile.ToolContract,
			expectedToolName,
		)
	}
	if !isLowerSHA256Hex(a.ToolsetFingerprint) {
		return fmt.Errorf("computer toolset fingerprint must be a lowercase SHA-256 digest")
	}
	return nil
}

func invalidPersistedRunf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidPersistedRun, fmt.Sprintf(format, args...))
}

func isLowerSHA256Hex(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
