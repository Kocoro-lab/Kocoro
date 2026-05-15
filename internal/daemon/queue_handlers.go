package daemon

import (
	"encoding/json"
	"net/http"

	"github.com/Kocoro-lab/ShanClaw/internal/agenttypes"
)

// handleGetQueue serves GET /queue?route_key=<key> or ?session_id=<id>.
// Returns the redacted DTO list — never the full QueuedMessage. See
// references/queue.md for the wire contract.
func (s *Server) handleGetQueue(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.SessionCache == nil {
		writeError(w, http.StatusServiceUnavailable, "session cache unavailable")
		return
	}
	routeKey := r.URL.Query().Get("route_key")
	if routeKey == "" {
		if sessionID := r.URL.Query().Get("session_id"); sessionID != "" {
			routeKey = s.routeKeyForSession(sessionID)
		}
	}
	if routeKey == "" {
		writeError(w, http.StatusBadRequest, "route_key or session_id required")
		return
	}
	items := s.deps.SessionCache.MailboxSnapshot(routeKey)
	writeJSON(w, http.StatusOK, map[string]any{
		"route_key": routeKey,
		"items":     ToDTOs(items),
	})
}

// handleDeleteQueueItem serves DELETE /queue/{id}?route_key=<key>.
// Refuses retract of Cloud-sourced (editable:false) messages with 403.
func (s *Server) handleDeleteQueueItem(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.SessionCache == nil {
		writeError(w, http.StatusServiceUnavailable, "session cache unavailable")
		return
	}
	id := r.PathValue("id")
	routeKey := r.URL.Query().Get("route_key")
	if routeKey == "" {
		if sessionID := r.URL.Query().Get("session_id"); sessionID != "" {
			routeKey = s.routeKeyForSession(sessionID)
			if routeKey == "" {
				// Desktop's default-agent path: the ad-hoc route was
				// registered under "session:<id>" (see runner.go ad-hoc
				// branch). Use the same synthetic key so a Desktop
				// retract finds the entry without needing to know the
				// daemon's internal route bookkeeping.
				routeKey = "session:" + sessionID
			}
		}
	}
	if id == "" || routeKey == "" {
		writeError(w, http.StatusBadRequest, "route_key or session_id required")
		return
	}
	snap := s.deps.SessionCache.MailboxSnapshot(routeKey)
	var target *agenttypes.QueuedMessage
	for i := range snap {
		if snap[i].ID == id {
			target = &snap[i]
			break
		}
	}
	if target == nil {
		writeError(w, http.StatusNotFound, "queued message not found")
		return
	}
	if !target.Editable {
		writeError(w, http.StatusForbidden, "queued message is not editable (sent from external channel)")
		return
	}
	if !s.deps.SessionCache.MailboxRetract(routeKey, id) {
		// Race with drain — message vanished between snapshot and retract.
		writeError(w, http.StatusConflict, "queued message was consumed before retract")
		return
	}
	s.publishQueueEvent(EventQueueRemoved, routeKey, map[string]any{
		"route_key":  routeKey,
		"message_id": id,
		"snapshot":   ToDTOs(s.deps.SessionCache.MailboxSnapshot(routeKey)),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// publishQueueEvent emits one of the queue.* SSE events via deps.EventBus.
// Safe to call when bus is nil (tests).
func (s *Server) publishQueueEvent(typ, routeKey string, payload map[string]any) {
	if s.deps == nil || s.deps.EventBus == nil {
		return
	}
	if payload == nil {
		payload = map[string]any{"route_key": routeKey}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	s.deps.EventBus.Emit(Event{Type: typ, Payload: raw})
}

// routeKeyForSession returns the route key currently bound to a session ID,
// or "" if no live route exists for it.
func (s *Server) routeKeyForSession(sessionID string) string {
	if s.deps == nil || s.deps.SessionCache == nil {
		return ""
	}
	return s.deps.SessionCache.RouteKeyForSession(sessionID)
}
