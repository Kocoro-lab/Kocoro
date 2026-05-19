package daemon

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
