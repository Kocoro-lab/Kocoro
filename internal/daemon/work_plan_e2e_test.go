package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// workPlanScriptedGateway scripts the main run by how many set_work_plan
// results ("wp1_" ids) the request transcript already carries — call-order
// independent, so async smart-title calls cannot steal a scripted slot.
//
//	0 results + primary text → tool_use set_work_plan (rev 1: step1 in_progress)
//	1 result                 → tool_use set_work_plan (rev 2: finalSteps)
//	2+ results               → plain final answer
func workPlanScriptedGateway(t *testing.T, primary string, finalSteps string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		raw := string(body)
		planResults := strings.Count(raw, "wp1_")
		resp := client.CompletionResponse{Model: "test-model", FinishReason: "end_turn", OutputText: "ok"}
		switch {
		case planResults >= 2:
			resp.OutputText = "All stages finished; report saved."
		case planResults == 1:
			resp.FinishReason = "tool_use"
			resp.ToolCalls = []client.FunctionCall{{
				ID:        "toolu_wp_2",
				Name:      "set_work_plan",
				Arguments: json.RawMessage(finalSteps),
			}}
		case strings.Contains(raw, primary):
			resp.FinishReason = "tool_use"
			resp.ToolCalls = []client.FunctionCall{{
				ID:        "toolu_wp_1",
				Name:      "set_work_plan",
				Arguments: json.RawMessage(`{"steps":[{"content":"Research the sources","status":"in_progress"},{"content":"Draft the report","status":"pending"},{"content":"Verify citations","status":"pending"}]}`),
			}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

type workPlanEventSummary struct {
	Revision    uint64 `json:"revision"`
	Lifecycle   string `json:"lifecycle"`
	CloseReason string `json:"close_reason"`
	Completed   int    `json:"completed"`
	Total       int    `json:"total"`
}

// drainWorkPlanEvents collects work_plan.updated events until the TERMINAL
// lifecycle event arrives (or a 3s deadline — generous so a loaded runner's
// final-save gap cannot flake the sequence assertion into a confusing count
// mismatch).
func drainWorkPlanEvents(t *testing.T, ch <-chan Event) []workPlanEventSummary {
	t.Helper()
	deadline := time.After(3 * time.Second)
	var out []workPlanEventSummary
	for {
		select {
		case evt := <-ch:
			if evt.Type != EventWorkPlanUpdated {
				continue
			}
			var s workPlanEventSummary
			if err := json.Unmarshal(evt.Payload, &s); err != nil {
				t.Fatalf("decode work_plan.updated: %v", err)
			}
			out = append(out, s)
			if s.Lifecycle != "active" {
				return out // terminal closure observed
			}
		case <-deadline:
			return out
		}
	}
}

func runWorkPlanE2E(t *testing.T, finalSteps string) ([]workPlanEventSummary, *session.Session) {
	t.Helper()
	const primary = "compile the quarterly research report"
	ts := httptest.NewServer(workPlanScriptedGateway(t, primary, finalSteps))
	defer ts.Close()

	deps := runAgentContractTestDeps(t, ts.URL)
	defer deps.SessionCache.CloseAll()
	deps.Config.Agent.MaxIterations = 6
	deps.EventBus = NewEventBus()
	sub := deps.EventBus.Subscribe()
	defer deps.EventBus.Unsubscribe(sub)

	res, err := RunAgent(context.Background(), deps,
		RunAgentRequest{Text: primary, Source: "desktop"}, &accumulatingHandler{})
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if res == nil || res.SessionID == "" {
		t.Fatal("no session id")
	}
	events := drainWorkPlanEvents(t, sub)

	mgr := deps.SessionCache.GetOrCreateManager(deps.SessionCache.SessionsDir(""))
	sess, err := mgr.Load(res.SessionID)
	if err != nil {
		inSessions, _ := filepath.Glob(filepath.Join(deps.ShannonDir, "sessions", "*"))
		t.Fatalf("load session: %v\nsessions dir: %v", err, inSessions)
	}
	return events, sess
}

// Clean run, every step completed → lifecycle completed / run_completed.
// Receiving BOTH active revisions (1 and 2) proves the forced checkpoint
// bypassed the 2s debounce: rev 2 lands well inside the debounce window and
// its event only exists because a durable save preceded it.
func TestE2E_WorkPlan_CleanCompletion(t *testing.T) {
	events, sess := runWorkPlanE2E(t,
		`{"explanation":"all stages landed","steps":[{"content":"Research the sources","status":"completed"},{"content":"Draft the report","status":"completed"},{"content":"Verify citations","status":"completed"}]}`)

	if len(events) != 3 {
		t.Fatalf("want 3 events (rev1 active, rev2 active, rev3 completed), got %d: %+v", len(events), events)
	}
	if events[0].Revision != 1 || events[0].Lifecycle != "active" ||
		events[1].Revision != 2 || events[1].Lifecycle != "active" {
		t.Fatalf("active revision sequence wrong: %+v", events)
	}
	terminal := events[2]
	if terminal.Revision != 3 || terminal.Lifecycle != "completed" ||
		terminal.CloseReason != session.WorkPlanCloseRunCompleted ||
		terminal.Completed != 3 || terminal.Total != 3 {
		t.Fatalf("terminal event wrong: %+v", terminal)
	}

	wp := sess.WorkPlan
	if wp == nil {
		t.Fatal("session lost work_plan")
	}
	if wp.Lifecycle != session.WorkPlanCompleted || wp.CloseReason != session.WorkPlanCloseRunCompleted {
		t.Fatalf("persisted closure wrong: %+v", wp)
	}
	if wp.Revision != terminal.Revision {
		t.Fatalf("persisted revision %d != last emitted %d (an event escaped without its save)", wp.Revision, terminal.Revision)
	}
	if sess.InProgress {
		t.Fatal("session left InProgress after a clean run")
	}
}

// Clean run with the model leaving pending steps → stopped /
// run_completed_with_pending_steps; incomplete steps preserved; the outer run
// result is not downgraded (RunAgent returned nil error above).
func TestE2E_WorkPlan_CleanRunWithPendingSteps(t *testing.T) {
	events, sess := runWorkPlanE2E(t,
		`{"steps":[{"content":"Research the sources","status":"completed"},{"content":"Draft the report","status":"in_progress"},{"content":"Verify citations","status":"pending"}]}`)

	if len(events) != 3 {
		t.Fatalf("want 3 events, got %d: %+v", len(events), events)
	}
	terminal := events[2]
	if terminal.Lifecycle != "stopped" || terminal.CloseReason != session.WorkPlanCloseRunCompletedWithPendingSteps {
		t.Fatalf("terminal event wrong: %+v", terminal)
	}
	wp := sess.WorkPlan
	if wp == nil || wp.Lifecycle != session.WorkPlanStopped ||
		wp.CloseReason != session.WorkPlanCloseRunCompletedWithPendingSteps {
		t.Fatalf("persisted closure wrong: %+v", wp)
	}
	if len(wp.Steps) != 3 || wp.Steps[2].Status != session.WorkPlanStepPending {
		t.Fatalf("incomplete steps not preserved: %+v", wp.Steps)
	}
}
