package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agenttypes"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// handleRewind serves POST /sessions/{id}/rewind?message_id=<msg>.
// Slices the session at the chosen prior user message and returns the
// captured RestoredMessage so the caller can refill it into a UI input box.
//
// If an active run is bound to this session's route, it is cancelled first
// (with a 5s deadline). The slice happens under routeEntry.mu via
// CancelRouteForRestore — see references/rewind.md.
func (s *Server) handleRewind(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.SessionCache == nil {
		writeError(w, http.StatusServiceUnavailable, "session cache unavailable")
		return
	}
	sessionID := r.PathValue("id")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "session id required")
		return
	}
	messageID := r.URL.Query().Get("message_id")
	if messageID == "" {
		writeError(w, http.StatusBadRequest, "message_id required")
		return
	}

	restored, err := s.rewindToMessage(sessionID, messageID)
	switch {
	case errors.Is(err, errRewindNotFound):
		writeError(w, http.StatusBadRequest, "message_id does not match any user message in session")
		return
	case errors.Is(err, errRewindSessionMissing):
		writeError(w, http.StatusNotFound, "session not found")
		return
	case errors.Is(err, ErrCancelRestoreTimeout):
		writeError(w, http.StatusGatewayTimeout, "active run did not exit within 5s; rewind aborted")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if restored == nil {
		writeError(w, http.StatusBadRequest, "session has no truncatable user message at message_id")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"restored": restored,
	})
}

var (
	errRewindNotFound       = errors.New("rewind: message_id not found in session")
	errRewindSessionMissing = errors.New("rewind: session not found")
)

// rewindToMessage finds the session's route (if any), cancels the active run
// to release entry.mu, loads the session, locates messageID, truncates, and
// saves. Returns the captured RestoredMessage or an error.
func (s *Server) rewindToMessage(sessionID, messageID string) (*session.RestoredMessage, error) {
	// (1) If an active run currently serves this session, cancel + wait for
	// it to exit before mutating session state. CancelRouteForRestore
	// returns (nil, nil) when there's no active route or no last user to
	// slice — both cases are fine; we'll do our own slice below.
	if routeKey := s.deps.SessionCache.RouteKeyForSession(sessionID); routeKey != "" {
		if _, err := s.deps.SessionCache.CancelRouteForRestore(routeKey, agenttypes.ReasonUserCancel, false, 5*time.Second); err != nil {
			return nil, err
		}
	}

	// (2) Resolve the session via whatever manager owns it. Sessions are
	// stored per-agent under ~/.shannon/agents/<agent>/sessions or under
	// the default ~/.shannon/sessions/. We try the default first (most
	// common) and fall through to scanning agent-scoped managers if needed.
	sess, mgr, err := s.resolveSessionForRewind(sessionID)
	if err != nil {
		return nil, err
	}

	idx := sess.FindUserMessageIndex(messageID)
	if idx < 0 {
		return nil, errRewindNotFound
	}
	restored, err := sess.TruncateAt(idx)
	if err != nil {
		return nil, err
	}
	if err := mgr.Save(); err != nil {
		return nil, err
	}
	return restored, nil
}

// resolveSessionForRewind opens whichever Manager owns the session. The
// default-agent sessions dir is checked first because Desktop's primary
// workspace uses it; for named agents we scan known managers via
// SessionsDir("") fallback.
func (s *Server) resolveSessionForRewind(sessionID string) (*session.Session, *session.Manager, error) {
	mgr := s.deps.SessionCache.GetOrCreate("")
	if mgr != nil {
		if sess, err := mgr.Resume(sessionID); err == nil && sess != nil {
			return sess, mgr, nil
		}
	}
	// TODO: extend to scan per-agent managers if a session id wasn't found
	// in the default workspace. For Phase 5 the default workspace covers
	// Desktop's main path. Named-agent rewinds can be added when needed.
	return nil, nil, errRewindSessionMissing
}

// publishRewind emits a structured event for future SSE subscribers. Phase 5
// does not use this directly (the HTTP response carries the data), but
// having the publish helper alongside the handler keeps future
// subscribers easy to wire.
func (s *Server) publishRewind(sessionID string, restored *session.RestoredMessage) {
	if s.deps == nil || s.deps.EventBus == nil || restored == nil {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"session_id":  sessionID,
		"text":        restored.Text,
		"attachments": restored.Attachments,
	})
	if err != nil {
		return
	}
	s.deps.EventBus.Emit(Event{Type: EventCancelRestored, Payload: payload})
}
