package daemon

import (
	"encoding/json"
	"log"
)

// CapIMMessageLifecycleV1 advertises that this daemon supports the IM
// message lifecycle protocol described in
// docs/superpowers/specs/2026-05-19-im-message-reaction-lifecycle-design.md.
//
// Cloud only attaches IMStatusContext when this capability is advertised;
// daemon only emits MESSAGE_LIFECYCLE events when IMStatusContext is present.
const CapIMMessageLifecycleV1 = "im_message_lifecycle_v1"

// Lifecycle event type sent via wsClient.SendEvent.
const EventTypeMessageLifecycle = "MESSAGE_LIFECYCLE"

// Lifecycle states (single source of truth — both daemon emit-sites and Cloud
// renderer dispatch table reference these by string literal).
const (
	LifecycleReceived   = "received"
	LifecycleProcessing = "processing"
	LifecycleDone       = "done"
	LifecycleCleared    = "cleared"
)

// LifecycleEventSender is the narrow interface lifecycle helpers need from
// *daemon.Client. Defined here so tests can substitute a recording fake
// without standing up a real WebSocket server.
type LifecycleEventSender interface {
	SendEvent(messageID, eventType, message string, data map[string]interface{}) error
}

// drainedInflightAppender is the SessionCache surface the run-scoped emitter
// uses to record drained-inflight entries. Decoupled from *SessionCache so
// tests can plug in a recording stub without standing up routes.
type drainedInflightAppender interface {
	AppendDrainedInflight(routeKey string, entry DrainedInflightEntry)
}

// drainedInflightTaker is the SessionCache surface the run-completion emit
// needs. Kept separate from drainedInflightAppender so completion paths
// cannot accidentally append, and so the RunLifecycleEmitter does not have
// to widen its dependency to the take side.
type drainedInflightTaker interface {
	TakeDrainedInflight(routeKey string) []DrainedInflightEntry
}

// RunLifecycleEmitter implements agent.LifecycleEmitter for a single agent
// run. Each instance is bound to one (route, ws client) pair and forwards
// every OnUserMessageProcessing call as a WS MESSAGE_LIFECYCLE "processing"
// event AND appends a DrainedInflightEntry to the route — both happen even
// if WS send fails so Task 9's run-completion sweep can still clean up.
type RunLifecycleEmitter struct {
	ws       LifecycleEventSender
	cache    drainedInflightAppender
	routeKey string
}

// NewRunLifecycleEmitter constructs the per-run emitter. All three params
// may be nil/empty individually; the emitter's call sites guard each access
// so a partially-wired construction (e.g. test environments without a WS
// client) degrades to no-op rather than panicking.
func NewRunLifecycleEmitter(ws LifecycleEventSender, cache drainedInflightAppender, routeKey string) *RunLifecycleEmitter {
	return &RunLifecycleEmitter{ws: ws, cache: cache, routeKey: routeKey}
}

// OnUserMessageProcessing fires the MESSAGE_LIFECYCLE "processing" event for
// the message AND appends the entry to the route's drained-inflight slice.
// Both halves run independently so a transient WS failure does not lose the
// bookkeeping Task 9 needs for "cleared" emission.
//
// Empty cloudMessageID or empty imStatusContext short-circuits — non-IM
// drains arrive with these unset and must not produce wire events.
func (e *RunLifecycleEmitter) OnUserMessageProcessing(cloudMessageID string, imStatusContext json.RawMessage) {
	if e == nil || cloudMessageID == "" || len(imStatusContext) == 0 {
		return
	}
	if e.ws != nil {
		if err := e.ws.SendEvent(cloudMessageID, EventTypeMessageLifecycle, "", map[string]interface{}{
			"state":             LifecycleProcessing,
			"im_status_context": imStatusContext,
		}); err != nil {
			log.Printf("daemon: lifecycle processing emit failed: %v", err)
		}
	}
	if e.cache != nil {
		e.cache.AppendDrainedInflight(e.routeKey, DrainedInflightEntry{
			CloudMessageID:  cloudMessageID,
			IMStatusContext: imStatusContext,
		})
	}
}

// EmitLifecycleOnRunCompletion drains the per-route in-flight list, emits
// MESSAGE_LIFECYCLE "done" for the tail entry and "cleared" for all earlier
// entries, then clears the slice from the route. Idempotent — calling again
// after the slice is taken is a silent no-op.
//
// Argument discipline:
//   - cache or routeKey unset: silent no-op (nothing to drain).
//   - wsClient nil: still calls TakeDrainedInflight so the route's slice is
//     drained even when no emitter is attached. In practice this only happens
//     in test fixtures (no WS client wired) or non-IM runs (slice is already
//     empty because the per-message emit short-circuited).
//
// Called from the runner's per-route completion defer — kept daemon-internal
// rather than threaded through agent.LifecycleEmitter because the runner has
// direct access to WSClient + SessionCache + routeKey and the lifecycle "done"
// emit is a daemon concern (Cloud-facing wire event), not an agent-loop
// turn-boundary concern like the "processing" emit.
func EmitLifecycleOnRunCompletion(wsClient LifecycleEventSender, cache drainedInflightTaker, routeKey string) {
	if cache == nil || routeKey == "" {
		return
	}
	entries := cache.TakeDrainedInflight(routeKey)
	if len(entries) == 0 || wsClient == nil {
		return
	}
	last := len(entries) - 1
	for i, e := range entries {
		state := LifecycleCleared
		if i == last {
			state = LifecycleDone
		}
		if err := wsClient.SendEvent(e.CloudMessageID, EventTypeMessageLifecycle, "", map[string]interface{}{
			"state":             state,
			"im_status_context": e.IMStatusContext,
		}); err != nil {
			log.Printf("daemon: lifecycle %s emit failed for %s: %v", state, e.CloudMessageID, err)
		}
	}
}
