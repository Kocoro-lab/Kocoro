package daemon

import (
	"encoding/json"
	"sync"
)

// Event types emitted by the daemon.
const (
	EventMessageReceived  = "message_received"
	EventAgentReply       = "agent_reply"
	EventApprovalRequest  = "approval_request"
	EventApprovalResolved = "approval_resolved"
	EventAgentError       = "agent_error"
	EventHeartbeatAlert   = "heartbeat_alert"
	EventToolStatus       = "tool_status"
	EventCloudAgent       = "cloud_agent"
	EventCloudProgress    = "cloud_progress"
	EventCloudPlan        = "cloud_plan"
	EventNotification     = "notification"
	EventRunStatus        = "run_status" // watchdog soft/hard events, LLM retries, etc.
)

// Event is a daemon lifecycle event pushed to SSE subscribers.
type Event struct {
	ID      uint64          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

const ringSize = 512

// EventBus is a simple pub/sub bus for daemon events.
// It maintains a ring buffer of the last ringSize events so that
// reconnecting clients can replay missed events via EventsSince.
type EventBus struct {
	mu          sync.RWMutex
	subscribers map[<-chan Event]chan Event
	ring        [ringSize]Event
	ringLen     int    // number of valid events in ring (≤ ringSize)
	ringHead    int    // next write position
	nextID      uint64 // monotonically increasing event ID, starts at 1
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

	// Write to ring buffer only after delivery attempt. Notification events
	// that were not delivered (delivered == 0) are excluded: the caller
	// (runner.go notify handler) falls back to osascript in that case, and
	// replaying the notification on reconnect would produce a duplicate banner.
	if evt.Type != EventNotification || delivered > 0 {
		b.ring[b.ringHead] = evt
		b.ringHead = (b.ringHead + 1) % ringSize
		if b.ringLen < ringSize {
			b.ringLen++
		}
	}

	return delivered
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
