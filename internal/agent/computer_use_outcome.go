package agent

import "fmt"

// ComputerUseCommitEffect is the task-level, monotonic summary of desktop
// mutations. It is daemon-internal and must never be inferred from model text.
type ComputerUseCommitEffect string

const (
	ComputerUseCommitNone    ComputerUseCommitEffect = "none"
	ComputerUseCommitKnown   ComputerUseCommitEffect = "committed"
	ComputerUseCommitUnknown ComputerUseCommitEffect = "unknown"
)

// MergeComputerUseCommitEffect keeps the strongest evidence seen over the
// entire task: none < committed < unknown.
func MergeComputerUseCommitEffect(
	left ComputerUseCommitEffect,
	right ComputerUseCommitEffect,
) ComputerUseCommitEffect {
	rank := func(effect ComputerUseCommitEffect) int {
		switch effect {
		case ComputerUseCommitKnown:
			return 1
		case ComputerUseCommitUnknown:
			return 2
		default:
			return 0
		}
	}
	if rank(right) > rank(left) {
		return right
	}
	if left == "" {
		return ComputerUseCommitNone
	}
	return left
}

type ComputerUseTaskStatus string

const (
	ComputerUseTaskCompleted    ComputerUseTaskStatus = "completed"
	ComputerUseTaskNotCompleted ComputerUseTaskStatus = "not_completed"
	ComputerUseTaskUnverified   ComputerUseTaskStatus = "unverified"
)

type ComputerUseTaskRecovery string

const (
	ComputerUseRecoveryNone             ComputerUseTaskRecovery = ""
	ComputerUseRecoveryRetryWithApps    ComputerUseTaskRecovery = "retry_with_apps"
	ComputerUseRecoveryAlternateControl ComputerUseTaskRecovery = "alternate_control"
)

// ComputerUseTaskOutcome is the structured daemon-to-AgentLoop completion
// contract. It stays out of provider-visible serialization but lets the outer
// loop enforce unknown-commit boundaries without parsing prose or IsError.
type ComputerUseTaskOutcome struct {
	Status      ComputerUseTaskStatus
	Effect      ComputerUseCommitEffect
	FailureCode string
	Recovery    ComputerUseTaskRecovery
}

func (outcome ComputerUseTaskOutcome) Validate() error {
	switch outcome.Status {
	case ComputerUseTaskCompleted,
		ComputerUseTaskNotCompleted,
		ComputerUseTaskUnverified:
	default:
		return fmt.Errorf("computer-use task status %q is invalid", outcome.Status)
	}
	switch outcome.Effect {
	case ComputerUseCommitNone,
		ComputerUseCommitKnown,
		ComputerUseCommitUnknown:
	default:
		return fmt.Errorf("computer-use commit effect %q is invalid", outcome.Effect)
	}
	switch outcome.Recovery {
	case ComputerUseRecoveryNone:
	case ComputerUseRecoveryRetryWithApps:
		if outcome.Status != ComputerUseTaskNotCompleted ||
			outcome.Effect != ComputerUseCommitNone {
			return fmt.Errorf(
				"computer-use recovery %q requires not_completed with no committed effect",
				outcome.Recovery,
			)
		}
	case ComputerUseRecoveryAlternateControl:
		if outcome.Status == ComputerUseTaskCompleted ||
			outcome.Effect != ComputerUseCommitNone {
			return fmt.Errorf(
				"computer-use recovery %q requires an incomplete task with no committed effect",
				outcome.Recovery,
			)
		}
	default:
		return fmt.Errorf("computer-use recovery %q is invalid", outcome.Recovery)
	}
	return nil
}
