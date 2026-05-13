package daemon

import (
	"net/http"
	"path/filepath"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agents"
)

// suggestionResponse is the JSON shape returned to Desktop for both GET and accept.
// Suggestion is set only on the /accept path so Desktop can echo back the
// exact text the user accepted (useful when the input was filled but not yet
// submitted).
type suggestionResponse struct {
	Text            string `json:"text"`
	SuggestedAtUnix int64  `json:"suggested_at_unix"`
	Suggestion      string `json:"suggestion,omitempty"` // /accept only — echoes Text
}

// validateSuggestionRoute resolves and validates the route inputs for both
// route shapes:
//   - /agents/{name}/sessions/{id}/suggestion[/accept]   — named agent
//   - /sessions/{id}/suggestion[/accept]                 — default agent ("")
//
// When r.PathValue("name") is empty (the default-agent route), the name
// validation and existence check are skipped. When non-empty, both are
// enforced exactly as before. The returned name is "" for default-agent
// callers, and SessionCache.GetOrCreate("") resolves to the default
// sessions directory per router.go:sessionsDir.
//
// Session id is always validated for shape and path-traversal.
func (s *Server) validateSuggestionRoute(w http.ResponseWriter, r *http.Request) (name, id string, ok bool) {
	name = r.PathValue("name")
	if name != "" {
		if err := agents.ValidateAgentName(name); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return "", "", false
		}
		if !s.agentExists(w, name) {
			return "", "", false
		}
	}
	id = r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "session id required")
		return "", "", false
	}
	if id != filepath.Base(id) || strings.ContainsAny(id, `/\`) {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return "", "", false
	}
	return name, id, true
}

// handleGetSuggestion returns the latest suggestion text for {name}/{id} or
// 404 if none. Wired at GET /agents/{name}/sessions/{id}/suggestion and
// GET /sessions/{id}/suggestion (default agent).
func (s *Server) handleGetSuggestion(w http.ResponseWriter, r *http.Request) {
	_, sessionID, ok := s.validateSuggestionRoute(w, r)
	if !ok {
		return
	}
	cur, present := s.suggestions.Get(sessionID)
	if !present || cur.Text == "" {
		writeError(w, http.StatusNotFound, "no suggestion available")
		return
	}
	writeJSON(w, http.StatusOK, suggestionResponse{
		Text:            cur.Text,
		SuggestedAtUnix: cur.SuggestedAt.Unix(),
	})
}

// handleAcceptSuggestion records that the user accepted the suggestion and
// returns the suggestion text so Desktop can fill the input. The user still
// has to press Enter — the normal POST /message flow handles persistence,
// just like any other typed message. Wired at POST .../suggestion/accept.
//
// Note on the Get→MarkAccepted race: a concurrent Clear() (e.g. from a turn
// starting in another goroutine) can drop the entry between our Get and
// MarkAccepted calls. In that case MarkAccepted is a silent no-op, but we
// still serve the previously-fetched suggestion text to the user — they
// clicked accept based on what they saw, which is the correct semantic.
func (s *Server) handleAcceptSuggestion(w http.ResponseWriter, r *http.Request) {
	_, sessionID, ok := s.validateSuggestionRoute(w, r)
	if !ok {
		return
	}
	cur, present := s.suggestions.Get(sessionID)
	if !present || cur.Text == "" {
		writeError(w, http.StatusNotFound, "no suggestion available")
		return
	}

	s.suggestions.MarkAccepted(sessionID)
	s.suggestions.Clear(sessionID) // accepting consumes the suggestion

	writeJSON(w, http.StatusOK, suggestionResponse{
		Text:            cur.Text,
		SuggestedAtUnix: cur.SuggestedAt.Unix(),
		Suggestion:      cur.Text,
	})
}
