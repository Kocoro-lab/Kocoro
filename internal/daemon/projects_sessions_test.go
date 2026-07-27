package daemon

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/projects"
)

// newProjectTestServer builds a Server wired with a real SessionCache and an
// empty agents dir (so allSessionScopes resolves to just the default scope).
func newProjectTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	shannonDir := t.TempDir()
	deps := &ServerDeps{
		ShannonDir:   shannonDir,
		AgentsDir:    filepath.Join(shannonDir, "agents"),
		ProjectsDir:  filepath.Join(shannonDir, "projects"),
		SessionCache: NewSessionCache(shannonDir),
	}
	return &Server{deps: deps}, shannonDir
}

// seedProjectSession persists a session into the default scope with the given project
// tag and message count (message count drives the MsgCount>0 filter).
func seedProjectSession(t *testing.T, s *Server, id, projectID string, msgs int) {
	t.Helper()
	mgr := s.deps.SessionCache.GetOrCreateManager(s.deps.SessionCache.SessionsDir(""))
	sess := mgr.NewSessionWithID(id)
	sess.ProjectID = projectID
	for i := 0; i < msgs; i++ {
		sess.Messages = append(sess.Messages, client.Message{
			Role:    "user",
			Content: client.NewTextContent("hi"),
		})
	}
	if err := mgr.Save(); err != nil {
		t.Fatalf("save session %q: %v", id, err)
	}
}

// TestCountSessionsForProject verifies the single-project counter honors the
// MsgCount>0 rule (empty sessions are excluded, matching the detail listing).
func TestCountSessionsForProject(t *testing.T) {
	s, _ := newProjectTestServer(t)
	p, err := projects.CreateProject(s.deps.ProjectsDir, &projects.ProjectCreateRequest{Name: "Kyoto"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	seedProjectSession(t, s, "2026-07-24-aaaaaaaaaaaa", p.ID, 2)          // counts
	seedProjectSession(t, s, "2026-07-24-bbbbbbbbbbbb", p.ID, 0)          // empty → excluded
	seedProjectSession(t, s, "2026-07-24-cccccccccccc", "proj-other", 2) // other project

	if got := s.countSessionsForProject(p.ID); got != 1 {
		t.Fatalf("countSessionsForProject = %d, want 1", got)
	}
	if got := s.countSessionsForProject("proj-other"); got != 1 {
		t.Fatalf("countSessionsForProject(other) = %d, want 1", got)
	}
	if got := s.countSessionsForProject("proj-none"); got != 0 {
		t.Fatalf("countSessionsForProject(none) = %d, want 0", got)
	}
}

// TestDeleteSessionsOfProject verifies the destructive delete removes every
// session filed under the project (empty ones too) and leaves others intact.
func TestDeleteSessionsOfProject(t *testing.T) {
	s, _ := newProjectTestServer(t)
	p, err := projects.CreateProject(s.deps.ProjectsDir, &projects.ProjectCreateRequest{Name: "Kyoto"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	seedProjectSession(t, s, "2026-07-24-aaaaaaaaaaaa", p.ID, 2)          // deleted
	seedProjectSession(t, s, "2026-07-24-bbbbbbbbbbbb", p.ID, 0)          // deleted (empty too)
	seedProjectSession(t, s, "2026-07-24-cccccccccccc", "proj-other", 2) // survives

	if got := s.deleteSessionsOfProject(p.ID); got != 2 {
		t.Fatalf("deleteSessionsOfProject = %d, want 2", got)
	}

	mgr := s.deps.SessionCache.GetOrCreateManager(s.deps.SessionCache.SessionsDir(""))
	summaries, err := mgr.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	remaining := map[string]bool{}
	for _, sum := range summaries {
		remaining[sum.ID] = true
	}
	if remaining["2026-07-24-aaaaaaaaaaaa"] || remaining["2026-07-24-bbbbbbbbbbbb"] {
		t.Fatalf("deleted sessions still present: %v", remaining)
	}
	if !remaining["2026-07-24-cccccccccccc"] {
		t.Fatalf("unrelated session was deleted: %v", remaining)
	}
}

// patchSession issues a PATCH /sessions/{id} against the handler and returns the
// recorder.
func patchSession(s *Server, id, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("PATCH", "/sessions/"+id, strings.NewReader(body))
	req.SetPathValue("id", id)
	rr := httptest.NewRecorder()
	s.handlePatchSession(rr, req)
	return rr
}

// TestHandlePatchSession_ProjectRefile covers filing, then unfiling, a session
// via PATCH /sessions and confirms the change is persisted.
func TestHandlePatchSession_ProjectRefile(t *testing.T) {
	s, _ := newProjectTestServer(t)
	p, err := projects.CreateProject(s.deps.ProjectsDir, &projects.ProjectCreateRequest{Name: "Kyoto"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	const id = "2026-07-24-dddddddddddd"
	seedProjectSession(t, s, id, "", 2)

	mgr := s.deps.SessionCache.GetOrCreateManager(s.deps.SessionCache.SessionsDir(""))

	// File into the project.
	if rr := patchSession(s, id, `{"project_id":"`+p.ID+`"}`); rr.Code != 200 {
		t.Fatalf("PATCH file: code = %d, body = %s", rr.Code, rr.Body.String())
	}
	if got, _ := mgr.Load(id); got == nil || got.ProjectID != p.ID {
		t.Fatalf("after file: project_id not persisted (%+v)", got)
	}

	// Unfile via empty string.
	if rr := patchSession(s, id, `{"project_id":""}`); rr.Code != 200 {
		t.Fatalf("PATCH unfile: code = %d, body = %s", rr.Code, rr.Body.String())
	}
	if got, _ := mgr.Load(id); got == nil || got.ProjectID != "" {
		t.Fatalf("after unfile: project_id still set (%+v)", got)
	}
}

// TestHandlePatchSession_RejectsUnknownProject ensures filing into a
// non-existent project is refused so no session points at a ghost project.
func TestHandlePatchSession_RejectsUnknownProject(t *testing.T) {
	s, _ := newProjectTestServer(t)
	const id = "2026-07-24-eeeeeeeeeeee"
	seedProjectSession(t, s, id, "", 2)

	rr := patchSession(s, id, `{"project_id":"proj-ghost01"}`)
	if rr.Code != 400 {
		t.Fatalf("PATCH into ghost project: code = %d, want 400 (body %s)", rr.Code, rr.Body.String())
	}
}

// TestHandlePatchSession_RejectsWhenProjectsUnconfigured verifies a non-empty
// project_id is rejected (not silently written) when the projects store is off.
func TestHandlePatchSession_RejectsWhenProjectsUnconfigured(t *testing.T) {
	shannonDir := t.TempDir()
	s := &Server{deps: &ServerDeps{
		ShannonDir:   shannonDir,
		AgentsDir:    filepath.Join(shannonDir, "agents"),
		ProjectsDir:  "", // projects store not configured
		SessionCache: NewSessionCache(shannonDir),
	}}

	rr := patchSession(s, "2026-07-24-ffffffffffff", `{"project_id":"proj-abc123"}`)
	if rr.Code != 400 {
		t.Fatalf("PATCH with unconfigured projects: code = %d, want 400 (body %s)", rr.Code, rr.Body.String())
	}
}
