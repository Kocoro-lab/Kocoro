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
		Token string `json:"token"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	token := strings.TrimSpace(request.Token)
	if token != request.Token || len(token) < minKoeLANTokenLength || len(token) > maxKoeLANTokenLength {
		writeErrorCode(w, http.StatusBadRequest, "invalid_token", "token must be a trimmed value between 24 and 1024 characters")
		return
	}

	if err := s.patchGlobalConfig(map[string]interface{}{
		"koe": map[string]interface{}{
			"lan_bind":  true,
			"lan_token": token,
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
