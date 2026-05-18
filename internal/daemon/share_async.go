package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
	"github.com/Kocoro-lab/ShanClaw/internal/share"
	"github.com/Kocoro-lab/ShanClaw/internal/uploads"
)

// shareTaskTimeout caps the total wall-clock time for one async share. 180s
// is the headline UX promise — well above the typical 5–30s render+upload
// path, but tight enough that a CDN/S3 sync stall surfaces as a clear
// "failed" event in under three minutes instead of the 600s the global
// GatewayClient.HTTPClient timeout would otherwise impose.
const shareTaskTimeout = 180 * time.Second

// shareTaskRetainAfterDone keeps a terminal task's snapshot reachable via
// GET /sessions/{id}/share/tasks/{task_id} so a UI that missed the final SSE
// event (network blip, page reload) can poll once to recover the url/upload_id.
// 5 minutes is comfortable — long enough for a slow user to reload, short
// enough that the map can't grow unboundedly under share-spamming.
const shareTaskRetainAfterDone = 5 * time.Minute

// shareUploadHTTPTimeout overrides the GatewayClient's 600s default for the
// async share path only. Same rationale as shareTaskTimeout — fast failure
// beats a 10-minute hang. Other uploads paths (image generation, file upload
// tools) keep the longer default because their bodies and S3 sync windows are
// not bounded the same way.
const shareUploadHTTPTimeout = 180 * time.Second

// Async share lifecycle phases. Linear and never repeat; "completed",
// "failed", and "cancelled" are terminal. Match EventShareProgress payload.
const (
	ShareTaskPhaseAccepted  = "accepted"
	ShareTaskPhaseRendering = "rendering"
	ShareTaskPhaseUploading = "uploading"
	ShareTaskPhaseListing   = "listing"
	ShareTaskPhaseCompleted = "completed"
	ShareTaskPhaseFailed    = "failed"
	ShareTaskPhaseCancelled = "cancelled"
)

// shareTaskState is the daemon-side snapshot of one async share. JSON-shaped
// for direct serialization out the GET /share/tasks/{task_id} endpoint —
// fields match the EventShareProgress payload so a UI handling either source
// can use a single decoder.
//
// Field mutability:
//   - TaskID, SessionID, Agent, CreatedAt are set ONCE in createShareTask and
//     never mutated afterwards. Reading them without the lock is safe because
//     createShareTask's Lock+Unlock + the subsequent goroutine spawn establish
//     a happens-before edge to any reader past that point.
//   - Phase, Message, URL, UploadID, Error, UpdatedAt are mutated by
//     runShareTask via updateShareTask. ALL reads of these fields MUST go
//     through getShareTask (which returns a defensive copy) or the snapshot
//     emitted in updateShareTask. Bare `task.Phase` reads race with the
//     goroutine — race detector caught one at the handler's 202 response.
type shareTaskState struct {
	TaskID    string    `json:"task_id"`
	SessionID string    `json:"session_id"`
	Agent     string    `json:"agent,omitempty"`
	Phase     string    `json:"phase"`
	Message   string    `json:"message,omitempty"`
	URL       string    `json:"url,omitempty"`
	UploadID  string    `json:"upload_id,omitempty"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// createShareTask allocates and registers a fresh task in the accepted phase.
// Emits the first share_progress event so a /events subscriber sees the new
// task immediately (before runShareTask has a chance to advance the phase).
func (s *Server) createShareTask(sessionID, agentName string) *shareTaskState {
	now := time.Now().UTC()
	task := &shareTaskState{
		TaskID:    newShareTaskID(),
		SessionID: sessionID,
		Agent:     agentName,
		Phase:     ShareTaskPhaseAccepted,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.shareTasksMu.Lock()
	if s.shareTasks == nil {
		s.shareTasks = make(map[string]*shareTaskState)
	}
	s.shareTasks[task.TaskID] = task
	s.shareTasksMu.Unlock()

	s.emitShareProgress(task)
	return task
}

// getShareTask returns a defensive copy so callers can serialize without
// holding the lock. Returns nil when the task has been GC'd or never existed.
func (s *Server) getShareTask(taskID string) *shareTaskState {
	s.shareTasksMu.RLock()
	defer s.shareTasksMu.RUnlock()
	t, ok := s.shareTasks[taskID]
	if !ok {
		return nil
	}
	cp := *t
	return &cp
}

// updateShareTask applies mutate under the table lock, then emits the
// progress event AFTER releasing the lock. Emitting under the lock would
// block on a slow SSE subscriber's channel send (capacity 64 but back-pressure
// is real under load) and stall every other share/task call site that needs
// the same lock.
func (s *Server) updateShareTask(task *shareTaskState, mutate func(*shareTaskState)) {
	s.shareTasksMu.Lock()
	mutate(task)
	task.UpdatedAt = time.Now().UTC()
	// Snapshot for emit outside the lock.
	snap := *task
	s.shareTasksMu.Unlock()
	s.emitShareProgress(&snap)
}

// emitShareProgress publishes one share_progress event derived from the task
// snapshot. Always uses s.eventBus directly (the SSE-attached bus); the
// dependency-injected deps.EventBus is wired to the same instance in Server
// construction but we go through the field to keep this independent of that
// indirection.
func (s *Server) emitShareProgress(task *shareTaskState) {
	if s == nil || s.eventBus == nil {
		return
	}
	payload := map[string]any{
		"task_id":    task.TaskID,
		"session_id": task.SessionID,
		"phase":      task.Phase,
	}
	if task.Agent != "" {
		payload["agent"] = task.Agent
	}
	if task.Message != "" {
		payload["message"] = task.Message
	}
	if task.URL != "" {
		payload["url"] = task.URL
	}
	if task.UploadID != "" {
		payload["upload_id"] = task.UploadID
	}
	if task.Error != "" {
		payload["error"] = task.Error
	}
	emitBusJSON(s.eventBus, EventShareProgress, payload)
}

// failShareTask transitions the task into a terminal failed/cancelled phase.
// Distinguishes context.Canceled (daemon shutdown / explicit cancel) from
// substantive errors (upload 500, render bug) so the UI can show a "cancelled"
// vs. "retry" affordance.
func (s *Server) failShareTask(task *shareTaskState, err error) {
	phase := ShareTaskPhaseFailed
	// context.DeadlineExceeded is "180s share-task timeout" — surface as failed.
	// Only context.Canceled (daemon Stop, future explicit cancel endpoint)
	// becomes "cancelled" so the UX distinction stays meaningful.
	if errors.Is(err, context.Canceled) {
		phase = ShareTaskPhaseCancelled
	}
	s.updateShareTask(task, func(t *shareTaskState) {
		t.Phase = phase
		t.Error = err.Error()
	})
}

// scheduleShareTaskGC drops the task from the in-memory map shareTaskRetainAfterDone
// after the goroutine finishes. Time.AfterFunc fires on a separate goroutine
// (no leak risk if the daemon stops earlier — the timer becomes a no-op once
// the map mutex is the only state left).
func (s *Server) scheduleShareTaskGC(taskID string) {
	time.AfterFunc(shareTaskRetainAfterDone, func() {
		s.shareTasksMu.Lock()
		delete(s.shareTasks, taskID)
		s.shareTasksMu.Unlock()
	})
}

// runShareTask drives one async share through render → upload → list →
// PublishedShares write, emitting EventShareProgress at each phase. Runs in a
// dedicated goroutine spawned by handleSessionShare's async branch; the
// parent HTTP request has long since returned 202.
//
// ctx is rooted at s.ctx with a shareTaskTimeout deadline applied at the
// caller — so daemon shutdown OR the 180s ceiling cancels the goroutine.
func (s *Server) runShareTask(
	ctx context.Context,
	task *shareTaskState,
	sess *session.Session,
	agentName string,
	cfg *config.Config,
) {
	// Panic recovery: a bug in share.Render / mgr.PatchPublishedShares /
	// any sanitizer regex below would otherwise kill the goroutine silently,
	// leaving the task stuck mid-phase until the 5min GC fires. UI clients
	// waiting on share_progress would never get a terminal event. Surface
	// it as a failed phase + log so operators can diagnose.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("daemon: share task %s panicked: %v", task.TaskID, r)
			s.updateShareTask(task, func(t *shareTaskState) {
				t.Phase = ShareTaskPhaseFailed
				t.Error = fmt.Sprintf("internal error: %v", r)
			})
		}
	}()
	defer s.scheduleShareTaskGC(task.TaskID)

	// Phase: rendering — sanitize + Haiku summary + Haiku slug + HTML build.
	s.updateShareTask(task, func(t *shareTaskState) {
		t.Phase = ShareTaskPhaseRendering
		t.Message = "rendering HTML and generating summary"
	})

	home, _ := os.UserHomeDir()
	result, err := share.Render(ctx, s.deps.GW, sess, share.Options{HomeDir: home})
	if err != nil {
		s.failShareTask(task, fmt.Errorf("render share: %w", err))
		return
	}
	if len(result.HTML) > maxShareHTMLBytes {
		s.failShareTask(task, fmt.Errorf(
			"rendered HTML is %d bytes, exceeds the %d-byte share limit (too many or too-large images in this session)",
			len(result.HTML), maxShareHTMLBytes))
		return
	}

	filename := buildShareFilename(result.Slug, sess.Title, sess.ID, time.Now().UTC())

	// Phase: uploading.
	s.updateShareTask(task, func(t *shareTaskState) {
		t.Phase = ShareTaskPhaseUploading
		t.Message = "uploading to CDN"
	})

	// Tight 180s HTTP client for the share path only — global GatewayClient
	// keeps its 600s default for other callers. Pair the per-request timeout
	// with the goroutine-scoped ctx so whichever fires first stops the work.
	tightClient := &http.Client{Timeout: shareUploadHTTPTimeout}
	uploadsClient := uploads.NewClient(cfg.Endpoint, cfg.APIKey, tightClient)

	htmlBytes := result.HTML
	openBody := func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(htmlBytes)), nil
	}

	upload, err := uploadsClient.Upload(ctx, openBody, uploads.UploadOptions{
		Filename:    filename,
		ContentType: "text/html",
		Kind:        uploads.KindSessionShare,
		Metadata:    buildShareUploadMetadata(sess.ID, agentName),
	})
	if err != nil {
		s.failShareTask(task, fmt.Errorf("upload: %w", err))
		return
	}

	// Phase: listing — resolve upload_id by URL match against the most recent
	// LIST page. Non-fatal: a missing upload_id still leaves a working URL.
	// Same kind-filtered lookup as the sync path so concurrent landing-page /
	// image uploads don't shove the row we just POSTed off the first page.
	s.updateShareTask(task, func(t *shareTaskState) {
		t.Phase = ShareTaskPhaseListing
		t.Message = "resolving upload id"
	})

	uploadID := ""
	if listResp, lerr := uploadsClient.List(ctx, uploads.ListOptions{Limit: shareLookupListLimit, Kind: uploads.KindSessionShare}); lerr == nil {
		for _, entry := range listResp.Uploads {
			if entry.URL == upload.URL {
				uploadID = entry.ID
				break
			}
		}
		if uploadID == "" {
			log.Printf("daemon: share task %s uploaded (%s, agent=%q) but LIST returned %d entries — none matched URL %s; retract will be impossible without UI fallback",
				task.TaskID, upload.Key, agentName, len(listResp.Uploads), upload.URL)
		}
	} else {
		log.Printf("daemon: share task %s uploaded (%s, agent=%q) but list lookup failed: %v",
			task.TaskID, upload.Key, agentName, lerr)
	}

	// Persist daemon SoT (mirrors the sync handler).
	if uploadID != "" {
		mgr := s.deps.SessionCache.GetOrCreateManager(s.deps.SessionCache.SessionsDir(agentName))
		entry := session.PublishedShareEntry{
			UploadID:  uploadID,
			URL:       upload.URL,
			Filename:  filename,
			CreatedAt: time.Now().UTC(),
		}
		if perr := mgr.PatchPublishedShares(sess.ID, func(cur []session.PublishedShareEntry) []session.PublishedShareEntry {
			return append(cur, entry)
		}); perr != nil {
			log.Printf("daemon: share task %s (agent=%q) persisted cloud-side but failed to write PublishedShares: %v",
				task.TaskID, agentName, perr)
		}
	}

	s.auditHTTPOp("POST", "/sessions/"+sess.ID+"/share",
		fmt.Sprintf("shared session as %s (async task=%s, agent=%q, %d bytes, upload_id=%s, summary_fallback=%t)",
			filename, task.TaskID, agentName, upload.Size, uploadID, result.SummaryFallback))

	// Phase: completed.
	s.updateShareTask(task, func(t *shareTaskState) {
		t.Phase = ShareTaskPhaseCompleted
		t.URL = upload.URL
		t.UploadID = uploadID
		t.Message = ""
	})
}

// newShareTaskID returns a "share-" + 16-hex-char identifier. 64 bits of
// entropy makes collisions a non-concern even under share spam; the prefix
// keeps it identifiable in logs and audit rows.
func newShareTaskID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Fallback: timestamp suffix. Unique-enough in single-process daemon
		// even without entropy; only triggers when /dev/urandom is broken.
		return fmt.Sprintf("share-%d", time.Now().UnixNano())
	}
	return "share-" + hex.EncodeToString(b)
}
