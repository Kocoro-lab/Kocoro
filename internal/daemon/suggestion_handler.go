package daemon

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/audit"
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
// Session id is always validated for shape and path-traversal — required
// because Task 11.5's AppendAcceptedSpeculation resolves a file path
// from {id}.
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
// Round 2 R6 — persist-failure downgrade: when speculation is present we
// MUST persist the (suggestion, speculated_response) pair to the session
// BEFORE returning it to Desktop. Otherwise Desktop renders text that the
// next turn's reloaded session does not contain, drifting context. On
// persist failure we suppress speculated_response in the reply so Desktop
// falls back to a normal POST /messages (extra LLM call, but consistent).
func (s *Server) handleAcceptSuggestion(w http.ResponseWriter, r *http.Request) {
	agentName, sessionID, ok := s.validateSuggestionRoute(w, r)
	if !ok {
		return
	}
	cur, present := s.suggestions.Get(sessionID)
	if !present || cur.Text == "" {
		writeError(w, http.StatusNotFound, "no suggestion available")
		return
	}

	// Round 2 R6: on persist failure, downgrade to "no speculation" so Desktop
	// re-sends via normal POST /messages instead of showing un-persisted text.
	// This costs an extra LLM call BUT keeps session state consistent.
	speculated := ""
	if cur.SpeculationText != "" && s.deps != nil && s.deps.SessionCache != nil {
		sessMgr := s.deps.SessionCache.GetOrCreate(agentName)
		if err := sessMgr.AppendAcceptedSpeculation(sessionID, cur.Text, cur.SpeculationText); err == nil {
			speculated = cur.SpeculationText
		} else if s.deps.Auditor != nil {
			s.deps.Auditor.Log(audit.AuditEntry{
				SessionID:    sessionID,
				Event:        "prompt_suggestion_persist_failed",
				InputSummary: fmt.Sprintf("session=%s err=%v — downgrading to non-speculation response", sessionID, err),
			})
		}
	}

	s.suggestions.MarkAccepted(sessionID)
	s.suggestions.Clear(sessionID) // accepting consumes the suggestion

	writeJSON(w, http.StatusOK, suggestionResponse{
		Text:               cur.Text,
		HasSpeculation:     speculated != "",
		SuggestedAtUnix:    cur.SuggestedAt.Unix(),
		Suggestion:         cur.Text,
		SpeculatedResponse: speculated,
	})
}
