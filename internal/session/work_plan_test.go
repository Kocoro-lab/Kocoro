package session

import (
	"encoding/json"
	"testing"
	"time"
)

// Legacy session JSON without work_plan must decode unchanged and re-encode
// without inventing the field (additive-optional contract; no SchemaVersion
// involvement).
func TestWorkPlan_LegacySessionJSONRoundTrips(t *testing.T) {
	legacy := []byte(`{"id":"legacy-1","created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z","title":"t","cwd":"/tmp","messages":[]}`)
	var sess Session
	if err := json.Unmarshal(legacy, &sess); err != nil {
		t.Fatalf("decode legacy session: %v", err)
	}
	if sess.WorkPlan != nil {
		t.Fatalf("legacy session decoded a phantom WorkPlan: %+v", sess.WorkPlan)
	}
	out, err := json.Marshal(&sess)
	if err != nil {
		t.Fatalf("re-encode legacy session: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if _, present := m["work_plan"]; present {
		t.Fatal("re-encoded legacy session grew a work_plan key (omitempty violated)")
	}
}

func sampleWorkPlan(lifecycle WorkPlanLifecycle, closeReason string) *WorkPlanSnapshot {
	return &WorkPlanSnapshot{
		PlanID:      "wp1_0123456789abcdef0123456789abcdef",
		RunID:       "run1_0123456789abcdef0123456789abcdef",
		Revision:    3,
		Lifecycle:   lifecycle,
		CloseReason: closeReason,
		Explanation: "Adjusted after discovering a verification requirement.",
		Steps: []WorkPlanStep{
			{Content: "Inspect current behavior", Status: WorkPlanStepCompleted},
			{Content: "Implement the change", Status: WorkPlanStepInProgress},
			{Content: "Exercise the real call path", Status: WorkPlanStepPending},
		},
		UpdatedAt: time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC),
	}
}

func TestWorkPlan_PersistsAndLoadsAcrossLifecycles(t *testing.T) {
	cases := []struct {
		name        string
		lifecycle   WorkPlanLifecycle
		closeReason string
	}{
		{"active", WorkPlanActive, ""},
		{"completed", WorkPlanCompleted, WorkPlanCloseRunCompleted},
		{"stopped", WorkPlanStopped, WorkPlanClosePartial},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store := NewStore(dir)
			sess := &Session{
				ID:        "wp-" + tc.name,
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
				Title:     "work plan " + tc.name,
				WorkPlan:  sampleWorkPlan(tc.lifecycle, tc.closeReason),
			}
			if err := store.Save(sess); err != nil {
				t.Fatalf("save: %v", err)
			}
			loaded, err := store.Load(sess.ID)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			got := loaded.WorkPlan
			if got == nil {
				t.Fatal("WorkPlan did not survive save/load")
			}
			want := sess.WorkPlan
			if got.PlanID != want.PlanID || got.RunID != want.RunID ||
				got.Revision != want.Revision || got.Lifecycle != want.Lifecycle ||
				got.CloseReason != want.CloseReason || got.Explanation != want.Explanation {
				t.Fatalf("scalar fields drifted: got %+v want %+v", got, want)
			}
			if len(got.Steps) != len(want.Steps) {
				t.Fatalf("steps count drifted: got %d want %d", len(got.Steps), len(want.Steps))
			}
			for i := range got.Steps {
				if got.Steps[i] != want.Steps[i] {
					t.Fatalf("step %d drifted: got %+v want %+v", i, got.Steps[i], want.Steps[i])
				}
			}
			if !got.UpdatedAt.Equal(want.UpdatedAt) {
				t.Fatalf("UpdatedAt drifted: got %v want %v", got.UpdatedAt, want.UpdatedAt)
			}
		})
	}
}

// Compaction rewrites CompactionCheckpoint/Messages, never the top-level
// WorkPlan. This pins the field against accidental clearing by checkpoint
// application code paths that rebuild session state.
func TestWorkPlan_SurvivesCompactionCheckpointRewrite(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	sess := &Session{
		ID:        "wp-compaction",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		WorkPlan:  sampleWorkPlan(WorkPlanActive, ""),
	}
	if err := store.Save(sess); err != nil {
		t.Fatalf("save: %v", err)
	}
	sess.CompactionCheckpoint = &CompactionCheckpoint{
		SchemaVersion:       CompactionCheckpointSchemaVersion,
		ArchiveThroughIndex: 0,
		Messages:            nil,
	}
	if err := store.Save(sess); err != nil {
		t.Fatalf("save with checkpoint: %v", err)
	}
	loaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.WorkPlan == nil || loaded.WorkPlan.PlanID != sess.WorkPlan.PlanID {
		t.Fatal("WorkPlan lost across a compaction-checkpoint rewrite")
	}
}

func TestWorkPlanSnapshot_CloneIsDeep(t *testing.T) {
	orig := sampleWorkPlan(WorkPlanActive, "")
	cp := orig.Clone()
	cp.Steps[0].Status = WorkPlanStepPending
	cp.Revision = 99
	if orig.Steps[0].Status != WorkPlanStepCompleted {
		t.Fatal("Clone shares the Steps backing array")
	}
	if orig.Revision != 3 {
		t.Fatal("Clone shares scalar state")
	}
	var nilSnap *WorkPlanSnapshot
	if nilSnap.Clone() != nil {
		t.Fatal("nil Clone must return nil")
	}
}

func TestWorkPlanSnapshot_CompletedStepCount(t *testing.T) {
	if got := sampleWorkPlan(WorkPlanActive, "").CompletedStepCount(); got != 1 {
		t.Fatalf("CompletedStepCount = %d, want 1", got)
	}
	var nilSnap *WorkPlanSnapshot
	if nilSnap.CompletedStepCount() != 0 {
		t.Fatal("nil CompletedStepCount must be 0")
	}
}
