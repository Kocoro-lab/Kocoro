package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
