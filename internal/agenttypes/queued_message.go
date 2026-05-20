// Package agenttypes holds low-level data types shared by internal/daemon and
// internal/agent. Putting them here breaks what would otherwise be an import
// cycle: daemon already imports agent; agent now needs to read mailbox payloads
// and inspect cancel reasons, which would require importing daemon back.
//
// This package MUST stay free of project-internal imports. Standard library
// only. New consumers should never add a dependency from agenttypes to any
// other internal/* package.
package agenttypes

import "time"

// Priority determines mailbox dequeue order.
// Lower number = higher priority. Within the same priority bucket, dequeue is
// strict FIFO by EnqueuedAt.
type Priority int

const (
	// PriorityNow preempts a running turn. Reserved for SDK-mode streaming
	// where the caller explicitly wants to interrupt; daemon WS path never
	// emits Now.
	PriorityNow Priority = 0

	// PriorityNext is the default for user-initiated input (TUI, Desktop,
	// IM channel). Items with this priority drain at the next turn boundary.
	PriorityNext Priority = 1

	// PriorityLater is for background notifications / channel announcements
	// that should never starve user input.
	PriorityLater Priority = 2
)

// QueuedMessage is one pending user-turn-equivalent payload waiting for a turn
// boundary. The mailbox stores values of this type; JSON-marshaling is used
// for SQLite persistence (see internal/daemon/mailbox_store.go).
//
// Forward-compat: adding new fields is safe (json.Unmarshal tolerates extras).
// Renaming or removing fields breaks recovery of in-flight mailbox rows from
// older daemon versions — coordinate via a migration if you must.
type QueuedMessage struct {
	// ID is a ULID generated at enqueue time. Also serves as the mailbox row
	// primary key and the dedup key when paired with RouteKey.
	ID string `json:"id"`

	// RouteKey identifies which route this belongs to. Filled in by
	// InjectMessage at the daemon layer; producers may leave empty.
	RouteKey string `json:"route_key"`

	// SessionID is the resolved session at enqueue time. May be empty if the
	// route is still resolving the session (recovery path or first message).
	SessionID string `json:"session_id,omitempty"`

	// CWD is the effective working directory at enqueue time. Daemon rejects
	// enqueue when this differs from the active run's CWD — prevents
	// cross-project execution and UI/LLM divergence.
	CWD string `json:"cwd,omitempty"`

	// Source identifies which transport originated the message:
	// "ws" | "http" | "sse" | "tui" | "recovery".
	Source string `json:"source"`

	// CloudMsgID is Cloud's wire-level msg_id, used by mailbox_store for
	// INSERT OR IGNORE dedup against replay-buffer resends. Empty for
	// non-WS sources.
	CloudMsgID string `json:"cloud_msg_id,omitempty"`

	// Text is the user-visible message body. Phase 1 is text-only; Phase 4
	// adds Attachments.
	Text string `json:"text"`

	// Mode is an opaque pass-through hint (e.g. Slack thread_ts) that the
	// daemon does not interpret but stores for round-trip back to Cloud.
	Mode string `json:"mode,omitempty"`

	// Priority controls dequeue order. Defaults to PriorityNext.
	Priority Priority `json:"priority"`

	// EnqueuedAt is wall-clock time at insertion. Within a priority bucket,
	// older EnqueuedAt dequeues first.
	EnqueuedAt time.Time `json:"enqueued_at"`

	// Editable signals whether UI clients may offer a "retract / pop back to
	// input box" affordance. False for Cloud-sourced messages — the user has
	// already sent them in Slack/LINE/etc. and we don't own those channels.
	Editable bool `json:"editable"`

	// Attachments is reserved for Phase 4. In Phase 1 the daemon rejects
	// enqueue when len(Attachments) > 0 AND the route has an active run; the
	// existing "active run + attachments → 409" behavior is preserved.
	Attachments []QueuedAttachment `json:"attachments,omitempty"`
}

// QueuedAttachment is the placeholder shape for Phase 4. Kind is one of
// "image" | "document" | "extracted".
type QueuedAttachment struct {
	Nonce       string `json:"nonce"`
	OriginalURL string `json:"original_url,omitempty"`
	Kind        string `json:"kind"`
}
