package daemon

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/uploads"
)

// requireCloudUploads is the shared gate for the /uploads handlers: cloud must
// be enabled with a live API key and a gateway client. On failure it writes the
// 503 + config hint and returns ok=false so the caller just returns. Single
// source of truth so list/create/delete can't drift on the gating contract.
func (s *Server) requireCloudUploads(w http.ResponseWriter) (cfg *config.Config, apiKey string, ok bool) {
	cfg, _, _ = s.deps.Snapshot()
	apiKey = s.liveAPIKey(cfg)
	if cfg == nil || !cfg.Cloud.Enabled || apiKey == "" || s.deps.GW == nil {
		writeError(w, http.StatusServiceUnavailable,
			"cloud uploads not configured (need cloud.enabled and api_key)")
		return nil, "", false
	}
	return cfg, apiKey, true
}

// uploadsListLimitMax mirrors Cloud's server-side clamp on GET /api/v1/uploads.
// Defense in depth — cloud also clamps, but failing fast here means a clearer
// 400 instead of silently truncated results when a buggy UI overshoots.
const uploadsListLimitMax = 100

// uploadImageInMemoryBytes is the multipart in-memory threshold for the avatar
// upload handler — parts under this stay in RAM, larger ones spill to a temp
// file. Avatars are small (the UI caps at 2 MiB) so 1 MiB keeps the common path
// fully in memory.
const uploadImageInMemoryBytes int64 = 1 << 20 // 1 MB

// allowedAvatarTypes is the image MIME whitelist for POST /uploads. Mirrors the
// renderer's <input accept> and the macOS picker — png/jpeg/webp only, so a
// stray document or SVG (script-bearing) never reaches the CDN as an avatar.
var allowedAvatarTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
}

// handleListUploads proxies GET /api/v1/uploads with the current user's API
// key. Desktop UI uses this for the "Published Files" management panel.
//
// Query parameters (passed through, with local clamping):
//   - limit  (default 20, max 100)
//   - offset (default 0)
//   - kind   (optional business-purpose filter: session_share / report /
//     landing_page / image / other). Validated against the upload-kind
//     whitelist before forwarding — unknown values return 400 locally
//     rather than burning a round trip on Cloud's CHECK rejection.
//
// Response is the raw cloud JSON: {"uploads": [...], "total_count": N}.
// Error mapping: 401 (api_key missing/invalid), 503 (cloud unreachable), 500
// (other). When cloud.enabled is false or api_key is empty, returns 503 with a
// configuration hint — same gating the cloud-uploaded tools use.
func (s *Server) handleListUploads(w http.ResponseWriter, r *http.Request) {
	if !s.requireDeps(w) {
		return
	}
	cfg, apiKey, ok := s.requireCloudUploads(w)
	if !ok {
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
	kind := strings.TrimSpace(q.Get("kind"))
	if kind != "" && !uploads.IsValidKind(kind) {
		writeError(w, http.StatusBadRequest,
			"invalid kind: allowed values are session_share, report, landing_page, image, other")
		return
	}

	client := uploads.NewClient(cfg.Endpoint, apiKey, s.deps.GW.HTTPClient())
	resp, err := client.List(r.Context(), uploads.ListOptions{Limit: limit, Offset: offset, Kind: kind})
	if err != nil {
		writeUploadsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleCreateUpload proxies a multipart image upload to Cloud's POST
// /api/v1/uploads with the current user's API key, returning the raw cloud JSON
// ({"url": "https://static.kocoro.ai/...", ...}). Desktop uses this for agent
// avatar uploads: the daemon stores avatars only as https URLs, so a local
// image must first become a CDN URL here.
//
// Multipart fields:
//   - file (required): the image bytes.
//   - content_type (optional): MIME override; otherwise the uploaded part's
//     Content-Type header is used.
//
// The MIME must be in allowedAvatarTypes (png/jpeg/webp) — anything else is a
// 400. Body is capped at maxUploadSize (10 MiB); the UI caps at 2 MiB. Kind is
// fixed to "image". Error mapping mirrors the list/delete handlers via
// writeUploadsError; cloud-not-configured returns 503.
func (s *Server) handleCreateUpload(w http.ResponseWriter, r *http.Request) {
	if !s.requireDeps(w) {
		return
	}
	cfg, apiKey, ok := s.requireCloudUploads(w)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(uploadImageInMemoryBytes); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "image too large (maximum 10 MB)")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid multipart form: "+err.Error())
		return
	}

	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing 'file' field in multipart form")
		return
	}
	defer file.Close()

	// Resolve MIME: explicit content_type field wins, else the uploaded part's
	// own Content-Type header. Validate against the avatar whitelist.
	mime := strings.TrimSpace(r.FormValue("content_type"))
	if mime == "" {
		mime = hdr.Header.Get("Content-Type")
	}
	mime = strings.ToLower(strings.TrimSpace(mime))
	if i := strings.IndexByte(mime, ';'); i >= 0 { // strip "; charset=…"
		mime = strings.TrimSpace(mime[:i])
	}
	if !allowedAvatarTypes[mime] {
		writeError(w, http.StatusBadRequest, "unsupported image type (allowed: png, jpeg, webp)")
		return
	}

	// uploads.Client.Upload reissues the body on transient retries, so buffer
	// the bytes (already capped at 10 MiB) and hand back a fresh reader each time.
	buf, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read uploaded file: "+err.Error())
		return
	}
	openBody := func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf)), nil
	}

	client := uploads.NewClient(cfg.Endpoint, apiKey, s.deps.GW.HTTPClient())
	resp, err := client.Upload(r.Context(), openBody, uploads.UploadOptions{
		Kind:        uploads.KindImage,
		Filename:    hdr.Filename,
		ContentType: mime,
	})
	if err != nil {
		writeUploadsError(w, err)
		return
	}
	s.auditHTTPOp("POST", "/uploads", "uploaded image")
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

	cfg, apiKey, ok := s.requireCloudUploads(w)
	if !ok {
		return
	}

	client := uploads.NewClient(cfg.Endpoint, apiKey, s.deps.GW.HTTPClient())
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
	case errors.Is(err, uploads.ErrInvalidKind):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, uploads.ErrBadRequest):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, uploads.ErrTransient):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}
