package daemon

import (
	"encoding/json"
	"sync"
)

// Event types emitted by the daemon.
const (
	EventMessageReceived = "message_received"
	// EventInjectedCommitted fires when a mid-run injected follow-up is
	// drained into the live turn — the consume boundary. Lets a client flip
	// its queued-draft card into a real user bubble. Delivered on TWO
	// surfaces: the run's per-request SSE stream (payload {message_id, text},
	// owning client only) and, since 2026-06, the broadcast EventBus (payload
	// {route_key, session_id, message_id, text}) so a Desktop mirroring a
	// session whose run belongs to another channel also observes the commit.
	// message_id echoes the client-supplied id.
	EventInjectedCommitted = "injected_committed"
	EventAgentReply        = "agent_reply"
	EventApprovalRequest   = "approval_request"
	EventApprovalResolved  = "approval_resolved"
	EventApprovalNotice    = "approval_notice" // post-decision feedback (e.g. "high-risk pattern: not saved")
	// EventApprovalAuto is emitted when the broker auto-approves a tool that would
	// otherwise have prompted — currently only for non-interactive IM channels
	// (WeChat/WeCom/…) that have no Allow/Deny UI. Because auto-approval bypasses
	// the normal approval_request flow, this notice is the ONLY controller-visible
	// telemetry (replay buffer + Desktop) that an unattended tool ran; without it
	// a channel-driven bash/http call executes with just a local log line. Payload:
	// {session_id, agent, tool, source, channel, reason, ts}.
	EventApprovalAuto = "approval_auto"
	EventAgentError        = "agent_error"
	EventHeartbeatAlert    = "heartbeat_alert"
	EventToolStatus        = "tool_status"
	EventAssistantText     = "assistant_text" // mid-turn agent narration (preamble + state-transition updates); distinct from EventAgentReply (final answer)
	EventUsage             = "usage"          // per-LLM-call usage snapshot for the run
	EventCloudAgent        = "cloud_agent"
	EventCloudProgress     = "cloud_progress"
	EventCloudPlan         = "cloud_plan"
	EventNotification      = "notification"
	EventRunStatus         = "run_status" // watchdog soft/hard events, LLM retries, etc.
	// EventSuggestionReady is emitted by the daemon's post-Run hook after a
	// prompt suggestion has been generated and stored in SuggestionState.
	// Payload: {session_id, agent, text}.
	EventSuggestionReady = "suggestion_ready"

	// EventScheduleRun marks the lifecycle of a scheduled agent run. The
	// payload carries `phase` (started/succeeded/failed) so Desktop can
	// distinguish scheduler-started work from ordinary agent progress events.
	EventScheduleRun = "schedule_run"
	// EventShareProgress reports the lifecycle of a POST /sessions/{id}/share
	// task in async mode. UI clients (Desktop) subscribe to /events and use
	// these to drive the share-button spinner / phase text without polling
	// the task endpoint.
	//
	// Payload shape: {
	//   "task_id":    string,            // UUID from the POST 202 response
	//   "session_id": string,
	//   "agent":      string,            // empty for default agent
	//   "phase":      string,            // see phases below
	//   "message":    string,            // optional human-readable status line
	//   "url":        string (optional), // populated on phase="completed"
	//   "upload_id":  string (optional), // populated on phase="completed"
	//   "error":      string (optional)  // populated on phase="failed"
	// }
	//
	// Phases (linear; never repeats; "completed"/"failed"/"cancelled" are terminal):
	//   accepted     — task accepted, goroutine started
	//   rendering    — sanitizing + RenderHTML (Haiku summary + slug in parallel)
	//   uploading    — POST /api/v1/uploads in flight
	//   listing      — POST returned; resolving upload_id via GET /api/v1/uploads
	//   completed    — done; url + upload_id populated; PublishedShares written
	//   failed       — upload error, render error, or 180s share-task timeout
	//   cancelled    — daemon stopping; goroutine drained
	//
	// Not retained in the notification ring (not a banner-class event) but
	// IS retained in the standard SSE replay ring, so a reconnecting client
	// can still pick up an in-flight share's recent phase events.
	EventShareProgress = "share_progress"

	// EventQueueAdded fires when EnqueueMessage succeeds (SQLite + in-memory
	// both committed). Payload: {route_key, message_id, snapshot:[DTO]}.
	EventQueueAdded = "queue.added"

	// EventQueueRemoved fires when a queued message was retracted by the
	// caller (DELETE /queue/{id}) or skipped by the drain idempotency check.
	// Payload: {route_key, message_id, snapshot:[DTO]}.
	EventQueueRemoved = "queue.removed"

	// EventQueueFlushed fires when the runner drained one or more mailbox
	// items into a user turn. Payload:
	// {route_key, consumed_ids:[string], snapshot:[DTO]}.
	EventQueueFlushed = "queue.flushed"

	// EventCancelRestored fires when POST /cancel with restore_last=true
	// successfully sliced the most recent user message off the session.
	// Payload: {route_key, text, attachments:[…]}. UI clients fill `text`
	// back into their input box.
	EventCancelRestored = "cancel.restored"

	// EventSessionTitleUpdated fires when the async smart-title upgrade
	// (fireTitleAfterRun) persists a new title for a session — including
	// background scheduler runs the user's open window did not trigger.
	// Payload: {session_id, title}. UI clients (Desktop) refresh the matching
	// row in their session list so the title stops lagging the placeholder
	// until the next manual GET /sessions.
	EventSessionTitleUpdated = "session_title_updated"

	// EventDeliverable fires when the present_deliverable tool surfaces a
	// finished artifact (PDF/PPTX/XLSX/DOCX/HTML/image/…) the agent produced.
	// Payload: {session_id, agent, source, id, path, filename, title, mime,
	// byte_size, ts}. The path is daemon-validated as a real regular local file
	// before emission, but it may live outside the session working directory.
	// Clients may trust the metadata came from the daemon, but must not treat it
	// as sandbox authorization or proof of authorship. Desktop renders it as a
	// card in the session's Deliverables sidebar. The call also persists as the
	// present_deliverable tool_use/tool_result pair, so the deliverable
	// re-surfaces when the session is reopened.
	EventDeliverable = "deliverable"
)

// Event is a daemon lifecycle event pushed to SSE subscribers.
type Event struct {
	ID      uint64          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

const ringSize = 512

// notifRingSize caps the dedicated notification history buffer. 500 covers
// roughly a week of normal use (notify tool + approvals + alerts) at the
// rates we see today; raise via Server-level override if it binds.
const notifRingSize = 500

// isNotificationType reports whether an event type should be retained in the
// notification history buffer. These are the events that surface as macOS
// banner notifications in Kocoro Desktop.
func isNotificationType(t string) bool {
	switch t {
	case EventNotification, EventApprovalRequest, EventHeartbeatAlert, EventAgentError:
		return true
	}
	return false
}

// EventBus is a simple pub/sub bus for daemon events.
// It maintains a ring buffer of the last ringSize events so that
// reconnecting clients can replay missed events via EventsSince.
//
// A second, smaller ring (notifRing) retains every notification-class event
// regardless of delivery status, so /notifications can answer "what banners
// did the user receive" even when no SSE subscriber was attached at emit time.
type EventBus struct {
	mu          sync.RWMutex
	subscribers map[<-chan Event]chan Event
	ring        [ringSize]Event
	ringLen     int    // number of valid events in ring (≤ ringSize)
	ringHead    int    // next write position
	nextID      uint64 // monotonically increasing event ID, starts at 1

	notifRing    [notifRingSize]Event
	notifLen     int
	notifHead    int
	notifPersist func(Event) // optional disk-append hook; nil = in-memory only
}

// SetNotifPersister installs a callback invoked under the bus lock for every
// notification-class event. Used by the daemon to append the event to the
// on-disk JSONL log so /notifications survives a restart. nil clears it.
func (b *EventBus) SetNotifPersister(fn func(Event)) {
	b.mu.Lock()
	b.notifPersist = fn
	b.mu.Unlock()
}

// RestoreNotifications seeds the notification ring with previously-persisted
// events (typically loaded from disk at daemon startup) and advances nextID
// past the highest restored ID so newly-emitted events keep monotonic IDs
// across restarts — preserving the /notifications?since=<cursor> contract for
// Desktop clients holding a cursor from before the restart.
func (b *EventBus) RestoreNotifications(events []Event) {
	if len(events) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var maxID uint64
	for _, e := range events {
		if e.ID > maxID {
			maxID = e.ID
		}
		b.notifRing[b.notifHead] = e
		b.notifHead = (b.notifHead + 1) % notifRingSize
		if b.notifLen < notifRingSize {
			b.notifLen++
		}
	}
	if maxID > b.nextID {
		b.nextID = maxID
	}
}

// NewEventBus creates a new EventBus.
func NewEventBus() *EventBus {
	return &EventBus{
		subscribers: make(map[<-chan Event]chan Event),
	}
}

// Subscribe returns a channel that receives all emitted events.
// Caller must call Unsubscribe when done.
func (b *EventBus) Subscribe() <-chan Event {
	ch := make(chan Event, 64)
	b.mu.Lock()
	b.subscribers[ch] = ch
	b.mu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber. No further events will be sent to ch.
// The channel is not closed; callers should stop reading after Unsubscribe.
func (b *EventBus) Unsubscribe(ch <-chan Event) {
	b.mu.Lock()
	delete(b.subscribers, ch)
	b.mu.Unlock()
}

// Emit sends an event to all subscribers. Non-blocking: if a subscriber's
// buffer is full, the event is dropped for that subscriber.
func (b *EventBus) Emit(evt Event) {
	_ = b.EmitTo(evt)
}

// EmitTo sends an event to all subscribers and returns the number of
// subscribers that actually accepted the event (i.e. had buffer space).
// Subscribers whose buffer was full are counted as drops. Callers that need
// to make a real delivery decision — e.g. the notify tool choosing between
// the Desktop path and the osascript fallback — should use this method; a
// zero return value means "nobody got the event, fall back".
//
// Known limitation: EmitTo cannot distinguish a Desktop client from, say, a
// curl session debugging /events. It only reports best-effort delivery to
// any current subscriber. Capability negotiation on the /events endpoint is
// tracked as future work if this becomes a real problem.
func (b *EventBus) EmitTo(evt Event) int {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Assign monotonically increasing ID.
	b.nextID++
	evt.ID = b.nextID

	delivered := 0
	for _, ch := range b.subscribers {
		select {
		case ch <- evt:
			delivered++
		default:
			// subscriber too slow, drop
		}
	}

	// Notification history ring: retain banner-class events so /notifications
	// can serve them after the fact.
	//
	// EventNotification is special-cased to mirror the SSE replay rule below:
	// when no subscriber received the event, the notify tool's caller
	// (runner.go notify handler → tools/notify.go) falls back to osascript and
	// the banner has ALREADY been shown by macOS at this point. Persisting it
	// would let Desktop re-banner the same notification on its next launch.
	// Other notification-class events (approval_request, heartbeat_alert,
	// agent_error) have no osascript fallback path, so we always retain them.
	if isNotificationType(evt.Type) {
		retain := true
		if evt.Type == EventNotification && delivered == 0 {
			retain = false
		}
		if retain {
			b.notifRing[b.notifHead] = evt
			b.notifHead = (b.notifHead + 1) % notifRingSize
			if b.notifLen < notifRingSize {
				b.notifLen++
			}
			if b.notifPersist != nil {
				// Called under b.mu; persister must be fast (file append).
				// Single-instance daemon means no cross-process contention.
				b.notifPersist(evt)
			}
		}
	}

	// Write to SSE replay ring only after delivery attempt. Transient
	// notification-style events that were not delivered (delivered == 0) are
	// excluded so reconnecting clients don't see stale banners for actions
	// that already happened:
	//   - EventNotification: caller (runner.go notify handler) falls back to
	//     osascript when undelivered; replay would duplicate the banner.
	//   - EventApprovalNotice: post-decision feedback ("not saved to config —
	//     high-risk pattern"); the approval decision is one-shot and replaying
	//     the notice on reconnect produces a phantom warning for a resolved call.
	switch evt.Type {
	case EventNotification, EventApprovalNotice:
		if delivered == 0 {
			return delivered
		}
	}
	b.ring[b.ringHead] = evt
	b.ringHead = (b.ringHead + 1) % ringSize
	if b.ringLen < ringSize {
		b.ringLen++
	}

	return delivered
}

// Notifications returns notification-class events with ID > sinceID, oldest
// first. If types is non-empty, only events whose Type is in the set are
// returned. limit caps the result; 0 means no cap.
func (b *EventBus) Notifications(sinceID uint64, types map[string]struct{}, limit int) []Event {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.notifLen == 0 {
		return nil
	}
	out := make([]Event, 0, b.notifLen)
	start := (b.notifHead - b.notifLen + notifRingSize) % notifRingSize
	for i := 0; i < b.notifLen; i++ {
		idx := (start + i) % notifRingSize
		evt := b.notifRing[idx]
		if evt.ID <= sinceID {
			continue
		}
		if len(types) > 0 {
			if _, ok := types[evt.Type]; !ok {
				continue
			}
		}
		out = append(out, evt)
	}
	if limit > 0 && len(out) > limit {
		// Keep most recent when truncating.
		out = out[len(out)-limit:]
	}
	return out
}

// SubscribeWithReplay atomically registers a subscriber and returns all
// events with ID > lastID from the ring buffer. Because both operations
// happen under a single write lock, no events can be emitted between the
// replay snapshot and the subscriber registration — closing the gap that
// would exist if EventsSince and Subscribe were called separately.
func (b *EventBus) SubscribeWithReplay(lastID uint64) ([]Event, <-chan Event) {
	ch := make(chan Event, 64)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subscribers[ch] = ch
	var missed []Event
	if b.ringLen > 0 && lastID < b.nextID {
		start := (b.ringHead - b.ringLen + ringSize) % ringSize
		for i := 0; i < b.ringLen; i++ {
			idx := (start + i) % ringSize
			if b.ring[idx].ID > lastID {
				missed = append(missed, b.ring[idx])
			}
		}
	}
	return missed, ch
}

// EventsSince returns events with ID > lastID from the ring buffer.
// Returns nil if the buffer is empty or the client is already up to date.
func (b *EventBus) EventsSince(lastID uint64) []Event {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.ringLen == 0 || lastID >= b.nextID {
		return nil
	}
	var result []Event
	start := (b.ringHead - b.ringLen + ringSize) % ringSize
	for i := 0; i < b.ringLen; i++ {
		idx := (start + i) % ringSize
		if b.ring[idx].ID > lastID {
			result = append(result, b.ring[idx])
		}
	}
	return result
}

// HasSubscribers reports whether at least one subscriber is currently attached.
// Retained for callers that only need a cheap liveness check. New delivery
// decisions should prefer EmitTo's return value instead.
func (b *EventBus) HasSubscribers() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers) > 0
}
