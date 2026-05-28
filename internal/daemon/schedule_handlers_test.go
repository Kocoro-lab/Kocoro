package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/schedule"
)

// newTestServerWithScheduleMgr returns a minimal Server+Manager pair for
// testing HTTP schedule handlers. Only ScheduleManager is wired — handlers
// that touch other deps must construct their own helper.
func newTestServerWithScheduleMgr(t *testing.T) (*Server, *schedule.Manager, string) {
	t.Helper()
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "schedules.json")
	mgr := schedule.NewManager(indexPath)
	deps := &ServerDeps{ScheduleManager: mgr}
	srv := &Server{deps: deps}
	return srv, mgr, indexPath
}

func TestHandleCreateSchedule_DefaultStateless(t *testing.T) {
	srv, _, _ := newTestServerWithScheduleMgr(t)
	body := `{"agent":"x","cron":"0 9 * * *","prompt":"p"}`
	req := httptest.NewRequest(http.MethodPost, "/schedules", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	srv.handleCreateSchedule(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var got schedule.Schedule
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Stateful == nil || *got.Stateful {
		t.Errorf("default Create should produce Stateful=*false, got %v", got.Stateful)
	}
}

func TestHandleCreateSchedule_ExplicitStateful(t *testing.T) {
	srv, _, _ := newTestServerWithScheduleMgr(t)
	body := `{"agent":"x","cron":"0 9 * * *","prompt":"p","stateful":true}`
	req := httptest.NewRequest(http.MethodPost, "/schedules", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	srv.handleCreateSchedule(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var got schedule.Schedule
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Stateful == nil || !*got.Stateful {
		t.Errorf("explicit stateful=true lost: got %v", got.Stateful)
	}
}

func TestHandleCreateSchedule_AcceptsBroadcastAndSource(t *testing.T) {
	srv, _, _ := newTestServerWithScheduleMgr(t)
	body := `{"agent":"x","cron":"0 9 * * *","prompt":"p","broadcast":"on","created_from_source":"webview"}`
	req := httptest.NewRequest(http.MethodPost, "/schedules", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	srv.handleCreateSchedule(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var got schedule.Schedule
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Broadcast == nil || *got.Broadcast != true {
		t.Errorf("broadcast not applied on create: %v", got.Broadcast)
	}
	if got.CreatedFromSource != "webview" {
		t.Errorf("created_from_source not applied: %q", got.CreatedFromSource)
	}
}

func TestHandleCreateSchedule_RejectsInvalidBroadcast(t *testing.T) {
	srv, _, _ := newTestServerWithScheduleMgr(t)
	body := `{"agent":"x","cron":"0 9 * * *","prompt":"p","broadcast":"maybe"}`
	req := httptest.NewRequest(http.MethodPost, "/schedules", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	srv.handleCreateSchedule(w, req)
	if w.Code < 400 || w.Code >= 500 {
		t.Fatalf("expected 4xx for invalid broadcast, got %d body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "broadcast") {
		t.Errorf("error body should mention broadcast: %s", w.Body.String())
	}
}

func TestHandlePatchSchedule_FlipStatefulTrue(t *testing.T) {
	srv, mgr, _ := newTestServerWithScheduleMgr(t)
	id, _ := mgr.Create("x", "0 9 * * *", "p", false)

	body := `{"stateful":true}`
	req := httptest.NewRequest(http.MethodPatch, "/schedules/"+id, bytes.NewBufferString(body))
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	srv.handlePatchSchedule(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	got, _ := mgr.Get(id)
	if got.Stateful == nil || !*got.Stateful {
		t.Errorf("PATCH stateful=true did not persist: %v", got.Stateful)
	}
}

// Critical zero-value coverage: a naive PATCH decoder that drops zero values
// would silently swallow stateful:false. The *bool decoder must preserve
// "field present and false" as a non-nil pointer.
func TestHandlePatchSchedule_FlipStatefulFalse(t *testing.T) {
	srv, mgr, _ := newTestServerWithScheduleMgr(t)
	id, _ := mgr.Create("x", "0 9 * * *", "p", true)

	body := `{"stateful":false}`
	req := httptest.NewRequest(http.MethodPatch, "/schedules/"+id, bytes.NewBufferString(body))
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	srv.handlePatchSchedule(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	got, _ := mgr.Get(id)
	if got.Stateful == nil || *got.Stateful {
		t.Errorf("PATCH stateful=false did not persist: %v", got.Stateful)
	}
}

// Operator path: migrate a legacy schedule (no stateful on disk) to stateless
// via PATCH. Mirrors TestManager_Update_MigrateLegacyToStateless at the HTTP layer.
func TestHandlePatchSchedule_MigrateLegacyToStateless(t *testing.T) {
	srv, mgr, indexPath := newTestServerWithScheduleMgr(t)
	legacyRaw := `[{"id":"legacy","agent":"x","cron":"0 9 * * *","prompt":"p","enabled":true,"sync_status":"ok","created_at":"2025-01-01T00:00:00Z"}]`
	if err := os.WriteFile(indexPath, []byte(legacyRaw), 0600); err != nil {
		t.Fatal(err)
	}

	body := `{"stateful":false}`
	req := httptest.NewRequest(http.MethodPatch, "/schedules/legacy", bytes.NewBufferString(body))
	req.SetPathValue("id", "legacy")
	w := httptest.NewRecorder()
	srv.handlePatchSchedule(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	got, _ := mgr.Get("legacy")
	if got.Stateful == nil || *got.Stateful {
		t.Errorf("legacy → stateless via PATCH failed: %v", got.Stateful)
	}
}

// --- Task 7: GET /schedules/{id}/last-run -----------------------------------

func TestHandleScheduleLastRun_NeverRun(t *testing.T) {
	srv, mgr, _ := newTestServerWithScheduleMgr(t)
	id, err := mgr.Create("tracker", "0 9 * * *", "p", false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/schedules/"+id+"/last-run", nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	srv.handleScheduleLastRun(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var got schedule.LastRunSummary
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.SessionID != "" || got.LastRunAt != nil {
		t.Errorf("never-run should serialize empty/nil, got %+v", got)
	}
}

func TestHandleScheduleLastRun_NormalReturn(t *testing.T) {
	srv, mgr, indexPath := newTestServerWithScheduleMgr(t)
	id, err := mgr.Create("tracker", "0 9 * * *", "p", false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	shan := filepath.Dir(indexPath)
	sessDir := filepath.Join(shan, "agents", "tracker", "sessions")
	if err := os.MkdirAll(sessDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "sess-X.json"), []byte(
		`{"id":"sess-X","schema_version":1,"messages":[{"role":"assistant","content":"hello world"}]}`,
	), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := mgr.MarkLastRun(id, "sess-X", time.Now(), 0, 1); err != nil {
		t.Fatalf("MarkLastRun: %v", err)
	}
	srv.deps.ShannonDir = shan

	req := httptest.NewRequest(http.MethodGet, "/schedules/"+id+"/last-run", nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	srv.handleScheduleLastRun(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var got schedule.LastRunSummary
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.SessionID != "sess-X" {
		t.Errorf("SessionID = %q, want %q", got.SessionID, "sess-X")
	}
	if len(got.Turns) != 1 || got.Turns[0].Text != "hello world" {
		t.Errorf("turns: %+v", got.Turns)
	}
}

func TestHandleScheduleLastRun_UnknownID404(t *testing.T) {
	srv, _, _ := newTestServerWithScheduleMgr(t)
	req := httptest.NewRequest(http.MethodGet, "/schedules/nope/last-run", nil)
	req.SetPathValue("id", "nope")
	w := httptest.NewRecorder()
	srv.handleScheduleLastRun(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestHandleScheduleLastRun_MissingSession500(t *testing.T) {
	srv, mgr, indexPath := newTestServerWithScheduleMgr(t)
	id, err := mgr.Create("tracker", "0 9 * * *", "p", false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.MarkLastRun(id, "sess-gone", time.Now(), 0, 4); err != nil {
		t.Fatalf("MarkLastRun: %v", err)
	}
	srv.deps.ShannonDir = filepath.Dir(indexPath)

	req := httptest.NewRequest(http.MethodGet, "/schedules/"+id+"/last-run", nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	srv.handleScheduleLastRun(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (session file missing)", w.Code)
	}
}

// --- Task A8: broadcast + created_from_source surface --------------------------

// GET /schedules returns the new Broadcast + CreatedFromSource JSON tags so
// Desktop can render the IM-push state. Backed by plain json.Marshal of the
// Schedule struct from A1 — this is a regression guard, not new code.
func TestHandleListSchedules_IncludesBroadcastFields(t *testing.T) {
	srv, mgr, _ := newTestServerWithScheduleMgr(t)
	bTrue := true
	if _, err := mgr.CreateWithOpts("x", "0 9 * * *", "p", false, schedule.CreateOpts{
		Broadcast:         &bTrue,
		CreatedFromSource: "slack",
	}); err != nil {
		t.Fatalf("CreateWithOpts: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/schedules", nil)
	w := httptest.NewRecorder()
	srv.handleListSchedules(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"broadcast":true`) {
		t.Errorf("response missing broadcast field: %s", body)
	}
	if !strings.Contains(body, `"created_from_source":"slack"`) {
		t.Errorf("response missing created_from_source field: %s", body)
	}
}

// GET /schedules/{id} surfaces the same fields for a single-row lookup. The
// single-resource handler json-marshals *Schedule directly (no list wrapper),
// so this is a separate code path.
func TestHandleGetSchedule_IncludesBroadcastFields(t *testing.T) {
	srv, mgr, _ := newTestServerWithScheduleMgr(t)
	bFalse := false
	id, err := mgr.CreateWithOpts("x", "0 9 * * *", "p", false, schedule.CreateOpts{
		Broadcast:         &bFalse,
		CreatedFromSource: "feishu",
	})
	if err != nil {
		t.Fatalf("CreateWithOpts: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/schedules/"+id, nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	srv.handleGetSchedule(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"broadcast":false`) {
		t.Errorf("response missing broadcast=false: %s", body)
	}
	if !strings.Contains(body, `"created_from_source":"feishu"`) {
		t.Errorf("response missing created_from_source=feishu: %s", body)
	}
}

// PATCH /schedules/{id} accepts the broadcast enum ("auto"|"on"|"off") and
// rewrites Schedule.Broadcast via UpdateOpts.Broadcast. "on" → *true.
func TestHandlePatchSchedule_AcceptsBroadcastOn(t *testing.T) {
	srv, mgr, _ := newTestServerWithScheduleMgr(t)
	id, err := mgr.Create("x", "0 9 * * *", "p", false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	body := `{"broadcast":"on"}`
	req := httptest.NewRequest(http.MethodPatch, "/schedules/"+id, bytes.NewBufferString(body))
	req.SetPathValue("id", id)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handlePatchSchedule(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	got, err := mgr.Get(id)
	if err != nil || got == nil {
		t.Fatalf("Get: %v %v", err, got)
	}
	if got.Broadcast == nil || *got.Broadcast != true {
		t.Errorf("PATCH broadcast=on did not produce *true: got %v", got.Broadcast)
	}
}

// PATCH broadcast=off → *false. Mirrors the on path; the BroadcastOpt wrapper
// distinguishes "leave alone" (nil) from "rewrite to *false" (non-nil with
// *false inside), so this catches regressions that drop the latter case.
func TestHandlePatchSchedule_AcceptsBroadcastOff(t *testing.T) {
	srv, mgr, _ := newTestServerWithScheduleMgr(t)
	bTrue := true
	id, err := mgr.CreateWithOpts("x", "0 9 * * *", "p", false, schedule.CreateOpts{
		Broadcast: &bTrue,
	})
	if err != nil {
		t.Fatalf("CreateWithOpts: %v", err)
	}

	body := `{"broadcast":"off"}`
	req := httptest.NewRequest(http.MethodPatch, "/schedules/"+id, bytes.NewBufferString(body))
	req.SetPathValue("id", id)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handlePatchSchedule(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	got, _ := mgr.Get(id)
	if got.Broadcast == nil || *got.Broadcast != false {
		t.Errorf("PATCH broadcast=off did not produce *false: got %v", got.Broadcast)
	}
}

// PATCH broadcast="auto" clears Broadcast back to nil (smart default).
func TestHandlePatchSchedule_AcceptsBroadcastAutoClearsBack(t *testing.T) {
	srv, mgr, _ := newTestServerWithScheduleMgr(t)
	bTrue := true
	id, err := mgr.CreateWithOpts("x", "0 9 * * *", "p", false, schedule.CreateOpts{
		Broadcast: &bTrue,
	})
	if err != nil {
		t.Fatalf("CreateWithOpts: %v", err)
	}

	body := `{"broadcast":"auto"}`
	req := httptest.NewRequest(http.MethodPatch, "/schedules/"+id, bytes.NewBufferString(body))
	req.SetPathValue("id", id)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handlePatchSchedule(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	got, _ := mgr.Get(id)
	if got.Broadcast != nil {
		t.Errorf("PATCH broadcast=auto did not clear back to nil: got %v", got.Broadcast)
	}
}

// PATCH with an invalid broadcast value must return 4xx (not silent-accept,
// not 5xx). The LLM- and Desktop-facing contract is "auto"|"on"|"off".
func TestHandlePatchSchedule_RejectsInvalidBroadcast(t *testing.T) {
	srv, mgr, _ := newTestServerWithScheduleMgr(t)
	id, err := mgr.Create("x", "0 9 * * *", "p", false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	body := `{"broadcast":"maybe"}`
	req := httptest.NewRequest(http.MethodPatch, "/schedules/"+id, bytes.NewBufferString(body))
	req.SetPathValue("id", id)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handlePatchSchedule(w, req)
	if w.Code < 400 || w.Code >= 500 {
		t.Errorf("expected 4xx for invalid broadcast, got %d body %s", w.Code, w.Body.String())
	}
	// Stronger assertion: the 4xx must actually mention `broadcast` so we know
	// the validation fired here, not the pre-existing "no fields to update"
	// branch (which would also produce a 4xx but mask a regression where the
	// handler silently accepts unknown enums).
	if !strings.Contains(w.Body.String(), "broadcast") {
		t.Errorf("4xx body should reference broadcast validation, got %s", w.Body.String())
	}
	got, _ := mgr.Get(id)
	if got.Broadcast != nil {
		t.Errorf("invalid broadcast must not mutate stored schedule: got %v", got.Broadcast)
	}
}
