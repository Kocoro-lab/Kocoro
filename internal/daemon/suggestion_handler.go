package daemon

import (
	"net/http"
	"path/filepath"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agents"
)

// suggestionResponse is the JSON shape returned to Desktop for both GET and accept.
// Fields tagged `omitempty` are only populated on the /accept path.
type suggestionResponse struct {
	Text               string `json:"text"`
	HasSpeculation     bool   `json:"has_speculation"`
	SuggestedAtUnix    int64  `json:"suggested_at_unix"`
	Suggestion         string `json:"suggestion,omitempty"`          // /accept only — echoes Text
	SpeculatedResponse string `json:"speculated_response,omitempty"` // /accept only
}

// validateSuggestionRoute applies the standard /agents/{name}/sessions/{id}/...
// validation: agent name shape + existence, session id shape + path-traversal
// guard. Returns the validated (name, id, ok). On invalid input it writes the
// appropriate 400 / 404 via writeError / agentExists and returns ok=false.
//
// Pulled out so Task 11.5's extension of handleAcceptSuggestion (which will
// resolve the agent's session manager and call AppendAcceptedSpeculation) can
// trust name+id are filesystem-safe before any disk access.
func (s *Server) validateSuggestionRoute(w http.ResponseWriter, r *http.Request) (name, id string, ok bool) {
	name = r.PathValue("name")
	if err := agents.ValidateAgentName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return "", "", false
	}
	if !s.agentExists(w, name) {
		return "", "", false
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

// handleGetSuggestion returns the latest suggestion for {name}/{id} or 404.
// Wired at GET /agents/{name}/sessions/{id}/suggestion.
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
		HasSpeculation:  cur.SpeculationText != "",
		SuggestedAtUnix: cur.SuggestedAt.Unix(),
	})
}

// handleAcceptSuggestion marks the current suggestion accepted and returns
// the suggestion text + speculated response (if any). Desktop uses the
// speculated response for instant display; runner uses MarkAccepted to skip
// the next main-turn LLM call when the speculated response is served.
// Wired at POST /agents/{name}/sessions/{id}/suggestion/accept.
//
// Note on the Get→MarkAccepted race: a concurrent Clear() (e.g. from a turn
// starting in another goroutine) can drop the entry between our Get and
// MarkAccepted calls. In that case MarkAccepted is a silent no-op, but we
// still serve the previously-fetched suggestion text to the user — they
// clicked accept based on what they saw, which is the correct semantic.
//
// NOTE: this Task 9 version returns SpeculatedResponse to Desktop without
// persisting the user/assistant message pair to session. Task 11.5 will
// extend this handler to atomically write both messages via
// SessionManager.AppendAcceptedSpeculation, with a downgrade-to-no-spec
// fallback if persistence fails. Do not pre-implement that here — it
// requires the AppendAcceptedSpeculation method (added in T11.5).
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

	writeJSON(w, http.StatusOK, suggestionResponse{
		Text:               cur.Text,
		HasSpeculation:     cur.SpeculationText != "",
		SuggestedAtUnix:    cur.SuggestedAt.Unix(),
		Suggestion:         cur.Text,
		SpeculatedResponse: cur.SpeculationText,
	})
}
