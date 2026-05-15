package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/share"
	"github.com/Kocoro-lab/ShanClaw/internal/uploads"
)

// shareLookupListLimit caps the number of recent uploads we scan when
// resolving the freshly-uploaded row's UUID. Cloud sorts newest-first, so a
// row we just POSTed is at index 0 in the steady state; the buffer is just
// for the rare case of a concurrent upload arriving between our POST and the
// follow-up LIST (a user driving multiple share clicks within sub-second).
const shareLookupListLimit = 20

// validateShareRequest runs the prologue both share endpoints need: deps
// check, session-id path validation, agent-name validation, and the cloud
// configuration gate. It writes the HTTP error response on failure and
// returns ok=false so the handler can early-return. agentName is whatever the
// `agent` query parameter held (possibly empty for default-agent sessions).
func (s *Server) validateShareRequest(w http.ResponseWriter, r *http.Request) (id, agentName string, ok bool) {
	if !s.requireDeps(w) {
		return "", "", false
	}

	id = strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "session id required")
		return "", "", false
	}
	if id != filepath.Base(id) {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return "", "", false
	}

	agentName = r.URL.Query().Get("agent")
	if agentName != "" {
		if err := agents.ValidateAgentName(agentName); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return "", "", false
		}
	}

	cfg, _, _ := s.deps.Snapshot()
	if cfg == nil || !cfg.Cloud.Enabled || cfg.APIKey == "" || s.deps.GW == nil {
		writeError(w, http.StatusServiceUnavailable,
			"session share requires cloud uploads (need cloud.enabled and api_key)")
		return "", "", false
	}

	return id, agentName, true
}

// handleSessionShare renders the named session as a self-contained HTML page
// (Haiku-generated summary up top, images data-URI inlined, files and
// thinking blocks stripped) and uploads it to the cloud uploads endpoint.
//
// Returns:
//
//	200 {"url": "...", "key": "...", "size": N, "upload_id": "...", "summary_fallback": false}
//
// Where:
//   - url             — public CDN URL the user shares
//   - key             — S3 storage key (for client-side audit, not directly useful)
//   - size            — final HTML size in bytes
//   - upload_id       — UUID for later retraction; may be empty if the
//                       post-upload list lookup failed (rare; retraction
//                       still possible via GET /uploads)
//   - summary_fallback — true when Haiku was unreachable or returned empty
//                        and the page used the session title / first user
//                        message as the summary instead
//
// Status codes mirror handleSessionSummary and writeUploadsError.
func (s *Server) handleSessionShare(w http.ResponseWriter, r *http.Request) {
	id, agentName, ok := s.validateShareRequest(w, r)
	if !ok {
		return
	}
	cfg, _, _ := s.deps.Snapshot()

	mgr := s.deps.SessionCache.GetOrCreateManager(s.deps.SessionCache.SessionsDir(agentName))
	sess, err := mgr.Load(id)
	if err != nil {
		// Store.Load wraps the OS error with %w, so os.IsNotExist (Go 1.12-
		// style, not chain-aware) would miss it; errors.Is unwraps properly.
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("session %q not found", id))
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(sess.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "session has no messages to share")
		return
	}

	// Sanitizer replaces this prefix with "~" in any leaked text path so the
	// share page never advertises the daemon operator's username. Failing to
	// resolve the home dir is non-fatal — the regex-based fallback still
	// catches /Users/*/.shannon/... and /home/*/.shannon/... paths.
	home, _ := os.UserHomeDir()

	result, err := share.Render(r.Context(), s.deps.GW, sess, share.Options{HomeDir: home})
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("render share: %v", err))
		return
	}

	// Defense in depth: the cloud upload endpoint caps at 50 MiB. We pre-check
	// at 45 MiB so the error message points at the right tunable (image count
	// or aggregate image bytes in the session) instead of a generic 413.
	const maxShareHTMLBytes = 45 * 1024 * 1024
	if len(result.HTML) > maxShareHTMLBytes {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("rendered HTML is %d bytes, exceeds the %d-byte share limit (too many or too-large images in this session)",
				len(result.HTML), maxShareHTMLBytes))
		return
	}

	filename := buildShareFilename(sess.Title, sess.ID, time.Now().UTC())

	uploadsClient := uploads.NewClient(cfg.Endpoint, cfg.APIKey, s.deps.GW.HTTPClient())

	// openBody returns a fresh reader on each call so the uploads retry
	// machinery (3 attempts on transient 5xx) can stream the body again.
	htmlBytes := result.HTML
	openBody := func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(htmlBytes)), nil
	}

	upload, err := uploadsClient.Upload(r.Context(), filename, "text/html", openBody)
	if err != nil {
		writeUploadsError(w, err)
		return
	}

	// Resolve the upload's UUID via LIST — cloud's POST response doesn't carry
	// the id, but the row is at or near the top of LIST sorted newest-first.
	// Failure here is non-fatal: we still return the URL/key, and Desktop UI
	// can fall back to a separate GET /uploads lookup to discover the id.
	uploadID := ""
	if listResp, lerr := uploadsClient.List(r.Context(), shareLookupListLimit, 0); lerr == nil {
		for _, entry := range listResp.Uploads {
			if entry.URL == upload.URL {
				uploadID = entry.ID
				break
			}
		}
	} else {
		log.Printf("daemon: share %s uploaded (%s) but list lookup failed: %v", id, upload.Key, lerr)
	}

	s.auditHTTPOp("POST", "/sessions/"+id+"/share",
		fmt.Sprintf("shared session as %s (%d bytes, upload_id=%s, summary_fallback=%t)",
			filename, upload.Size, uploadID, result.SummaryFallback))

	writeJSON(w, http.StatusOK, map[string]any{
		"url":              upload.URL,
		"key":              upload.Key,
		"size":             upload.Size,
		"upload_id":        uploadID,
		"summary_fallback": result.SummaryFallback,
	})
}

// handleSessionShareRetract retracts a previously-shared HTML by upload_id.
// Thin session-context wrapper over uploads.Client.Delete; the only reason
// it exists alongside the generic DELETE /uploads/{id} is to:
//
//   - Give clients a symmetric pair (POST + DELETE on the same path).
//   - Record a session-aware audit row ("session_share retracted") instead of
//     a generic upload-retraction log line.
//
// Returns 200 with cloud's DeleteResponse on success. Idempotent at the cloud
// layer — a second retraction of the same upload_id yields 404 with the
// "already retracted" semantic surfaced by ErrNotFound.
func (s *Server) handleSessionShareRetract(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.validateShareRequest(w, r)
	if !ok {
		return
	}
	uploadID := strings.TrimSpace(r.URL.Query().Get("upload_id"))
	if uploadID == "" {
		writeError(w, http.StatusBadRequest, "upload_id query parameter required")
		return
	}
	cfg, _, _ := s.deps.Snapshot()

	uploadsClient := uploads.NewClient(cfg.Endpoint, cfg.APIKey, s.deps.GW.HTTPClient())
	resp, err := uploadsClient.Delete(r.Context(), uploadID)
	if err != nil {
		writeUploadsError(w, err)
		return
	}

	s.auditHTTPOp("DELETE", "/sessions/"+id+"/share",
		fmt.Sprintf("retracted share upload %s (cdn eviction %ds)", uploadID, resp.CDNEvictionSeconds))

	writeJSON(w, http.StatusOK, resp)
}

// buildShareFilename produces a human-readable filename for the uploaded
// HTML so users browsing the "Published files" panel can identify each
// share by its conversation topic at a glance instead of seeing a wall of
// indistinguishable session-id-stamp.html entries.
//
// Shape:  session-<slug>-<YYYYMMDD-HHMMSS>.html
//
// - session- prefix     — disambiguates from publish_to_web / generate_image
//                         uploads in a mixed listing.
// - slug                — sanitized session.Title; non-ASCII (CJK etc.)
//                         characters preserved verbatim, filesystem-unsafe
//                         chars stripped, whitespace runs collapsed to "-",
//                         length capped at 40 runes. Empty/all-stripped
//                         titles fall back to the session-ID short prefix.
// - timestamp           — keeps repeat shares of the same session unique
//                         and provides a chronological hint.
//
// Total length stays well under typical filesystem / S3 key limits (256B).
func buildShareFilename(title, sessionID string, now time.Time) string {
	slug := slugifyTitleForFilename(title)
	if slug == "" {
		short := sessionID
		if len(short) > 24 {
			short = short[:24]
		}
		slug = short
	}
	return fmt.Sprintf("session-%s-%s.html", slug, now.Format("20060102-150405"))
}

// slugifyTitleForFilename trims a session title down to a single token safe
// for use in a URL path / S3 key. Allows ASCII letters, digits, hyphens,
// underscores, and dots; replaces whitespace with "-"; drops every other
// character including non-ASCII letters (CJK / Cyrillic / Arabic / etc.),
// punctuation, emojis, and control chars.
//
// Why ASCII-only: a CJK title makes a CJK S3 key, which round-trips through
// percent-encoding fine on most paths but breaks on a few:
//   - macOS filesystems normalize to NFD whereas HTTP carries NFC bytes
//   - some HTTP libraries / proxies double-encode or re-canonicalize the path
//   - CloudFront/S3 key lookup is byte-exact after percent-decode, so any
//     normalization mismatch becomes a 404
// ASCII-only filenames sidestep the entire class. Titles with no ASCII
// content (e.g. "现在支持哪些模型") slug to empty, which buildShareFilename
// falls back to a session-ID-based filename for.
//
// Caps at 40 runes so a paragraph-long title doesn't produce a 200-character
// filename.
func slugifyTitleForFilename(title string) string {
	const maxRunes = 40

	var b strings.Builder
	prevDash := false
	for _, r := range strings.TrimSpace(title) {
		switch {
		case unicode.IsSpace(r):
			if !prevDash && b.Len() > 0 {
				b.WriteRune('-')
				prevDash = true
			}
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
			prevDash = (r == '-')
		case r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
			prevDash = false
		default:
			// Strip non-ASCII letters, filesystem-unsafe punctuation, emojis,
			// control chars — all collapse silently.
		}
	}
	s := strings.Trim(b.String(), "-_.")

	// Truncate to maxRunes runes, then strip any trailing dash/underscore/dot
	// the cut may have orphaned. (After ASCII-only filtering len(rune)==len(byte),
	// but the rune slice is still correct.)
	r := []rune(s)
	if len(r) > maxRunes {
		s = string(r[:maxRunes])
		s = strings.TrimRight(s, "-_.")
	}
	return s
}
