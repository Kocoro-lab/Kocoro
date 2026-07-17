package daemon

import (
	"net/http"
	"strings"
)

const (
	minKoeLANTokenLength = 24
	maxKoeLANTokenLength = 1024
)

// handleKoeLANConfigure is the explicit, local-presence-gated product path for
// provisioning the protected koe.lan_* fields. Generic PATCH /config keeps
// rejecting these fields: exposing the daemon beyond loopback and changing its
// bearer secret must never happen through an ordinary config editor.
//
// The endpoint is deliberately loopback-only even when the daemon is already
// listening on the LAN. Kocoro Desktop generates the bearer, calls this route
// with its per-launch local-presence token, then restarts the daemon. The bearer
// is never returned or logged.
func (s *Server) handleKoeLANConfigure(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackRemoteAddr(r.RemoteAddr) || !localPresenceAuthorized(r) {
		writeErrorCode(w, http.StatusForbidden, "local_presence_required", "local presence confirmation required")
		return
	}
	if s.deps == nil || strings.TrimSpace(s.deps.ShannonDir) == "" {
		writeErrorCode(w, http.StatusServiceUnavailable, "config_unavailable", "daemon configuration is unavailable")
		return
	}

	var request struct {
		Token  string `json:"token"`
		UnitID string `json:"unit_id"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	token := strings.TrimSpace(request.Token)
	if token != request.Token || len(token) < minKoeLANTokenLength || len(token) > maxKoeLANTokenLength {
		writeErrorCode(w, http.StatusBadRequest, "invalid_token", "token must be a trimmed value between 24 and 1024 characters")
		return
	}
	unitID := strings.TrimSpace(request.UnitID)
	// Without an id this robot's token could only ever be revoked by clobbering
	// the map — which is the failure per-robot slots exist to remove.
	if unitID == "" || unitID != request.UnitID {
		writeErrorCode(w, http.StatusBadRequest, "invalid_unit_id", "unit_id is required")
		return
	}

	if err := s.patchGlobalConfig(map[string]interface{}{
		"koe": map[string]interface{}{
			"lan_bind": true,
			// patchGlobalConfig read-modify-writes the file and deepMerge recurses
			// into nested maps, so this adds one key and leaves every other robot's
			// slot untouched. Reading s.deps.Config instead would miss a robot
			// paired earlier this session and wipe it.
			"lan_tokens": map[string]interface{}{unitID: token},
		},
	}); err != nil {
		writeErrorCode(w, http.StatusInternalServerError, "config_write_failed", "failed to store wireless access configuration")
		return
	}

	s.auditHTTPOp("POST", "/koe/lan/configure", "updated wireless access configuration")
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":           "configured",
		"lan_enabled":      true,
		"restart_required": true,
	})
}

// handleKoeLANRevoke frees one robot's LAN slot. Same loopback + local-presence
// gate as configure: revoking a pairing is as sensitive as granting one.
//
// Idempotent — revoking a robot that was never paired is success, because the
// caller asked for an end state and that end state already holds.
func (s *Server) handleKoeLANRevoke(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackRemoteAddr(r.RemoteAddr) || !localPresenceAuthorized(r) {
		writeErrorCode(w, http.StatusForbidden, "local_presence_required", "local presence confirmation required")
		return
	}
	if s.deps == nil || strings.TrimSpace(s.deps.ShannonDir) == "" {
		writeErrorCode(w, http.StatusServiceUnavailable, "config_unavailable", "daemon configuration is unavailable")
		return
	}

	var request struct {
		UnitID string `json:"unit_id"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	unitID := strings.TrimSpace(request.UnitID)
	if unitID == "" || unitID != request.UnitID {
		writeErrorCode(w, http.StatusBadRequest, "invalid_unit_id", "unit_id is required")
		return
	}

	if err := s.patchGlobalConfig(map[string]interface{}{
		// deepMerge deletes a key whose patch value is nil, so this frees exactly
		// one slot and leaves every other robot paired.
		"koe": map[string]interface{}{"lan_tokens": map[string]interface{}{unitID: nil}},
	}); err != nil {
		writeErrorCode(w, http.StatusInternalServerError, "config_write_failed", "failed to store wireless access configuration")
		return
	}

	s.auditHTTPOp("POST", "/koe/lan/revoke", "revoked wireless access for one robot")
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "revoked"})
}
