package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/runstatus"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// WorkPlan: an optional, durable progress checklist inside one execution run.
// The set_work_plan tool records progress; it never executes work and never
// decides whether the outer run succeeded. One latest plan per session.
//
// Ownership split:
//   - model-writable: step content + status, optional explanation
//   - runtime-owned:  plan_id, run_id, revision, lifecycle, close_reason,
//     updated_at — never accepted as tool arguments
//
// Persistence ordering (see runPlanController): tool validates → controller
// updates in memory → result joins the transcript → the loop forces a durable
// checkpoint (ToolResult.CheckpointNow) → the checkpoint copies the snapshot
// into Session.WorkPlan and saves → only then is work_plan.updated emitted.
// SSE never observes a revision that could vanish in a daemon crash.

const (
	workPlanMinSteps = 2
	workPlanMaxSteps = 8
)

func newWorkPlanID() (string, error) { return mintRunEventID("wp1_") }

// runPlanController owns one run's WorkPlan state. It is created per RunAgent
// call and dies with it; the durable home is Session.WorkPlan, written only
// through StageForSave on the runner's persistence paths.
//
// The mutex covers tool-goroutine writes (the dispatcher may run tools off the
// loop goroutine) racing checkpoint-goroutine reads.
type runPlanController struct {
	mu        sync.Mutex
	sessionID string
	runID     string
	snap      *session.WorkPlanSnapshot
	// lastNormHash fingerprints the normalized step list (content+status only;
	// explanation deliberately excluded so re-sending identical steps with new
	// prose cannot mint revisions — that is the ritual-update channel loop
	// detection watches). Empty until the first applied snapshot.
	lastNormHash string
	// pendingEvent is the latest snapshot awaiting emission. It is staged on
	// every applied change and taken only after a successful durable save.
	// Multiple changes between saves coalesce to the newest snapshot, which is
	// correct under the full-snapshot + revision recovery contract.
	pendingEvent *session.WorkPlanSnapshot
}

func newRunPlanController(sessionID, runID string) *runPlanController {
	return &runPlanController{sessionID: sessionID, runID: runID}
}

// Restore adopts a persisted active plan for the SAME RunID (interrupted-run
// recovery: AttemptID changes, RunID does not). Closed plans are UI history
// and are never re-adopted.
func (c *runPlanController) Restore(snap *session.WorkPlanSnapshot) {
	if snap == nil || snap.Lifecycle != session.WorkPlanActive || snap.RunID != c.runID {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snap = snap.Clone()
	c.lastNormHash = normalizedWorkPlanHash(c.snap.Steps)
}

// ActiveSnapshot returns a copy of the current plan when it is active, nil
// otherwise. Used to inject the plan into a resumed run's volatile context.
func (c *runPlanController) ActiveSnapshot() *session.WorkPlanSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snap == nil || c.snap.Lifecycle != session.WorkPlanActive {
		return nil
	}
	return c.snap.Clone()
}

type workPlanApplyResult struct {
	planID    string
	revision  uint64
	completed int
	total     int
	changed   bool
}

// Apply installs a full-snapshot update. steps must already be validated and
// normalized (trimmed content). Identical normalized steps are a no-op:
// revision does not advance, no event is staged, no checkpoint is requested.
func (c *runPlanController) Apply(explanation string, steps []session.WorkPlanStep, now time.Time) (workPlanApplyResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	hash := normalizedWorkPlanHash(steps)
	if c.snap != nil && c.snap.Lifecycle == session.WorkPlanActive && hash == c.lastNormHash {
		return workPlanApplyResult{
			planID:    c.snap.PlanID,
			revision:  c.snap.Revision,
			completed: c.snap.CompletedStepCount(),
			total:     len(c.snap.Steps),
			changed:   false,
		}, nil
	}
	if c.snap == nil {
		planID, err := newWorkPlanID()
		if err != nil {
			return workPlanApplyResult{}, fmt.Errorf("mint work plan id: %w", err)
		}
		c.snap = &session.WorkPlanSnapshot{
			PlanID:    planID,
			RunID:     c.runID,
			Revision:  0,
			Lifecycle: session.WorkPlanActive,
		}
	}
	c.snap.Revision++
	c.snap.Lifecycle = session.WorkPlanActive
	c.snap.CloseReason = ""
	c.snap.Explanation = explanation
	c.snap.Steps = append([]session.WorkPlanStep(nil), steps...)
	c.snap.UpdatedAt = now
	c.lastNormHash = hash
	c.pendingEvent = c.snap.Clone()
	return workPlanApplyResult{
		planID:    c.snap.PlanID,
		revision:  c.snap.Revision,
		completed: c.snap.CompletedStepCount(),
		total:     len(c.snap.Steps),
		changed:   true,
	}, nil
}

// CloseForRun applies the runtime closure rules at the end of the owning run.
// The outer run result is authoritative and is never modified here; stale
// pending steps never downgrade a clean run.
func (c *runPlanController) CloseForRun(status agent.RunStatus, runErr error, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snap == nil || c.snap.Lifecycle != session.WorkPlanActive {
		return false
	}
	// Closure is a revision like any other: consumers drop lower-or-equal
	// revisions, so a terminal lifecycle transition riding the same revision
	// number as the last step update would be discarded as stale.
	c.snap.Revision++
	switch {
	case status.FailureCode == runstatus.CodeUserCancelled:
		c.snap.Lifecycle = session.WorkPlanStopped
		c.snap.CloseReason = session.WorkPlanCloseCancelled
	case runErr != nil:
		c.snap.Lifecycle = session.WorkPlanStopped
		c.snap.CloseReason = session.WorkPlanCloseFailed
	case status.Partial:
		c.snap.Lifecycle = session.WorkPlanStopped
		c.snap.CloseReason = session.WorkPlanClosePartial
	case c.snap.CompletedStepCount() == len(c.snap.Steps):
		c.snap.Lifecycle = session.WorkPlanCompleted
		c.snap.CloseReason = session.WorkPlanCloseRunCompleted
	default:
		c.snap.Lifecycle = session.WorkPlanStopped
		c.snap.CloseReason = session.WorkPlanCloseRunCompletedWithPendingSteps
	}
	c.snap.UpdatedAt = now
	c.pendingEvent = c.snap.Clone()
	return true
}

// StageForSave copies the controller snapshot into the session about to be
// persisted. A run that never created a plan leaves an earlier run's closed
// plan in place; a run that did replaces it — that overwrite IS the
// "superseded" transition for a previous still-active plan (crash leftovers),
// since only the latest plan per session is retained.
func (c *runPlanController) StageForSave(sess *session.Session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snap == nil {
		return
	}
	sess.WorkPlan = c.snap.Clone()
}

// TakePendingEvent returns the snapshot awaiting emission and clears it.
// Callers MUST invoke this only after the durable save that covered the
// snapshot succeeded.
func (c *runPlanController) TakePendingEvent() *session.WorkPlanSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	snap := c.pendingEvent
	c.pendingEvent = nil
	return snap
}

func normalizedWorkPlanHash(steps []session.WorkPlanStep) string {
	h := sha256.New()
	for _, s := range steps {
		h.Write([]byte(s.Content))
		h.Write([]byte{0})
		h.Write([]byte(s.Status))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// workPlanEventPayload is the work_plan.updated wire payload, pinned by
// docs/desktop-wire-fixtures/bus_event.work_plan.updated.json. Full snapshot +
// revision is the recovery contract: the event is coalescible under
// backpressure and GET /sessions/{id} remains recovery authority.
type workPlanEventPayload struct {
	SessionID   string                    `json:"session_id"`
	RunID       string                    `json:"run_id"`
	PlanID      string                    `json:"plan_id"`
	Revision    uint64                    `json:"revision"`
	Lifecycle   session.WorkPlanLifecycle `json:"lifecycle"`
	CloseReason string                    `json:"close_reason,omitempty"`
	Explanation string                    `json:"explanation,omitempty"`
	Steps       []session.WorkPlanStep    `json:"steps"`
	Completed   int                       `json:"completed"`
	Total       int                       `json:"total"`
	UpdatedAt   time.Time                 `json:"updated_at"`
	Ts          time.Time                 `json:"ts"`
}

// emitWorkPlanUpdated publishes one snapshot on the broadcast bus. The single
// producer for both live emission and the wire-fixture test.
func emitWorkPlanUpdated(bus *EventBus, sessionID string, snap *session.WorkPlanSnapshot, ts time.Time) {
	if bus == nil || snap == nil {
		return
	}
	payload, err := json.Marshal(workPlanEventPayload{
		SessionID:   sessionID,
		RunID:       snap.RunID,
		PlanID:      snap.PlanID,
		Revision:    snap.Revision,
		Lifecycle:   snap.Lifecycle,
		CloseReason: snap.CloseReason,
		Explanation: snap.Explanation,
		Steps:       snap.Steps,
		Completed:   snap.CompletedStepCount(),
		Total:       len(snap.Steps),
		UpdatedAt:   snap.UpdatedAt,
		Ts:          ts,
	})
	if err != nil {
		return
	}
	bus.Emit(Event{Type: EventWorkPlanUpdated, Payload: payload})
}

// renderWorkPlanForPrompt renders an active plan as prompt-neutral text for
// the resumed run's VolatileContext (after <!-- cache_break -->; never System
// or StableContext). A rendered string keeps the prompt package free of
// session storage imports.
func renderWorkPlanForPrompt(snap *session.WorkPlanSnapshot) string {
	if snap == nil || snap.Lifecycle != session.WorkPlanActive {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Active work plan (revision %d, %d/%d steps completed) from the interrupted run you are continuing. Submit the full updated list via set_work_plan when a step completes or scope changes:\n",
		snap.Revision, snap.CompletedStepCount(), len(snap.Steps))
	for _, s := range snap.Steps {
		marker := "[ ]"
		switch s.Status {
		case session.WorkPlanStepCompleted:
			marker = "[x]"
		case session.WorkPlanStepInProgress:
			marker = "[>]"
		}
		fmt.Fprintf(&b, "%s %s\n", marker, s.Content)
	}
	return strings.TrimRight(b.String(), "\n")
}

// setWorkPlanTool is the daemon-owned, run-scoped Direct tool. Local tools
// default to Direct exposure (no ToolExposureProvider implementation), which
// keeps the schema present before any other work starts.
type setWorkPlanTool struct {
	controller *runPlanController
}

type setWorkPlanArgs struct {
	Explanation string `json:"explanation"`
	Steps       []struct {
		Content string `json:"content"`
		Status  string `json:"status"`
	} `json:"steps"`
}

func (t *setWorkPlanTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        "set_work_plan",
		Description: "Maintain a concise execution progress plan for work Kocoro is actively carrying out. Use it only when the current request requires multiple meaningful execution stages, dependencies, several deliverables, a long-running tool sequence, or the user explicitly asks to track progress. Do not use it for questions, explanations, small talk, translation, rewriting, one lookup, one calculation, one direct action, one schedule operation, or advice that only asks you to propose a plan. Submit the complete current step list each time. Update it only when a step or the task scope materially changes. This tool records progress; it does not perform work or prove completion.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"explanation": map[string]any{
					"type":        "string",
					"description": "Optional concise reason for a material change to the plan.",
				},
				"steps": map[string]any{
					"type":     "array",
					"minItems": workPlanMinSteps,
					"maxItems": workPlanMaxSteps,
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"content": map[string]any{"type": "string"},
							"status": map[string]any{
								"type": "string",
								"enum": []string{
									string(session.WorkPlanStepPending),
									string(session.WorkPlanStepInProgress),
									string(session.WorkPlanStepCompleted),
								},
							},
						},
						"required":             []string{"content", "status"},
						"additionalProperties": false,
					},
				},
			},
			"required":             []string{"steps"},
			"additionalProperties": false,
		},
		Required: []string{"steps"},
	}
}

func (*setWorkPlanTool) RequiresApproval() bool { return false }

// Not read-only: it mutates durable session metadata.
func (*setWorkPlanTool) IsReadOnlyCall(string) bool { return false }

// Serialized with the tool order: revisions must observe transcript order.
func (*setWorkPlanTool) IsConcurrencySafeCall(string) bool { return false }

// No material side effect: it never mutates external or user-visible state
// outside the session record, so it MUST NOT enter the side-effect journal
// (that journal protects external actions and has different replay semantics).
func (*setWorkPlanTool) HasMaterialSideEffect(string) bool { return false }

// SkillExempt: pure core harness infrastructure with zero external I/O — the
// same class as think/tool_search/use_skill. A skill's allowed-tools filter
// must not silently break progress tracking mid-skill; execution-time denial
// of an internal metadata write would punish exactly the long multi-stage
// runs the plan exists for. Covered by an execution-filter test.
func (*setWorkPlanTool) SkillExempt() bool { return true }

func (t *setWorkPlanTool) Run(ctx context.Context, args string) (agent.ToolResult, error) {
	var in setWorkPlanArgs
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return agent.ValidationError("invalid JSON arguments: " + err.Error()), nil
	}
	if len(in.Steps) == 0 {
		return agent.ValidationError("steps is required"), nil
	}
	if len(in.Steps) < workPlanMinSteps {
		return agent.ValidationError(fmt.Sprintf("a work plan needs at least %d steps; do not use set_work_plan for single-step work", workPlanMinSteps)), nil
	}
	if len(in.Steps) > workPlanMaxSteps {
		return agent.ValidationError(fmt.Sprintf("at most %d steps; merge related stages", workPlanMaxSteps)), nil
	}
	steps := make([]session.WorkPlanStep, 0, len(in.Steps))
	seen := make(map[string]bool, len(in.Steps))
	inProgress := 0
	for i, s := range in.Steps {
		content := strings.TrimSpace(s.Content)
		if content == "" {
			return agent.ValidationError(fmt.Sprintf("steps[%d].content is empty", i)), nil
		}
		norm := strings.ToLower(strings.Join(strings.Fields(content), " "))
		if seen[norm] {
			return agent.ValidationError(fmt.Sprintf("steps[%d] duplicates another step: %q", i, content)), nil
		}
		seen[norm] = true
		status := session.WorkPlanStepStatus(s.Status)
		switch status {
		case session.WorkPlanStepPending, session.WorkPlanStepInProgress, session.WorkPlanStepCompleted:
		default:
			return agent.ValidationError(fmt.Sprintf("steps[%d].status %q is not one of pending, in_progress, completed", i, s.Status)), nil
		}
		if status == session.WorkPlanStepInProgress {
			inProgress++
		}
		steps = append(steps, session.WorkPlanStep{Content: content, Status: status})
	}
	// Zero in_progress is legal before work starts and after every step
	// completes; more than one is not a snapshot of sequential execution.
	if inProgress > 1 {
		return agent.ValidationError("at most one step may be in_progress"), nil
	}
	if t.controller == nil {
		return agent.ToolResult{}, errors.New("set_work_plan: no run plan controller bound")
	}
	res, err := t.controller.Apply(strings.TrimSpace(in.Explanation), steps, time.Now())
	if err != nil {
		return agent.ToolResult{}, err
	}
	body, err := json.Marshal(map[string]any{
		"plan_id":   res.planID,
		"revision":  res.revision,
		"completed": res.completed,
		"total":     res.total,
		"changed":   res.changed,
	})
	if err != nil {
		return agent.ToolResult{}, err
	}
	return agent.ToolResult{
		Content: string(body),
		// A changed snapshot must be durable before the next provider call;
		// an identical no-op must not force an extra persistence write.
		CheckpointNow: res.changed,
	}, nil
}
