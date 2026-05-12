package daemon

import (
	"encoding/json"
	"net/http"
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

// handleGetSuggestion returns the latest suggestion for {name}/{id} or 404.
// Wired at GET /agents/{name}/sessions/{id}/suggestion.
func (s *Server) handleGetSuggestion(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if sessionID == "" {
		http.Error(w, "missing session id", http.StatusBadRequest)
		return
	}
	cur, ok := s.suggestions.Get(sessionID)
	if !ok || cur.Text == "" {
		http.NotFound(w, r)
		return
	}
	resp := suggestionResponse{
		Text:            cur.Text,
		HasSpeculation:  cur.SpeculationText != "",
		SuggestedAtUnix: cur.SuggestedAt.Unix(),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleAcceptSuggestion marks the current suggestion accepted and returns
// the suggestion text + speculated response (if any). Desktop uses the
// speculated response for instant display; runner uses MarkAccepted to skip
// the next main-turn LLM call when the speculated response is served.
// Wired at POST /agents/{name}/sessions/{id}/suggestion/accept.
//
// NOTE: this Task 9 version returns SpeculatedResponse to Desktop without
// persisting the user/assistant message pair to session. Task 11.5 will
// extend this handler to atomically write both messages via
// SessionManager.AppendAcceptedSpeculation, with a downgrade-to-no-spec
// fallback if persistence fails. Do not pre-implement that here — it
// requires the AppendAcceptedSpeculation method (added in T11.5).
func (s *Server) handleAcceptSuggestion(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if sessionID == "" {
		http.Error(w, "missing session id", http.StatusBadRequest)
		return
	}
	cur, ok := s.suggestions.Get(sessionID)
	if !ok || cur.Text == "" {
		http.NotFound(w, r)
		return
	}
	s.suggestions.MarkAccepted(sessionID)

	resp := suggestionResponse{
		Text:               cur.Text,
		HasSpeculation:     cur.SpeculationText != "",
		SuggestedAtUnix:    cur.SuggestedAt.Unix(),
		Suggestion:         cur.Text,
		SpeculatedResponse: cur.SpeculationText,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
