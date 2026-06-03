package daemon

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/Kocoro-lab/ShanClaw/internal/session"
	"github.com/Kocoro-lab/ShanClaw/internal/share"
	"github.com/Kocoro-lab/ShanClaw/internal/uploads"
)

// shareLookupListLimit caps the number of recent uploads we scan when
// resolving the freshly-uploaded row's UUID. Cloud sorts newest-first, so a
// row we just POSTed is at index 0 in the steady state; the buffer is just
// for the rare case of a concurrent upload arriving between our POST and the
// follow-up LIST (a user driving multiple share clicks within sub-second).
const shareLookupListLimit = 20

// maxShareHTMLBytes pre-checks at 45 MiB so a rejection message points at the
// right tunable (image count / aggregate image bytes) instead of the generic
// 413 the upload endpoint returns at 50 MiB. Shared by both the sync and
// async share paths so the limit is consistent.
const maxShareHTMLBytes = 45 * 1024 * 1024

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
	apiKey := s.liveAPIKey(cfg)
	if cfg == nil || !cfg.Cloud.Enabled || apiKey == "" || s.deps.GW == nil {
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
// Two modes, selected via `?async=` query parameter:
//
//   - async=true (DEFAULT) — Returns 202 Accepted immediately with a
//     {task_id, session_id, phase: "accepted", status: "accepted"} body.
//     The actual render + upload runs on a background goroutine with a 180s
//     ceiling; progress is reported via share_progress events on
//     GET /events and snapshot is readable at
//     GET /sessions/{id}/share/tasks/{task_id}. UI clients should consume
//     the SSE stream to drive their share-button UX.
//
//   - async=false — Returns 200 with the legacy synchronous shape:
//     {"url":..., "key":..., "size":N, "upload_id":..., "summary_fallback":...}.
//     Inherits the GatewayClient's 600s HTTPClient timeout. Kept for
//     scripted clients (curl, CLI tests) that cannot subscribe to SSE.
//
// Status codes (sync path) mirror handleSessionSummary and writeUploadsError.
// Async path can fail-fast with the same 4xx codes (400 invalid id, 404
// session missing, 503 cloud disabled, 413 oversized HTML when a fast
// pre-render check is added in the future) but a per-task failure surfaces
// as phase="failed" on the SSE stream, not an HTTP error.
func (s *Server) handleSessionShare(w http.ResponseWriter, r *http.Request) {
	id, agentName, ok := s.validateShareRequest(w, r)
	if !ok {
		return
	}
	cfg, _, _ := s.deps.Snapshot()
	cfg = s.configWithLiveAPIKey(cfg)

	mgr := s.deps.SessionCache.GetOrCreateManager(s.deps.SessionCache.SessionsDir(agentName))
	sess, err := mgr.Load(id)
	if err != nil {
		// Store.Load returns the not-exist error UNWRAPPED (other errors stay
		// %w-wrapped). errors.Is is the wrap-robust idiom: it matches whether
		// or not the error is wrapped, so this stays correct even if a future
		// Load path re-wraps. Do NOT switch to os.IsNotExist (not chain-aware).
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

	// Per-request `?async` query parameter overrides daemon.share_async_default
	// in either direction. Defaults are picked so a daemon operator can roll
	// back to synchronous shares if the UI hasn't learned share_progress yet
	// (set daemon.share_async_default=false in config.yaml).
	async := cfg.Daemon.ShareAsyncDefault
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("async"))) {
	case "true", "1", "yes":
		async = true
	case "false", "0", "no":
		async = false
	}
	if async {
		task := s.createShareTask(id, agentName)
		// Detach from the HTTP request's context — the client gets a 202 and
		// hangs up, so r.Context() would cancel the goroutine almost
		// immediately. Root at s.ctx so daemon shutdown still drains shares
		// cleanly via context.Canceled.
		parentCtx := s.ctx
		if parentCtx == nil {
			parentCtx = context.Background()
		}
		taskCtx, cancel := context.WithTimeout(parentCtx, shareTaskTimeout)
		go func() {
			defer cancel()
			s.runShareTask(taskCtx, task, sess, agentName, cfg)
		}()
		// Use the phase literal — task.Phase races with the goroutine that
		// just started runShareTask (it transitions to "rendering" before
		// the handler finishes writing the response). The handler's
		// contract is "I accepted this task", so the literal is also the
		// semantically-correct value to return.
		writeJSON(w, http.StatusAccepted, map[string]any{
			"task_id":    task.TaskID,
			"session_id": id,
			"agent":      agentName,
			"phase":      ShareTaskPhaseAccepted,
			"status":     "accepted",
		})
		return
	}

	// Sanitizer replaces this prefix with "~" in any leaked text path so the
	// share page never advertises the daemon operator's username. Failing to
	// resolve the home dir is non-fatal — the regex-based fallback still
	// catches /Users/*/.shannon/... and /home/*/.shannon/... paths.
	home, _ := os.UserHomeDir()

	result, err := share.Render(r.Context(), s.deps.GW, sess, share.Options{
		HomeDir:  home,
		Metadata: shareMetadataFromConfig(cfg),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("render share: %v", err))
		return
	}

	// Defense in depth: the cloud upload endpoint caps at 50 MiB. We pre-check
	// at maxShareHTMLBytes (45 MiB, package-level) so the error message points
	// at the right tunable (image count or aggregate image bytes in the
	// session) instead of a generic 413.
	if len(result.HTML) > maxShareHTMLBytes {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("rendered HTML is %d bytes, exceeds the %d-byte share limit (too many or too-large images in this session)",
				len(result.HTML), maxShareHTMLBytes))
		return
	}

	filename := buildShareFilename(result.Slug, sess.Title, sess.ID, time.Now().UTC())

	uploadsClient := uploads.NewClient(cfg.Endpoint, cfg.APIKey, s.deps.GW.HTTPClient())

	// openBody returns a fresh reader on each call so the uploads retry
	// machinery (3 attempts on transient 5xx) can stream the body again.
	htmlBytes := result.HTML
	openBody := func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(htmlBytes)), nil
	}

	upload, err := uploadsClient.Upload(r.Context(), openBody, uploads.UploadOptions{
		Filename:    filename,
		ContentType: "text/html",
		Kind:        uploads.KindSessionShare,
		Metadata:    buildShareUploadMetadata(sess.ID, agentName),
	})
	if err != nil {
		writeUploadsError(w, err)
		return
	}

	// Resolve the upload's UUID via LIST — cloud's POST response doesn't carry
	// the id, but the row is at or near the top of LIST sorted newest-first.
	// Failure here is non-fatal: we still return the URL/key, and Desktop UI
	// can fall back to a separate GET /uploads lookup to discover the id.
	// Filter by Kind=session_share so a burst of concurrent landing_page /
	// image / other uploads can't shove our row out of the lookup window.
	uploadID := ""
	if listResp, lerr := uploadsClient.List(r.Context(), uploads.ListOptions{Limit: shareLookupListLimit, Kind: uploads.KindSessionShare}); lerr == nil {
		for _, entry := range listResp.Uploads {
			if entry.URL == upload.URL {
				uploadID = entry.ID
				break
			}
		}
		// LIST succeeded but no row matched the URL we just POSTed. This
		// strands the UI: retract needs the upload_id and the daemon can
		// only get it from this LIST window. Log enough context (agent +
		// list size) to diagnose whether the user is hitting paging
		// (LIST limit too small) or a Cloud-side replication delay.
		if uploadID == "" {
			log.Printf("daemon: share %s uploaded (%s, agent=%q) but LIST returned %d entries — none matched URL %s; retract will be impossible without UI fallback",
				id, upload.Key, agentName, len(listResp.Uploads), upload.URL)
		}
	} else {
		log.Printf("daemon: share %s uploaded (%s, agent=%q) but list lookup failed: %v",
			id, upload.Key, agentName, lerr)
	}

	// Persist daemon-side SoT so the UI has a fallback when its own
	// upload_id storage drifts (the suspected root cause of named-agent
	// retract failures). Best-effort: a write failure here does NOT fail
	// the share — the user already has a working URL, and retract via the
	// generic DELETE /uploads/{id} path still works.
	if uploadID != "" {
		entry := session.PublishedShareEntry{
			UploadID:  uploadID,
			URL:       upload.URL,
			Filename:  filename,
			CreatedAt: time.Now().UTC(),
		}
		if perr := mgr.PatchPublishedShares(id, func(cur []session.PublishedShareEntry) []session.PublishedShareEntry {
			return append(cur, entry)
		}); perr != nil {
			log.Printf("daemon: share %s (agent=%q) persisted to cloud but failed to write PublishedShares: %v",
				id, agentName, perr)
		}
	}

	s.auditHTTPOp("POST", "/sessions/"+id+"/share",
		fmt.Sprintf("shared session as %s (agent=%q, %d bytes, upload_id=%s, summary_fallback=%t)",
			filename, agentName, upload.Size, uploadID, result.SummaryFallback))

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
	id, agentName, ok := s.validateShareRequest(w, r)
	if !ok {
		return
	}
	uploadID := strings.TrimSpace(r.URL.Query().Get("upload_id"))
	if uploadID == "" {
		writeError(w, http.StatusBadRequest, "upload_id query parameter required")
		return
	}
	cfg, _, _ := s.deps.Snapshot()
	apiKey := s.liveAPIKey(cfg)

	uploadsClient := uploads.NewClient(cfg.Endpoint, apiKey, s.deps.GW.HTTPClient())
	resp, err := uploadsClient.Delete(r.Context(), uploadID)
	if err != nil {
		writeUploadsError(w, err)
		return
	}

	// Drop the retracted entry from the daemon-side SoT. Best-effort: if
	// the session file isn't reachable we still report success to the user
	// because cloud-side retract already succeeded. The stale entry will be
	// pruned next time the user shares from the same session.
	mgr := s.deps.SessionCache.GetOrCreateManager(s.deps.SessionCache.SessionsDir(agentName))
	if perr := mgr.PatchPublishedShares(id, func(cur []session.PublishedShareEntry) []session.PublishedShareEntry {
		out := cur[:0]
		for _, e := range cur {
			if e.UploadID != uploadID {
				out = append(out, e)
			}
		}
		return out
	}); perr != nil {
		log.Printf("daemon: retract %s (agent=%q, upload_id=%s) succeeded cloud-side but failed to update PublishedShares: %v",
			id, agentName, uploadID, perr)
	}

	s.auditHTTPOp("DELETE", "/sessions/"+id+"/share",
		fmt.Sprintf("retracted share upload %s (agent=%q, cdn eviction %ds)",
			uploadID, agentName, resp.CDNEvictionSeconds))

	writeJSON(w, http.StatusOK, resp)
}

// handleSessionShareTask returns the current snapshot of an async share
// task: the same JSON shape emitted via EventShareProgress (task_id, phase,
// url/upload_id on completion, error on failure). UI clients normally drive
// off the SSE stream — this endpoint is the polling fallback for clients
// that reconnected after missing the terminal event, plus a self-test path
// for scripts.
//
// Returns 200 with the task snapshot when found, 404 when the task expired
// (older than shareTaskRetainAfterDone past its terminal phase) or never
// existed. The session id in the path is validated for shape but is NOT
// cross-checked against the task's recorded session_id — that lookup mismatch
// would be a UI bug, and 404 is the safer response than 200 with a confusing
// task body.
func (s *Server) handleSessionShareTask(w http.ResponseWriter, r *http.Request) {
	if !s.requireDeps(w) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" || id != filepath.Base(id) {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}
	taskID := strings.TrimSpace(r.PathValue("task_id"))
	if taskID == "" {
		writeError(w, http.StatusBadRequest, "task_id required")
		return
	}

	task := s.getShareTask(taskID)
	if task == nil {
		writeError(w, http.StatusNotFound,
			fmt.Sprintf("share task %q not found (may have expired)", taskID))
		return
	}
	// Defense: a malicious caller could probe random task IDs hoping to hit
	// another session's in-flight share. The IDs are 64-bit random so blind
	// probing is hopeless, but match the path's session_id anyway so a
	// confused client gets 404 instead of leaking task metadata.
	if task.SessionID != id {
		writeError(w, http.StatusNotFound,
			fmt.Sprintf("share task %q not found for session %q", taskID, id))
		return
	}

	writeJSON(w, http.StatusOK, task)
}

// handleSessionShares returns the daemon's record of currently-published
// share artifacts for this session. UI clients use this as a fallback when
// their own upload_id storage gets out of sync — they can call this endpoint
// to recover the upload_ids needed for retraction.
//
// Returns 200 with `{"shares": [...]}` (empty array, never null) on success.
// 404 if the session doesn't exist. Agent name is honored via the `?agent=`
// query parameter, matching the share/retract endpoints.
func (s *Server) handleSessionShares(w http.ResponseWriter, r *http.Request) {
	if !s.requireDeps(w) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "session id required")
		return
	}
	if id != filepath.Base(id) {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}
	agentName := r.URL.Query().Get("agent")
	if agentName != "" {
		if err := agents.ValidateAgentName(agentName); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	mgr := s.deps.SessionCache.GetOrCreateManager(s.deps.SessionCache.SessionsDir(agentName))
	sess, err := mgr.Load(id)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("session %q not found", id))
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	shares := sess.PublishedShares
	if shares == nil {
		shares = []session.PublishedShareEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"shares": shares,
	})
}

// buildShareFilename produces a human-readable filename for the uploaded
// HTML so users browsing the "Published files" panel can identify each
// share by its conversation topic at a glance instead of seeing a wall of
// indistinguishable session-id-stamp.html entries.
//
// Shape:  session-<slug>-<YYYYMMDD-HHMMSS>.html
//
// Slug source priority (each falls back when the previous yields empty):
//  1. haikuSlug — Haiku-generated English slug ("debug-payment-bug"),
//     ASCII-only by construction. Best UX for non-English sessions.
//  2. slugifyTitleForFilename(title) — ASCII portion of the session title
//     ("Refactor the loader" → "Refactor-the-loader"). Used when Haiku is
//     unavailable but the title has usable ASCII content.
//  3. sessionID short prefix — last-resort identifier; loses topic info
//     but stays unique and S3-safe.
//
// Total length stays well under typical filesystem / S3 key limits (256B).
func buildShareFilename(haikuSlug, title, sessionID string, now time.Time) string {
	slug := strings.TrimSpace(haikuSlug)
	if slug == "" {
		slug = slugifyTitleForFilename(title)
	}
	if slug == "" {
		short := sessionID
		if len(short) > 24 {
			short = short[:24]
		}
		slug = short
	}
	return fmt.Sprintf("session-%s-%s.html", slug, now.Format("20060102-150405"))
}

// buildShareUploadMetadata returns the JSON payload attached to every session-
// share upload. Cloud stores it on the row so the Desktop UI can cross-reference
// uploads back to their originating session/agent without round-tripping the
// daemon. agentName empty (default agent) omits the key so the JSON stays
// minimal — Cloud's 8 KiB metadata cap is generous but we keep this lean.
//
// json.Marshal failure on a string→string map is unreachable; on the off chance
// the encoder ever fails we fall back to nil, which Upload treats as "omit the
// metadata field entirely" — the share still goes through, just without the
// cross-reference. Better than aborting the share.
func buildShareUploadMetadata(sessionID, agentName string) json.RawMessage {
	md := map[string]string{"session_id": sessionID}
	if agentName != "" {
		md["agent"] = agentName
	}
	raw, err := json.Marshal(md)
	if err != nil {
		return nil
	}
	return raw
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
//
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
