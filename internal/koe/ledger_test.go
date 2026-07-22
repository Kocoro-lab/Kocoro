//go:build darwin && cgo

package koe

import "testing"

func TestBeginTaskLaneSelection(t *testing.T) {
	state := NewCallState("burst-1", "")
	first := state.BeginTask("check the weather", "")
	if first.ID != "t01" || first.ThreadID != "burst-1" || first.State != TaskRunning || first.Revision != 1 {
		t.Fatalf("first task defaults wrong: %+v", first)
	}
	second := state.BeginTask("sort my email", "")
	if second.ThreadID != "burst-1.t02" {
		t.Fatalf("concurrent same-agent task must fork a sub-lane: %+v", second)
	}
	otherAgent := state.BeginTask("draft the report", "writer")
	if otherAgent.ThreadID != "burst-1" {
		t.Fatalf("different agent owns a separate main route: %+v", otherAgent)
	}
	state.LandResult(first.ID, SayResult{Status: "ok", SpokenSummary: "sunny"})
	sequential := state.BeginTask("book a table", "")
	if sequential.ThreadID != "burst-1" {
		t.Fatalf("completed main lane must be reused: %+v", sequential)
	}
}

func TestBeginFollowUpRevision(t *testing.T) {
	state := NewCallState("burst-2", "")
	first := state.BeginTask("compare weather", "")
	if _, ok := state.BeginFollowUp("t99", "add Shanghai"); ok {
		t.Fatal("unknown task id must not resolve")
	}
	followUp, ok := state.BeginFollowUp(first.ID, "add Shanghai")
	if !ok || followUp.Revision != 2 || followUp.Label != "add Shanghai" {
		t.Fatalf("follow-up bookkeeping wrong: ok=%v task=%+v", ok, followUp)
	}
	state.LandResult(first.ID, SayResult{Status: "ok", SpokenSummary: "done"})
	reopened, ok := state.BeginFollowUp(first.ID, "now add Osaka")
	if !ok || reopened.State != TaskRunning || reopened.Revision != 3 {
		t.Fatalf("completed task must reopen on follow-up: ok=%v task=%+v", ok, reopened)
	}
}

func TestLandResultUpdatesTaskLifecycle(t *testing.T) {
	state := NewCallState("burst-3", "")
	first := state.BeginTask("task a", "")
	state.LandResult(first.ID, SayResult{Status: "injected"})
	if got, _ := state.TaskByID(first.ID); got.State != TaskRunning {
		t.Fatalf("injected landing mutated state: %+v", got)
	}
	state.LandResult(first.ID, SayResult{Status: "ok", SpokenSummary: "sunny", Context: "detail"})
	if got, _ := state.TaskByID(first.ID); got.State != TaskCompleted || got.SpokenSummary != "sunny" || got.DeliveredRevision != 1 {
		t.Fatalf("completed landing wrong: %+v", got)
	}
	failed := state.BeginTask("task b", "")
	state.LandResult(failed.ID, SayResult{Status: "failed", FailReason: "boom"})
	if got, _ := state.TaskByID(failed.ID); got.State != TaskFailed || got.FailReason != "boom" {
		t.Fatalf("failed landing wrong: %+v", got)
	}
	cancelled := state.BeginTask("task c", "")
	state.MarkCancelled(cancelled.ID)
	if got, _ := state.TaskByID(cancelled.ID); got.State != TaskCancelled {
		t.Fatalf("cancelled landing wrong: %+v", got)
	}
}
