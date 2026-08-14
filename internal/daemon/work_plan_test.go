package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/runstatus"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

const testWorkPlanRunID = "run1_0123456789abcdef0123456789abcdef"

func newTestWorkPlanTool() (*setWorkPlanTool, *runPlanController) {
	c := newRunPlanController("sess-1", testWorkPlanRunID)
	return &setWorkPlanTool{controller: c}, c
}

func runWorkPlanTool(t *testing.T, tool *setWorkPlanTool, args string) agent.ToolResult {
	t.Helper()
	res, err := tool.Run(context.Background(), args)
	if err != nil {
		t.Fatalf("Run returned hard error: %v", err)
	}
	return res
}

func validStepsJSON(statuses ...string) string {
	type step struct {
		Content string `json:"content"`
		Status  string `json:"status"`
	}
	steps := make([]step, 0, len(statuses))
	names := []string{"Inspect current behavior", "Implement the change", "Exercise the real call path", "Verify the outcome", "Write the report", "Review edge cases", "Polish naming", "Final sweep"}
	for i, s := range statuses {
		steps = append(steps, step{Content: names[i], Status: s})
	}
	b, _ := json.Marshal(map[string]any{"steps": steps})
	return string(b)
}

func TestSetWorkPlanTool_ValidationErrors(t *testing.T) {
	tool, _ := newTestWorkPlanTool()
	cases := []struct {
		name string
		args string
		want string
	}{
		{"invalid json", `{`, "invalid JSON"},
		{"missing steps", `{}`, "steps is required"},
		{"empty steps", `{"steps":[]}`, "steps is required"},
		{"one step", validStepsJSON("pending"), "at least 2"},
		{"nine steps", `{"steps":[{"content":"a1","status":"pending"},{"content":"a2","status":"pending"},{"content":"a3","status":"pending"},{"content":"a4","status":"pending"},{"content":"a5","status":"pending"},{"content":"a6","status":"pending"},{"content":"a7","status":"pending"},{"content":"a8","status":"pending"},{"content":"a9","status":"pending"}]}`, "at most 8"},
		{"empty content", `{"steps":[{"content":"  ","status":"pending"},{"content":"b","status":"pending"}]}`, "content is empty"},
		{"duplicate content", `{"steps":[{"content":"Same  Step","status":"pending"},{"content":"same step","status":"pending"}]}`, "duplicates"},
		{"unknown status", `{"steps":[{"content":"a","status":"doing"},{"content":"b","status":"pending"}]}`, "not one of"},
		{"two in_progress", validStepsJSON("in_progress", "in_progress"), "at most one step may be in_progress"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runWorkPlanTool(t, tool, tc.args)
			if !res.IsError || res.ErrorCategory != agent.ErrCategoryValidation {
				t.Fatalf("want validation error, got %+v", res)
			}
			if !strings.HasPrefix(res.Content, "[validation error] ") {
				t.Fatalf("missing load-bearing prefix: %q", res.Content)
			}
			if !strings.Contains(res.Content, tc.want) {
				t.Fatalf("error %q does not mention %q", res.Content, tc.want)
			}
			if res.CheckpointNow {
				t.Fatal("validation failures must not force a checkpoint")
			}
		})
	}
}

func TestSetWorkPlanTool_InitialUpdateAndNoOp(t *testing.T) {
	tool, c := newTestWorkPlanTool()

	// Initial snapshot: runtime mints plan id + revision 1, forces checkpoint.
	res := runWorkPlanTool(t, tool, validStepsJSON("in_progress", "pending", "pending"))
	if res.IsError {
		t.Fatalf("initial snapshot rejected: %s", res.Content)
	}
	var out struct {
		PlanID    string `json:"plan_id"`
		Revision  uint64 `json:"revision"`
		Completed int    `json:"completed"`
		Total     int    `json:"total"`
		Changed   bool   `json:"changed"`
	}
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatalf("result not JSON: %v (%q)", err, res.Content)
	}
	if !strings.HasPrefix(out.PlanID, "wp1_") || out.Revision != 1 || out.Completed != 0 || out.Total != 3 || !out.Changed {
		t.Fatalf("unexpected initial result: %+v", out)
	}
	if !res.CheckpointNow {
		t.Fatal("changed snapshot must set CheckpointNow")
	}
	if c.TakePendingEvent() == nil {
		t.Fatal("changed snapshot must stage a pending event")
	}

	// Identical normalized snapshot: no-op — same revision, no checkpoint, no event.
	res = runWorkPlanTool(t, tool, validStepsJSON("in_progress", "pending", "pending"))
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatalf("no-op result not JSON: %v", err)
	}
	if out.Changed || out.Revision != 1 {
		t.Fatalf("identical snapshot must be a no-op: %+v", out)
	}
	if res.CheckpointNow {
		t.Fatal("no-op must not force a checkpoint")
	}
	if c.TakePendingEvent() != nil {
		t.Fatal("no-op must not stage an event")
	}

	// Progress update: revision increments; explanation is model-writable.
	res = runWorkPlanTool(t, tool, `{"explanation":"first stage landed","steps":[{"content":"Inspect current behavior","status":"completed"},{"content":"Implement the change","status":"in_progress"},{"content":"Exercise the real call path","status":"pending"}]}`)
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatalf("update result not JSON: %v", err)
	}
	if !out.Changed || out.Revision != 2 || out.Completed != 1 {
		t.Fatalf("unexpected update result: %+v", out)
	}
	snap := c.ActiveSnapshot()
	if snap == nil || snap.Explanation != "first stage landed" || snap.RunID != testWorkPlanRunID {
		t.Fatalf("controller snapshot wrong: %+v", snap)
	}
}

// Runtime-owned fields in the arguments are ignored, never adopted: the
// decoder's typed struct has no such fields, and the controller mints its own.
func TestSetWorkPlanTool_ModelCannotInjectRuntimeFields(t *testing.T) {
	tool, c := newTestWorkPlanTool()
	res := runWorkPlanTool(t, tool, `{"plan_id":"wp1_ffffffffffffffffffffffffffffffff","revision":99,"lifecycle":"completed","close_reason":"run_completed","steps":[{"content":"a","status":"pending"},{"content":"b","status":"pending"}]}`)
	if res.IsError {
		t.Fatalf("rejected: %s", res.Content)
	}
	snap := c.ActiveSnapshot()
	if snap.PlanID == "wp1_ffffffffffffffffffffffffffffffff" {
		t.Fatal("model-supplied plan_id was adopted")
	}
	if snap.Revision != 1 {
		t.Fatalf("model-supplied revision leaked: %d", snap.Revision)
	}
	if snap.Lifecycle != session.WorkPlanActive || snap.CloseReason != "" {
		t.Fatalf("model-supplied lifecycle/close_reason leaked: %+v", snap)
	}
}

func TestSetWorkPlanTool_PolicyAxes(t *testing.T) {
	tool, _ := newTestWorkPlanTool()
	if tool.RequiresApproval() {
		t.Error("must not require approval")
	}
	if tool.IsReadOnlyCall("{}") {
		t.Error("must not be read-only: it mutates durable session metadata")
	}
	if tool.IsConcurrencySafeCall("{}") {
		t.Error("must not be concurrency-safe: revisions observe transcript order")
	}
	if tool.HasMaterialSideEffect("{}") {
		t.Error("must not be material: it never enters the side-effect journal")
	}
	if !tool.SkillExempt() {
		t.Error("skill-exempt core infrastructure (documented in work_plan.go)")
	}
	if !agent.IsSkillExempt(tool) {
		t.Error("agent.IsSkillExempt must see the exemption — all three execution-filter sites gate through it")
	}
	var asAny any = tool
	if _, ok := asAny.(interface{ TrustsDistinctOutcomeProgress() bool }); ok {
		t.Error("must NOT implement BoundedProgressTool: plan revisions are not real progress evidence")
	}
	if _, ok := asAny.(interface{ ToolExposure() agent.ToolExposure }); ok {
		t.Error("must NOT implement ToolExposureProvider: local default (Direct) is the contract")
	}
	info := tool.Info()
	if info.Name != "set_work_plan" {
		t.Errorf("name = %q", info.Name)
	}
	if len(info.Required) != 1 || info.Required[0] != "steps" {
		t.Errorf("Required = %v", info.Required)
	}
}

func TestRunPlanController_CloseForRun(t *testing.T) {
	completedSteps := []session.WorkPlanStep{
		{Content: "a", Status: session.WorkPlanStepCompleted},
		{Content: "b", Status: session.WorkPlanStepCompleted},
	}
	pendingSteps := []session.WorkPlanStep{
		{Content: "a", Status: session.WorkPlanStepCompleted},
		{Content: "b", Status: session.WorkPlanStepPending},
	}
	cases := []struct {
		name       string
		steps      []session.WorkPlanStep
		status     agent.RunStatus
		runErr     error
		wantLife   session.WorkPlanLifecycle
		wantReason string
	}{
		{"clean all complete", completedSteps, agent.RunStatus{}, nil, session.WorkPlanCompleted, session.WorkPlanCloseRunCompleted},
		{"clean pending steps", pendingSteps, agent.RunStatus{}, nil, session.WorkPlanStopped, session.WorkPlanCloseRunCompletedWithPendingSteps},
		{"partial", pendingSteps, agent.RunStatus{Partial: true, FailureCode: runstatus.CodeIterationLimit}, nil, session.WorkPlanStopped, session.WorkPlanClosePartial},
		{"cancelled", pendingSteps, agent.RunStatus{FailureCode: runstatus.CodeUserCancelled}, errors.New("ctx cancelled"), session.WorkPlanStopped, session.WorkPlanCloseCancelled},
		{"hard error", pendingSteps, agent.RunStatus{FailureCode: runstatus.CodeUnexpected}, errors.New("boom"), session.WorkPlanStopped, session.WorkPlanCloseFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newRunPlanController("sess-1", testWorkPlanRunID)
			if _, err := c.Apply("", tc.steps, time.Now()); err != nil {
				t.Fatalf("apply: %v", err)
			}
			c.TakePendingEvent() // drain the apply event
			if !c.CloseForRun(tc.status, tc.runErr, time.Now()) {
				t.Fatal("CloseForRun reported no change on an active plan")
			}
			var sess session.Session
			c.StageForSave(&sess)
			if sess.WorkPlan.Lifecycle != tc.wantLife || sess.WorkPlan.CloseReason != tc.wantReason {
				t.Fatalf("got %s/%s want %s/%s", sess.WorkPlan.Lifecycle, sess.WorkPlan.CloseReason, tc.wantLife, tc.wantReason)
			}
			if len(sess.WorkPlan.Steps) != len(tc.steps) {
				t.Fatal("closure dropped steps; incomplete steps must be preserved")
			}
			if c.TakePendingEvent() == nil {
				t.Fatal("closure must stage a terminal event")
			}
			// Second closure is a no-op: at most one terminal transition.
			if c.CloseForRun(tc.status, tc.runErr, time.Now()) {
				t.Fatal("closing a closed plan must be a no-op")
			}
		})
	}
}

func TestRunPlanController_CloseWithoutPlanIsNoOp(t *testing.T) {
	c := newRunPlanController("sess-1", testWorkPlanRunID)
	if c.CloseForRun(agent.RunStatus{}, nil, time.Now()) {
		t.Fatal("closing a run that never made a plan must report no change")
	}
	var sess session.Session
	prior := &session.WorkPlanSnapshot{PlanID: "wp1_prior", Lifecycle: session.WorkPlanCompleted}
	sess.WorkPlan = prior
	c.StageForSave(&sess)
	if sess.WorkPlan != prior {
		t.Fatal("a run without a plan must leave the previous run's plan untouched")
	}
}

func TestRunPlanController_RestoreRules(t *testing.T) {
	base := &session.WorkPlanSnapshot{
		PlanID:    "wp1_0123456789abcdef0123456789abcdef",
		RunID:     testWorkPlanRunID,
		Revision:  4,
		Lifecycle: session.WorkPlanActive,
		Steps: []session.WorkPlanStep{
			{Content: "a", Status: session.WorkPlanStepCompleted},
			{Content: "b", Status: session.WorkPlanStepInProgress},
		},
	}

	// Same RunID + active: adopted, and an identical re-submission stays a no-op.
	c := newRunPlanController("sess-1", testWorkPlanRunID)
	c.Restore(base)
	if got := c.ActiveSnapshot(); got == nil || got.Revision != 4 {
		t.Fatalf("active same-run plan not adopted: %+v", got)
	}
	res, err := c.Apply("", base.Steps, time.Now())
	if err != nil || res.changed {
		t.Fatalf("re-submitting the restored steps must be a no-op: %+v err=%v", res, err)
	}

	// Different RunID: ignored.
	c = newRunPlanController("sess-1", "run1_ffffffffffffffffffffffffffffffff")
	c.Restore(base)
	if c.ActiveSnapshot() != nil {
		t.Fatal("another run's plan must not be adopted")
	}

	// Closed plan: ignored.
	closed := base.Clone()
	closed.Lifecycle = session.WorkPlanStopped
	c = newRunPlanController("sess-1", testWorkPlanRunID)
	c.Restore(closed)
	if c.ActiveSnapshot() != nil {
		t.Fatal("a closed plan is UI history and must not be re-adopted")
	}
}

func TestRenderWorkPlanForPrompt(t *testing.T) {
	snap := &session.WorkPlanSnapshot{
		Revision:  2,
		Lifecycle: session.WorkPlanActive,
		Steps: []session.WorkPlanStep{
			{Content: "Inspect current behavior", Status: session.WorkPlanStepCompleted},
			{Content: "Implement the change", Status: session.WorkPlanStepInProgress},
			{Content: "Exercise the real call path", Status: session.WorkPlanStepPending},
		},
	}
	got := renderWorkPlanForPrompt(snap)
	for _, want := range []string{"revision 2", "1/3", "[x] Inspect current behavior", "[>] Implement the change", "[ ] Exercise the real call path", "set_work_plan"} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q in:\n%s", want, got)
		}
	}
	closed := snap.Clone()
	closed.Lifecycle = session.WorkPlanCompleted
	if renderWorkPlanForPrompt(closed) != "" {
		t.Error("closed plans must render nothing (never injected as active instructions)")
	}
	if renderWorkPlanForPrompt(nil) != "" {
		t.Error("nil plan must render nothing")
	}
}

func TestEmitWorkPlanUpdated_PersistFirstContract(t *testing.T) {
	bus := NewEventBus()
	ch := bus.Subscribe()
	defer bus.Unsubscribe(ch)
	snap := &session.WorkPlanSnapshot{
		PlanID:    "wp1_0123456789abcdef0123456789abcdef",
		RunID:     testWorkPlanRunID,
		Revision:  2,
		Lifecycle: session.WorkPlanActive,
		Steps: []session.WorkPlanStep{
			{Content: "a", Status: session.WorkPlanStepCompleted},
			{Content: "b", Status: session.WorkPlanStepInProgress},
		},
		UpdatedAt: time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC),
	}
	emitWorkPlanUpdated(bus, "sess-1", snap, time.Date(2026, 8, 14, 0, 0, 1, 0, time.UTC))
	select {
	case evt := <-ch:
		if evt.Type != EventWorkPlanUpdated {
			t.Fatalf("type = %q", evt.Type)
		}
		var payload map[string]any
		if err := json.Unmarshal(evt.Payload, &payload); err != nil {
			t.Fatalf("payload: %v", err)
		}
		if payload["session_id"] != "sess-1" || payload["revision"] != float64(2) ||
			payload["completed"] != float64(1) || payload["total"] != float64(2) {
			t.Fatalf("payload fields: %v", payload)
		}
	default:
		t.Fatal("no event delivered")
	}
	// nil bus / nil snapshot are silent no-ops (persist-first callers pass
	// whatever TakePendingEvent returned).
	emitWorkPlanUpdated(nil, "sess-1", snap, time.Now())
	emitWorkPlanUpdated(bus, "sess-1", nil, time.Now())
	select {
	case evt := <-ch:
		t.Fatalf("unexpected event: %+v", evt)
	default:
	}
}
