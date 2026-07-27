package daemon

import (
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/Kocoro-lab/ShanClaw/internal/projects"
)

// projectsReady reports whether the daemon is configured to serve project
// endpoints and, if not, writes a 500. Every project handler calls this first.
func (s *Server) projectsReady(w http.ResponseWriter) bool {
	if s.deps == nil || s.deps.ProjectsDir == "" {
		writeError(w, http.StatusInternalServerError, "projects not configured")
		return false
	}
	return true
}

// countSessionsByProject tallies how many sessions reference each project id,
// across the default scope and every named agent. Best-effort: scopes that fail
// to list are skipped so a single bad directory doesn't blank the whole page.
func (s *Server) countSessionsByProject() map[string]int {
	counts := make(map[string]int)
	scopes, err := s.allSessionScopes()
	if err != nil {
		return counts
	}
	for _, scope := range scopes {
		mgr := s.deps.SessionCache.GetOrCreateManager(s.deps.SessionCache.SessionsDir(scope))
		summaries, err := mgr.List()
		if err != nil {
			continue
		}
		for _, sum := range summaries {
			// Match the detail listing, which drops empty sessions via
			// enrichSummaries — otherwise the card count exceeds the chats shown.
			if sum.ProjectID != "" && sum.MsgCount > 0 {
				counts[sum.ProjectID]++
			}
		}
	}
	return counts
}

// countSessionsForProject tallies sessions for a single project across scopes,
// without building the whole project→count map — used by GET /projects/{id},
// which needs exactly one count. Same MsgCount>0 rule as countSessionsByProject.
func (s *Server) countSessionsForProject(projectID string) int {
	scopes, err := s.allSessionScopes()
	if err != nil {
		return 0
	}
	n := 0
	for _, scope := range scopes {
		mgr := s.deps.SessionCache.GetOrCreateManager(s.deps.SessionCache.SessionsDir(scope))
		summaries, err := mgr.List()
		if err != nil {
			continue
		}
		for _, sum := range summaries {
			if sum.ProjectID == projectID && sum.MsgCount > 0 {
				n++
			}
		}
	}
	return n
}

// deleteSessionsOfProject permanently deletes every session filed under the
// given project, across the default scope and every named agent. Mirrors the
// single-session delete path (cancel active run → Manager.Delete removes JSON +
// index → clear route bindings). Best-effort: a session that fails to delete is
// skipped (the run path also guards on project existence as a backstop). Called
// before DeleteProject removes the dir so no session is left pointing at a
// non-existent project. Returns the number of sessions deleted.
func (s *Server) deleteSessionsOfProject(projectID string) int {
	scopes, err := s.allSessionScopes()
	if err != nil {
		return 0
	}
	deleted := 0
	for _, scope := range scopes {
		mgr := s.deps.SessionCache.GetOrCreateManager(s.deps.SessionCache.SessionsDir(scope))
		summaries, err := mgr.List()
		if err != nil {
			continue
		}
		for _, sum := range summaries {
			if sum.ProjectID != projectID {
				continue
			}
			s.deps.SessionCache.CancelBySessionID(sum.ID)
			if err := mgr.Delete(sum.ID); err != nil {
				continue
			}
			s.deps.SessionCache.ClearSessionBindings(sum.ID)
			deleted++
		}
	}
	return deleted
}

// handleProjects lists all projects (most-recently-updated first) with per-
// project session counts.
func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	if !s.projectsReady(w) {
		return
	}
	list, err := projects.ListProjects(s.deps.ProjectsDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	counts := s.countSessionsByProject()
	out := make([]*projects.ProjectAPI, 0, len(list))
	for _, p := range list {
		api := p.ToAPI()
		api.SessionCount = counts[p.ID]
		out = append(out, api)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"projects": out})
}

// handleGetProject returns a single project by id.
func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	if !s.projectsReady(w) {
		return
	}
	id := r.PathValue("id")
	if err := projects.ValidateProjectID(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := projects.LoadProject(s.deps.ProjectsDir, id)
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("project %q not found", id))
		return
	}
	api := p.ToAPI()
	api.SessionCount = s.countSessionsForProject(id)
	writeJSON(w, http.StatusOK, api)
}

// handleCreateProject creates a new project.
func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	if !s.projectsReady(w) {
		return
	}
	var req projects.ProjectCreateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := projects.CreateProject(s.deps.ProjectsDir, &req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.auditHTTPOp("POST", "/projects", p.ID+" ("+p.Name+")")
	writeJSON(w, http.StatusCreated, p.ToAPI())
}

// handleUpdateProject applies a partial update to a project.
func (s *Server) handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	if !s.projectsReady(w) {
		return
	}
	id := r.PathValue("id")
	if err := projects.ValidateProjectID(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var req projects.ProjectUpdateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	p, err := projects.UpdateProject(s.deps.ProjectsDir, id, &req)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("project %q not found", id))
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.auditHTTPOp("PUT", "/projects/"+id, "updated")
	writeJSON(w, http.StatusOK, p.ToAPI())
}

// handleDeleteProject removes a project directory. Requires ?confirm=true.
// Sessions that referenced the project keep their now-dangling project_id and
// render as unfiled.
func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	if !s.projectsReady(w) {
		return
	}
	id := r.PathValue("id")
	if err := projects.ValidateProjectID(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if r.URL.Query().Get("confirm") != "true" {
		writeError(w, http.StatusBadRequest, "deletion requires ?confirm=true")
		return
	}
	if _, err := projects.LoadProject(s.deps.ProjectsDir, id); err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("project %q not found", id))
		return
	}
	// Destructive delete (ChatGPT/Claude-style): permanently delete every
	// session filed under this project along with the project itself. Done
	// BEFORE removing the dir so a mid-delete crash never leaves sessions
	// pointing at a half-removed project. The renderer gates this behind a
	// strong confirmation warning.
	deleted := s.deleteSessionsOfProject(id)
	if err := projects.DeleteProjectDir(s.deps.ProjectsDir, id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.auditHTTPOp("DELETE", "/projects/"+id, fmt.Sprintf("deleted (+%d sessions)", deleted))
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
