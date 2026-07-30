package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

type openAIComputerSafetyAcknowledgementSeal struct{}

var trustedOpenAIComputerSafetyAcknowledgementSeal = &openAIComputerSafetyAcknowledgementSeal{}

type openAIComputerSafetyAcknowledgementState uint8

const (
	openAIComputerSafetyAcknowledgementMinted openAIComputerSafetyAcknowledgementState = iota
	openAIComputerSafetyAcknowledgementExecutionConsumed
	openAIComputerSafetyAcknowledgementContinuationConsumed
)

// OpenAIComputerSafetyAcknowledgement is a process-local, one-shot capability
// binding attended confirmation (or the explicit absence of pending checks) to
// one trusted profile, provider response, and exact normalized computer_call.
// It is never serialized and cannot be constructed outside this package.
type OpenAIComputerSafetyAcknowledgement struct {
	mu sync.Mutex

	seal       *openAIComputerSafetyAcknowledgementSeal
	profile    *client.ExecutionProfile
	responseID string
	callDigest [sha256.Size]byte
	checks     []client.OpenAIComputerSafetyCheck
	state      openAIComputerSafetyAcknowledgementState
}

func (trajectory *openAIComputerTrajectory) newSafetyAcknowledgement(
	userConfirmed bool,
) (*OpenAIComputerSafetyAcknowledgement, error) {
	if trajectory == nil || trajectory.profile == nil {
		return nil, fmt.Errorf("OpenAI computer safety trajectory is unavailable")
	}
	if err := validateTrustedOpenAIComputerProfile(trajectory.profile); err != nil {
		return nil, err
	}
	if len(trajectory.call.PendingSafetyChecks) > 0 && !userConfirmed {
		return nil, fmt.Errorf(
			"OpenAI computer pending safety checks require attended confirmation",
		)
	}
	digest, err := digestOpenAIComputerSafetyCall(trajectory.call)
	if err != nil {
		return nil, err
	}
	return &OpenAIComputerSafetyAcknowledgement{
		seal:       trustedOpenAIComputerSafetyAcknowledgementSeal,
		profile:    trajectory.profile,
		responseID: trajectory.responseID,
		callDigest: digest,
		checks: client.CloneOpenAIComputerSafetyChecks(
			trajectory.call.PendingSafetyChecks,
		),
		state: openAIComputerSafetyAcknowledgementMinted,
	}, nil
}

func (trajectory *openAIComputerTrajectory) safetyConfirmationArguments() (
	string,
	bool,
	error,
) {
	if trajectory == nil || trajectory.profile == nil {
		return "", false, fmt.Errorf("OpenAI computer safety trajectory is unavailable")
	}
	if len(trajectory.call.PendingSafetyChecks) == 0 {
		return "", false, nil
	}
	payload, err := json.Marshal(struct {
		Action              string                             `json:"action"`
		Description         string                             `json:"description"`
		Provider            string                             `json:"provider"`
		ResponseID          string                             `json:"response_id"`
		CallID              string                             `json:"call_id"`
		PendingSafetyChecks []client.OpenAIComputerSafetyCheck `json:"pending_safety_checks"`
	}{
		Action:      "acknowledge_provider_safety_checks",
		Description: "Confirm the provider safety checks before any computer action is executed",
		Provider:    client.OpenAIComputerProvider,
		ResponseID:  trajectory.responseID,
		CallID:      trajectory.call.CallID,
		PendingSafetyChecks: client.CloneOpenAIComputerSafetyChecks(
			trajectory.call.PendingSafetyChecks,
		),
	})
	if err != nil {
		return "", false, fmt.Errorf(
			"encode OpenAI computer safety confirmation: %w",
			err,
		)
	}
	return string(payload), true, nil
}

// ConsumeForExecution atomically admits the exact batch once. The daemon must
// call it before acquiring GUI authority or executing any action.
func (acknowledgement *OpenAIComputerSafetyAcknowledgement) ConsumeForExecution(
	profile *client.ExecutionProfile,
	responseID string,
	call client.OpenAIComputerCall,
) bool {
	if acknowledgement == nil {
		return false
	}
	digest, err := digestOpenAIComputerSafetyCall(call)
	if err != nil {
		return false
	}
	acknowledgement.mu.Lock()
	defer acknowledgement.mu.Unlock()
	if acknowledgement.state != openAIComputerSafetyAcknowledgementMinted ||
		!acknowledgement.matchesLocked(profile, responseID, digest) {
		return false
	}
	acknowledgement.state = openAIComputerSafetyAcknowledgementExecutionConsumed
	return true
}

func (acknowledgement *OpenAIComputerSafetyAcknowledgement) takeForContinuation(
	profile *client.ExecutionProfile,
	responseID string,
	call client.OpenAIComputerCall,
) ([]client.OpenAIComputerSafetyCheck, error) {
	if acknowledgement == nil {
		return nil, fmt.Errorf("OpenAI computer safety acknowledgement is unavailable")
	}
	digest, err := digestOpenAIComputerSafetyCall(call)
	if err != nil {
		return nil, err
	}
	acknowledgement.mu.Lock()
	defer acknowledgement.mu.Unlock()
	if acknowledgement.state != openAIComputerSafetyAcknowledgementExecutionConsumed ||
		!acknowledgement.matchesLocked(profile, responseID, digest) {
		return nil, fmt.Errorf(
			"OpenAI computer safety acknowledgement is mismatched or already consumed",
		)
	}
	acknowledgement.state = openAIComputerSafetyAcknowledgementContinuationConsumed
	return client.CloneOpenAIComputerSafetyChecks(acknowledgement.checks), nil
}

func (acknowledgement *OpenAIComputerSafetyAcknowledgement) matchesLocked(
	profile *client.ExecutionProfile,
	responseID string,
	callDigest [sha256.Size]byte,
) bool {
	return acknowledgement.seal == trustedOpenAIComputerSafetyAcknowledgementSeal &&
		acknowledgement.profile != nil &&
		profile != nil &&
		profile.IsTrustedResolution() &&
		acknowledgement.profile.MatchesExact(profile) &&
		acknowledgement.responseID == responseID &&
		acknowledgement.callDigest == callDigest
}

func digestOpenAIComputerSafetyCall(
	call client.OpenAIComputerCall,
) ([sha256.Size]byte, error) {
	payload, err := json.Marshal(call)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf(
			"bind OpenAI computer safety acknowledgement: %w",
			err,
		)
	}
	return sha256.Sum256(payload), nil
}
