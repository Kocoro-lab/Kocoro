package daemon

import (
	"fmt"

	"github.com/Kocoro-lab/ShanClaw/internal/executionprofile"
)

type koeFollowUpAction string

const (
	koeFollowUpInject koeFollowUpAction = "inject"
	koeFollowUpChild  koeFollowUpAction = "child_run"
)

// decideKoeFollowUp is pure and runs before the generic injection path. A
// fast->full upgrade must wait on the existing route lock and create a fresh
// AgentLoop; mutating or injecting into the Terra loop is forbidden.
func decideKoeFollowUp(active executionprofile.Run, req RunAgentRequest) koeFollowUpAction {
	if !isKoeSource(req.Source) {
		return koeFollowUpInject
	}
	requested := executionprofile.NormalizeMode(string(req.ExecutionMode))
	if active.Profile.EffectiveMode == executionprofile.ModeFast && requested == executionprofile.ModeFull {
		return koeFollowUpChild
	}
	// Koe may mint the child generation before the daemon's resolver result
	// lands back at the voice ledger. Honor that stable parent/child lineage
	// even when the parent fast request failed closed to full.
	if active.RunID != "" &&
		req.ParentRunID == active.RunID &&
		req.ExecutionRunID != "" &&
		req.ExecutionRunID != active.RunID {
		return koeFollowUpChild
	}
	return koeFollowUpInject
}

func prepareActiveKoeChild(sc *SessionCache, routeKey string, req RunAgentRequest) (RunAgentRequest, bool) {
	if sc == nil || routeKey == "" {
		return req, false
	}
	active, ok := sc.ActiveRouteExecutionSnapshot(routeKey)
	if !ok {
		return req, false
	}
	req.AuthoritativeActiveParent = cloneExecutionRun(active.Run)
	if decideKoeFollowUp(active.Run, req) != koeFollowUpChild {
		return req, false
	}
	if req.ParentRunID == "" {
		req.ParentRunID = active.Run.RunID
	}
	req.WaitForRouteBoundary = true
	req.routeBoundaryGeneration = active.RunGeneration
	req.routeBoundaryCancelGeneration = active.CancelGeneration
	req.routeBoundaryDone = active.Done
	return req, true
}

// inheritParentExecutionEvidence seeds a new Koe child generation with the
// authoritative provider-neutral evidence from its parent. The child gets no
// provider continuation state and no computer activation; it only learns which
// validated side effects and deliverables already crossed a durable boundary.
func inheritParentExecutionEvidence(
	child *executionprofile.Run,
	ledger []executionprofile.Run,
) error {
	if child == nil || child.ParentRunID == "" {
		return nil
	}
	if child.ComputerActivation != nil ||
		len(child.Evidence.ToolOutcomes) != 0 ||
		len(child.Evidence.Deliverables) != 0 {
		return fmt.Errorf(
			"%w: new child run %q already contains execution state",
			executionprofile.ErrInvalidPersistedRun,
			child.RunID,
		)
	}

	var parent *executionprofile.Run
	for i := range ledger {
		if ledger[i].RunID != child.ParentRunID {
			continue
		}
		if parent != nil {
			return fmt.Errorf(
				"%w: duplicate parent run_id %q in execution ledger",
				executionprofile.ErrInvalidPersistedRun,
				child.ParentRunID,
			)
		}
		parent = &ledger[i]
	}
	if parent == nil {
		return fmt.Errorf(
			"%w: missing parent run_id %q in execution ledger",
			executionprofile.ErrInvalidPersistedRun,
			child.ParentRunID,
		)
	}
	if parent.LogicalTaskID != child.LogicalTaskID {
		return fmt.Errorf(
			"%w: child run %q logical task %q differs from parent %q",
			executionprofile.ErrInvalidPersistedRun,
			child.RunID,
			child.LogicalTaskID,
			parent.LogicalTaskID,
		)
	}
	if err := parent.ValidatePersisted(); err != nil {
		return fmt.Errorf("validate parent execution run: %w", err)
	}

	parentClone := cloneExecutionRun(*parent)
	child.Evidence = parentClone.Evidence
	if err := child.ValidatePersisted(); err != nil {
		return fmt.Errorf("validate child execution run after evidence inheritance: %w", err)
	}
	return nil
}
