package daemon

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/uploads"
)

// uploadsListLimitMax mirrors Cloud's server-side clamp on GET /api/v1/uploads.
// Defense in depth — cloud also clamps, but failing fast here means a clearer
// 400 instead of silently truncated results when a buggy UI overshoots.
const uploadsListLimitMax = 100

// handleListUploads proxies GET /api/v1/uploads with the current user's API
// key. Desktop UI uses this for the "Published Files" management panel.
//
// Query parameters (passed through, with local clamping):
//   - limit  (default 20, max 100)
//   - offset (default 0)
//
// Response is the raw cloud JSON: {"uploads": [...], "total_count": N}.
// Error mapping: 401 (api_key missing/invalid), 503 (cloud unreachable), 500
// (other). When cloud.enabled is false or api_key is empty, returns 503 with a
// configuration hint — same gating the cloud-uploaded tools use.
func (s *Server) handleListUploads(w http.ResponseWriter, r *http.Request) {
	if !s.requireDeps(w) {
		return
	}
	cfg, _, _ := s.deps.Snapshot()
	if cfg == nil || !cfg.Cloud.Enabled || cfg.APIKey == "" || s.deps.GW == nil {
		writeError(w, http.StatusServiceUnavailable,
			"cloud uploads not configured (need cloud.enabled and api_key)")
		return
	}

	q := r.URL.Query()
	// parseIntParam returns def for n < 1, so `limit=0` (or negative / missing)
	// falls through to 20 — aligned with cloud's "0 → default 20" contract.
	// Same clause clamps any negative offset to 0; no extra guard needed.
	limit := parseIntParam(q.Get("limit"), 20)
	if limit > uploadsListLimitMax {
		limit = uploadsListLimitMax
	}
	offset := parseIntParam(q.Get("offset"), 0)

	client := uploads.NewClient(cfg.Endpoint, cfg.APIKey, s.deps.GW.HTTPClient())
	resp, err := client.List(r.Context(), limit, offset)
	if err != nil {
		writeUploadsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDeleteUpload proxies DELETE /api/v1/uploads/{id}. Owner-only: cross-
// user attempts return 404 (deliberate cloud behavior, do not try to
// disambiguate). Idempotent — calling twice on the same id returns 200 + 404.
//
// On success, audits the action ("DELETE /uploads/<id> retracted upload").
// The id is a UUID belonging to the current user — not secret material — so
// recording it in the audit summary is acceptable.
func (s *Server) handleDeleteUpload(w http.ResponseWriter, r *http.Request) {
	if !s.requireDeps(w) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	cfg, _, _ := s.deps.Snapshot()
	if cfg == nil || !cfg.Cloud.Enabled || cfg.APIKey == "" || s.deps.GW == nil {
		writeError(w, http.StatusServiceUnavailable,
			"cloud uploads not configured (need cloud.enabled and api_key)")
		return
	}

	client := uploads.NewClient(cfg.Endpoint, cfg.APIKey, s.deps.GW.HTTPClient())
	resp, err := client.Delete(r.Context(), id)
	if err != nil {
		writeUploadsError(w, err)
		return
	}
	s.auditHTTPOp("DELETE", "/uploads/"+id, "retracted upload")
	writeJSON(w, http.StatusOK, resp)
}

// writeUploadsError maps internal/uploads sentinel errors onto HTTP status
// codes for Desktop UI. Single source of truth so list / delete diverge only
// in the 404 path (delete-only).
func writeUploadsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, uploads.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, uploads.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, uploads.ErrEndpointNotFound):
		// Cloud responded but doesn't have this endpoint deployed — surface as
		// 503 so Desktop shows "service unavailable" rather than a misleading
		// 404 (which the UI may interpret as "the file was already retracted").
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, uploads.ErrBadRequest):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, uploads.ErrTransient):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}
