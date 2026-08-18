package session

import "time"

// WorkPlanStepStatus is the model-reported execution state of one plan step.
type WorkPlanStepStatus string

const (
	WorkPlanStepPending    WorkPlanStepStatus = "pending"
	WorkPlanStepInProgress WorkPlanStepStatus = "in_progress"
	WorkPlanStepCompleted  WorkPlanStepStatus = "completed"
)

// WorkPlanLifecycle is the runtime-owned lifecycle of a WorkPlan. The model
// never sets it: active plans are closed by the daemon at run completion using
// LastRunStatus evidence, never by a model claim.
type WorkPlanLifecycle string

const (
	WorkPlanActive    WorkPlanLifecycle = "active"
	WorkPlanCompleted WorkPlanLifecycle = "completed"
	WorkPlanStopped   WorkPlanLifecycle = "stopped"
)

// WorkPlan close reasons. Runtime-owned; recorded when a plan leaves the
// active lifecycle.
const (
	// WorkPlanCloseRunCompleted: clean run, every step completed.
	WorkPlanCloseRunCompleted = "run_completed"
	// WorkPlanCloseRunCompletedWithPendingSteps: the run finished clean but the
	// model left pending/in-progress steps. The outer run result is NOT
	// downgraded — stale steps are model metadata, not failure evidence.
	WorkPlanCloseRunCompletedWithPendingSteps = "run_completed_with_pending_steps"
	// WorkPlanClosePartial: the run ended with a partial result
	// (iteration limit, idle timeout force-stop, ...).
	WorkPlanClosePartial = "partial"
	// WorkPlanCloseFailed: the run ended with a hard error.
	WorkPlanCloseFailed = "failed"
	// WorkPlanCloseCancelled: the user cancelled the run.
	WorkPlanCloseCancelled = "cancelled"
	// WorkPlanCloseSuperseded: a later run installed a new plan while this one
	// was still active.
	WorkPlanCloseSuperseded = "superseded"
)

// WorkPlanStep is one checklist entry. Content and Status are the only
// model-writable fields in the whole WorkPlan domain.
type WorkPlanStep struct {
	Content string             `json:"content"`
	Status  WorkPlanStepStatus `json:"status"`
}

// WorkPlanSnapshot is the session's latest work plan — an optional, durable
// progress checklist recorded by the set_work_plan tool inside one execution
// run. It records progress; it never executes work and never decides whether
// the outer run succeeded. One latest plan per session: a later run's plan
// replaces this snapshot in place.
//
// PlanID, RunID, Revision, Lifecycle, CloseReason, and UpdatedAt are
// runtime-owned and are never accepted as model arguments. Ownership binds to
// the stable RunID (not AttemptID) so interrupted-recovery attempts continue
// the same plan.
type WorkPlanSnapshot struct {
	PlanID      string            `json:"plan_id"`
	RunID       string            `json:"run_id"`
	Revision    uint64            `json:"revision"`
	Lifecycle   WorkPlanLifecycle `json:"lifecycle"`
	CloseReason string            `json:"close_reason,omitempty"`
	Explanation string            `json:"explanation,omitempty"`
	Steps       []WorkPlanStep    `json:"steps"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// Clone returns a deep copy so callers can hand snapshots across goroutine or
// persistence boundaries without sharing the Steps backing array.
func (w *WorkPlanSnapshot) Clone() *WorkPlanSnapshot {
	if w == nil {
		return nil
	}
	cp := *w
	cp.Steps = append([]WorkPlanStep(nil), w.Steps...)
	return &cp
}

// CompletedStepCount returns how many steps are completed.
func (w *WorkPlanSnapshot) CompletedStepCount() int {
	if w == nil {
		return 0
	}
	n := 0
	for _, s := range w.Steps {
		if s.Status == WorkPlanStepCompleted {
			n++
		}
	}
	return n
}
