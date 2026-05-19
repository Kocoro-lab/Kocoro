package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agenttypes"
)

const maxQueueRequestBodyBytes = 1 << 20

// handleEnqueueQueue serves POST /queue. Lets HTTP clients (Desktop, CLI
// tooling, tests) push a message into a route's mailbox directly, bypassing
// the legacy /message + InjectMessage channel.
//
// This is the durability-first enqueue path: SQLite append must succeed
// before the in-memory mailbox is touched. The response carries the
// outcome string so callers can decide whether to retry (queue_full,
// persist_failed) or treat as success (queued, deduped).
//
// Phase 4 (attachments): the body's `attachments` array is round-tripped
// through QueuedMessage.Attachments. The runner's drain logic surfaces a
// summary line into the user-turn prompt when attachments are present;
// full RequestContentBlock integration with image compression / file_ref
// / auto-approval is deferred to a follow-up that wires attachments into
// the agent loop's content assembly directly.
func (s *Server) handleEnqueueQueue(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.SessionCache == nil {
		writeError(w, http.StatusServiceUnavailable, "session cache unavailable")
		return
	}

	var req struct {
		RouteKey    string                        `json:"route_key"`
		SessionID   string                        `json:"session_id,omitempty"`
		Text        string                        `json:"text"`
		Source      string                        `json:"source,omitempty"`
		CWD         string                        `json:"cwd,omitempty"`
		Mode        string                        `json:"mode,omitempty"`
		Editable    *bool                         `json:"editable,omitempty"`
		Priority    *int                          `json:"priority,omitempty"`
		Attachments []agenttypes.QueuedAttachment `json:"attachments,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxQueueRequestBodyBytes)).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "queue payload exceeds 1 MB cap")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.RouteKey == "" && req.SessionID != "" {
		req.RouteKey = s.deps.SessionCache.RouteKeyForSession(req.SessionID)
		if req.RouteKey == "" {
			// Default-agent / Desktop path: the active run was started without
			// an explicit route_key (handleMessage doesn't synthesize one) so
			// RouteKeyForSession's scan misses. Fall back to a synthetic
			// "session:<id>" key — EnqueueMessage will create a mailbox under
			// this key, and the runner path that resumed the session will
			// also discover and drain it on its next iteration boundary
			// (see runner.go drain block: it uses req.RouteKey which we
			// likewise default to "session:<id>" for default-agent runs).
			req.RouteKey = "session:" + req.SessionID
		}
	}
	if req.RouteKey == "" {
		writeError(w, http.StatusBadRequest, "route_key or session_id required")
		return
	}
	if req.Text == "" && len(req.Attachments) == 0 {
		writeError(w, http.StatusBadRequest, "text or attachments required")
		return
	}

	// Per-message text cap in addition to the wire body cap above, so a
	// compressed/compact request still cannot store an oversized prompt.
	if approxSize := len(req.Text); approxSize > 1<<20 {
		writeError(w, http.StatusRequestEntityTooLarge, "message text exceeds 1 MB cap")
		return
	}

	editable := true
	if req.Editable != nil {
		editable = *req.Editable
	}
	source := req.Source
	if source == "" {
		source = "http"
	}
	priority := agenttypes.PriorityNext
	if req.Priority != nil {
		priority = agenttypes.Priority(*req.Priority)
	}

	msg := agenttypes.QueuedMessage{
		ID:          newQueueID(),
		RouteKey:    req.RouteKey,
		SessionID:   req.SessionID,
		Source:      source,
		CWD:         req.CWD,
		Mode:        req.Mode,
		Text:        req.Text,
		Attachments: req.Attachments,
		Priority:    priority,
		EnqueuedAt:  time.Now().UTC(),
		Editable:    editable,
	}
	outcome, err := s.deps.SessionCache.EnqueueMessage(req.RouteKey, msg)
	switch {
	case errors.Is(err, agenttypes.ErrMailboxFull):
		writeError(w, http.StatusServiceUnavailable, "mailbox capacity reached for this route")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	switch outcome {
	case MailboxQueued:
		s.publishQueueEvent(EventQueueAdded, req.RouteKey, map[string]any{
			"route_key":  req.RouteKey,
			"message_id": msg.ID,
			"snapshot":   ToDTOs(s.deps.SessionCache.MailboxSnapshot(req.RouteKey)),
		})
		writeJSON(w, http.StatusAccepted, map[string]any{
			"ok":         true,
			"outcome":    outcome.String(),
			"message_id": msg.ID,
		})
	case MailboxDeduped:
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"outcome": outcome.String(),
		})
	case MailboxQueueFull:
		writeError(w, http.StatusServiceUnavailable, "mailbox capacity reached for this route")
	case MailboxPersistFailed:
		writeError(w, http.StatusInternalServerError, "mailbox persistence failed")
	case MailboxRouteMismatch:
		writeError(w, http.StatusConflict, "active run is using a different working directory")
	default:
		writeError(w, http.StatusInternalServerError, "unknown enqueue outcome")
	}
}

// newQueueID returns a short opaque identifier suitable for mailbox rows.
// We use a 16-byte random hex string instead of ULID to avoid pulling in a
// new dependency; collisions are vanishingly improbable for our scale.
func newQueueID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fall back to a timestamp on the (impossible-in-practice) error
		// path so we never enqueue with an empty ID — empty IDs would
		// collide on the SQLite PRIMARY KEY.
		return time.Now().UTC().Format("20060102T150405.000000000")
	}
	return hex.EncodeToString(b[:])
}
