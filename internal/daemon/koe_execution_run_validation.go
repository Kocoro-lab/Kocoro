package daemon

import (
	"fmt"

	"github.com/Kocoro-lab/ShanClaw/internal/executionprofile"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// authorizeKoeExecutionLineage reconciles an about-to-start AgentLoop with the
// authoritative session ledger. Wire lineage fields are only claims:
//
//   - a new child must reference one unique, valid persisted parent;
//   - a request that reuses an already-persisted run ID is a follow-up that
//     missed the active injection window, so it receives a fresh direct child;
//   - only a validated Full parent can establish the provider-neutral Full
//     lineage floor.
//
// The caller holds the route lock, so the supplied ledger cannot change while
// this decision is made.
func authorizeKoeExecutionLineage(
	req *RunAgentRequest,
	ledger []executionprofile.Run,
) error {
	if req == nil || !isKoeSource(req.Source) || req.ResumeInterrupted {
		return nil
	}

	run := &req.ExecutionRun
	req.LogicalTaskID = run.LogicalTaskID
	req.ExecutionRunID = run.RunID
	req.ParentRunID = run.ParentRunID
	req.InheritedMode = ""

	existing, found, err := uniqueValidatedKoeRun(ledger, run.RunID)
	if err != nil {
		return fmt.Errorf("validate reused Koe execution run: %w", err)
	}
	if found {
		if existing.LogicalTaskID != run.LogicalTaskID {
			return fmt.Errorf(
				"%w: reused run_id %q logical task %q differs from persisted task %q",
				executionprofile.ErrInvalidPersistedRun,
				run.RunID,
				run.LogicalTaskID,
				existing.LogicalTaskID,
			)
		}
		if existing.ParentRunID != run.ParentRunID {
			return fmt.Errorf(
				"%w: reused run_id %q parent %q differs from persisted parent %q",
				executionprofile.ErrInvalidPersistedRun,
				run.RunID,
				run.ParentRunID,
				existing.ParentRunID,
			)
		}

		// A persisted run already crossed a terminal/checkpoint boundary. A new
		// AgentLoop must never write new evidence under that immutable identity.
		run.RunID = newExecutionRunID()
		run.ParentRunID = existing.RunID
		req.ExecutionRunID = run.RunID
		req.ParentRunID = run.ParentRunID
		applyValidatedKoeLineageFloor(req, existing)
		return nil
	}

	active := cloneExecutionRun(req.AuthoritativeActiveParent)
	if active.RunID != "" &&
		(run.RunID == active.RunID || run.ParentRunID == active.RunID) {
		if err := active.ValidatePersisted(); err != nil {
			return fmt.Errorf("validate active Koe parent execution run: %w", err)
		}
		if active.LogicalTaskID != run.LogicalTaskID {
			return fmt.Errorf(
				"%w: active parent run %q logical task %q differs from request task %q",
				executionprofile.ErrInvalidPersistedRun,
				active.RunID,
				active.LogicalTaskID,
				run.LogicalTaskID,
			)
		}
		if run.RunID == active.RunID {
			run.RunID = newExecutionRunID()
			run.ParentRunID = active.RunID
			req.ExecutionRunID = run.RunID
			req.ParentRunID = run.ParentRunID
		}
		applyValidatedKoeLineageFloor(req, active)
	}

	if run.ParentRunID == "" {
		return nil
	}
	parent, found, err := uniqueValidatedKoeRun(ledger, run.ParentRunID)
	if err != nil {
		return fmt.Errorf("validate Koe parent execution run: %w", err)
	}
	if !found {
		return fmt.Errorf(
			"%w: execution ledger is missing parent run_id %q",
			executionprofile.ErrInvalidPersistedRun,
			run.ParentRunID,
		)
	}
	if parent.LogicalTaskID != run.LogicalTaskID {
		return fmt.Errorf(
			"%w: child run %q logical task %q differs from parent %q",
			executionprofile.ErrInvalidPersistedRun,
			run.RunID,
			run.LogicalTaskID,
			parent.LogicalTaskID,
		)
	}
	applyValidatedKoeLineageFloor(req, parent)
	return nil
}

func uniqueValidatedKoeRun(
	ledger []executionprofile.Run,
	runID string,
) (executionprofile.Run, bool, error) {
	if runID == "" {
		return executionprofile.Run{}, false, nil
	}
	var matched *executionprofile.Run
	for i := range ledger {
		if ledger[i].RunID != runID {
			continue
		}
		if matched != nil {
			return executionprofile.Run{}, false, fmt.Errorf(
				"%w: execution ledger contains duplicate run_id %q",
				executionprofile.ErrInvalidPersistedRun,
				runID,
			)
		}
		candidate := cloneExecutionRun(ledger[i])
		matched = &candidate
	}
	if matched == nil {
		return executionprofile.Run{}, false, nil
	}
	if err := matched.ValidatePersisted(); err != nil {
		return executionprofile.Run{}, false, err
	}
	return *matched, true, nil
}

func applyValidatedKoeLineageFloor(
	req *RunAgentRequest,
	parent executionprofile.Run,
) {
	if req == nil || parent.Profile.EffectiveMode != executionprofile.ModeFull {
		return
	}
	req.InheritedMode = executionprofile.ModeFull
	req.ExecutionRun.Profile = executionprofile.FullProfile(
		req.ExecutionRun.Profile.RequestedMode,
		"lineage_full_preserved",
	)
}

// validatePersistedKoeResumeRequest is the common recovery gate. The resolver
// bypasses every ResumeInterrupted request, and claimInterruptedResume invokes
// this against the authoritative session snapshot before constructing the loop.
func validatePersistedKoeResumeRequest(req RunAgentRequest) error {
	if !req.ResumeInterrupted || !isKoeSource(req.Source) {
		return nil
	}
	if err := req.ExecutionRun.ValidatePersisted(); err != nil {
		return fmt.Errorf("restore checkpointed Koe execution run: %w", err)
	}
	return nil
}

func abandonInvalidKoeResume(
	sessMgr *session.Manager,
	sess *session.Session,
	validationErr error,
) error {
	sess.InProgress = false
	sess.InterruptedTurn = nil
	if err := sessMgr.SavePreservingUpdatedAt(); err != nil {
		return fmt.Errorf(
			"persist invalid checkpointed Koe execution run abandonment: %v: %w",
			err,
			validationErr,
		)
	}
	return validationErr
}

// validatePersistedKoeRunAgainstLedger validates the authoritative interrupted
// snapshot and verifies that its immutable identity/profile fields match the
// exactly-one ledger entry with the same RunID. Computer activation and
// evidence remain mutable checkpoint state and are intentionally not compared.
func validatePersistedKoeRunAgainstLedger(
	run executionprofile.Run,
	ledger []executionprofile.Run,
) error {
	if err := run.ValidatePersisted(); err != nil {
		return err
	}

	var matched *executionprofile.Run
	for i := range ledger {
		if ledger[i].RunID != run.RunID {
			continue
		}
		if matched != nil {
			return fmt.Errorf(
				"%w: execution ledger contains duplicate run_id %q",
				executionprofile.ErrInvalidPersistedRun,
				run.RunID,
			)
		}
		matched = &ledger[i]
	}
	if matched == nil {
		return fmt.Errorf(
			"%w: execution ledger is missing run_id %q",
			executionprofile.ErrInvalidPersistedRun,
			run.RunID,
		)
	}
	if !sameExecutionRunImmutableFields(run, *matched) {
		return fmt.Errorf(
			"%w: execution ledger immutable fields changed for run_id %q",
			executionprofile.ErrInvalidPersistedRun,
			run.RunID,
		)
	}
	return nil
}

func sameExecutionRunImmutableFields(a, b executionprofile.Run) bool {
	return a.RunID == b.RunID &&
		a.LogicalTaskID == b.LogicalTaskID &&
		a.ParentRunID == b.ParentRunID &&
		a.Profile == b.Profile
}
