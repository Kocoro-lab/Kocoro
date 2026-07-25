package guicontrol

import (
	"fmt"
	"time"
)

// BusyError contains only redacted ownership metadata suitable for returning
// to another model workflow. Session identity intentionally remains blank.
type BusyError struct {
	ActiveLeaseID   string
	ActiveSessionID string
	SourceKind      string
	SourceLabel     string
	TargetBundleID  string
	TargetAppName   string
	RetryAfter      time.Duration
}

func (e *BusyError) Error() string {
	return fmt.Sprintf("computer_use_busy: GUI is controlled by %s for %s; retry after %s",
		nonempty(e.SourceLabel, e.SourceKind, "another workflow"),
		nonempty(e.TargetAppName, e.TargetBundleID, "an app"), e.RetryAfter)
}

type StaleLeaseError struct {
	LeaseID string
}

func (e *StaleLeaseError) Error() string {
	return fmt.Sprintf("stale computer-use lease %q", e.LeaseID)
}

type StaleActionError struct {
	LeaseID  string
	ActionID string
}

func (e *StaleActionError) Error() string {
	return fmt.Sprintf("stale computer-use action %q for lease %q", e.ActionID, e.LeaseID)
}

type InvalidTransitionError struct {
	Action ComputerUseControlAction
	State  ComputerUseLeaseState
}

func (e *InvalidTransitionError) Error() string {
	return fmt.Sprintf("computer-use control %q is invalid while lease is %q", e.Action, e.State)
}

type IdempotencyConflictError struct {
	Key string
}

func (e *IdempotencyConflictError) Error() string {
	return fmt.Sprintf("computer-use idempotency key %q was already used for another request", e.Key)
}

type StoppedTurnError struct {
	TurnID string
}

func (e *StoppedTurnError) Error() string {
	return fmt.Sprintf("computer-use was stopped for turn %q", e.TurnID)
}

type LeaseExpiredError struct {
	LeaseID string
	TurnID  string
}

func (e *LeaseExpiredError) Error() string {
	return fmt.Sprintf("computer-use lease %q expired for turn %q", e.LeaseID, e.TurnID)
}

type ActionInProgressError struct {
	LeaseID  string
	ActionID string
}

type ControllerUnavailableError struct {
	LeaseID string
}

// ReobservationRequiredError prevents a resumed controller from applying a
// mutation to UI state that the user may have changed while Kocoro was paused.
// Only a newly completed, verified observation clears this barrier.
type ReobservationRequiredError struct {
	LeaseID string
}

// PolicyDeniedError is returned before an action handle exists, so callers
// cannot accidentally treat a target-policy failure as a committed mutation.
type PolicyDeniedError struct {
	LeaseID          string
	TargetBundleID   string
	PolicySnapshotID string
	Reason           string
}

func (e *PolicyDeniedError) Error() string {
	return fmt.Sprintf("computer-use policy denied target %q: %s", e.TargetBundleID, nonempty(e.Reason, "target is not allowed"))
}

func (e *ControllerUnavailableError) Error() string {
	return fmt.Sprintf("computer-use controller has not acknowledged lease %q", e.LeaseID)
}

func (e *ReobservationRequiredError) Error() string {
	return fmt.Sprintf("computer-use lease %q requires a verified observation before mutation", e.LeaseID)
}

func (e *ActionInProgressError) Error() string {
	return fmt.Sprintf("computer-use action %q is already active for lease %q", e.ActionID, e.LeaseID)
}

func nonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return "unknown"
}
