// Package desktop_rpc implements the daemon side of the Calendar RPC v1
// protocol — a length-prefixed JSON over Unix domain socket transport between
// the daemon (this process) and Kocoro Desktop (the parent process).
//
// See docs/desktop-calendar-rpc.md (v0.5.1) for the full protocol contract.
// §5.5 of that document is the source of truth for every string constant in
// this file; if you add a method / error code / enum value here, update
// §5.5 in the same PR.
package desktop_rpc

import "encoding/json"

// ProtocolVersion is the wire-protocol version this daemon implements.
// Bumped per spec §5.5.1 versioning rule.
const ProtocolVersion = "1.0.0"

// ProtocolMethods lists every method supported by the protocol version.
// Both responder sides (Daemon and Desktop) must return this exact slice when
// answering `system.capabilities`. See spec §5.5.2.
//
// Ordering is normative — fixture files and Desktop's Swift constant must
// match this order so byte-identical comparison passes in tests.
var ProtocolMethods = []string{
	MethodSystemPing,
	MethodSystemCapabilities,
	MethodCalendarListSources,
	MethodCalendarListEvents,
	MethodCalendarGetEvent,
	MethodCalendarCreateEvent,
	MethodCalendarUpdateEvent,
	MethodCalendarDeleteEvent,
	MethodCalendarCheckPermission,
	MethodCalendarRequestPermission,
}

// Method names (spec §5.5.2). System methods are bidirectional; calendar.*
// flows daemon → desktop only.
const (
	MethodSystemPing                = "system.ping"
	MethodSystemCapabilities        = "system.capabilities"
	MethodCalendarListSources       = "calendar.list_sources"
	MethodCalendarListEvents        = "calendar.list_events"
	MethodCalendarGetEvent          = "calendar.get_event"
	MethodCalendarCreateEvent       = "calendar.create_event"
	MethodCalendarUpdateEvent       = "calendar.update_event"
	MethodCalendarDeleteEvent       = "calendar.delete_event"
	MethodCalendarCheckPermission   = "calendar.check_permission"
	MethodCalendarRequestPermission = "calendar.request_permission"
)

// Frame type identifiers (spec §5.5.3). The cancel frame is reserved for v1.x
// (see §8.2 changelog) and is not produced or accepted by v1 implementations.
const (
	FrameDesktopRPCRequest = "desktop_rpc_request"
	FrameDesktopRPCResult  = "desktop_rpc_result"
	FrameDesktopEvent      = "desktop_event"
	FrameDesktopRPCCancel  = "desktop_rpc_cancel" // v1.x placeholder
)

// Error codes (spec §5.5.4 / §5.3). See spec §5.3 for which side may produce
// each code. `timeout` is produced by either side; the rest are Desktop-only
// except `desktop_disconnected` (Daemon-constructed).
//
// The 8 codes listed below are the complete set per spec §5.5.4. Adding a
// 9th here requires a spec PR (and Desktop side update) first — do not
// silently introduce new codes.
const (
	ErrCodePermissionDenied        = "calendar_permission_denied"
	ErrCodePermissionNotDetermined = "calendar_permission_not_determined"
	ErrCodeNotFound                = "not_found"
	ErrCodeInvalidArgument         = "invalid_argument"
	ErrCodeReadOnlyCalendar        = "read_only_calendar"
	ErrCodeInternal                = "internal_error"
	ErrCodeTimeout                 = "timeout"
	ErrCodeDesktopDisconnected     = "desktop_disconnected"
)

// TCC permission status enum (spec §5.5.5).
const (
	PermissionNotDetermined = "not_determined"
	PermissionRestricted    = "restricted"
	PermissionDenied        = "denied"
	PermissionGranted       = "granted"
	PermissionWriteOnly     = "write_only"
)

// Calendar account type enum (spec §5.5.6).
const (
	AccountTypeICloud       = "icloud"
	AccountTypeGoogle       = "google"
	AccountTypeExchange     = "exchange"
	AccountTypeOutlook      = "outlook"
	AccountTypeLocal        = "local"
	AccountTypeSubscription = "subscription"
	AccountTypeOther        = "other"
)

// Attendee participation status enum (spec §5.5.7).
const (
	AttendeeAccepted    = "accepted"
	AttendeeTentative   = "tentative"
	AttendeeDeclined    = "declined"
	AttendeeNeedsAction = "needs_action"
)

// Event scope enum (spec §5.5.8). update_event must reject ScopeAll
// with invalid_argument; delete_event accepts all three.
const (
	ScopeThis           = "this"
	ScopeThisAndFuture  = "this_and_future"
	ScopeAll            = "all"
)

// Desktop event types (spec §5.5.9).
//
// EventDesktopOffline is daemon-internal: emitted on the EventBus when the
// sock connection drops, never crossing the wire (Desktop cannot announce
// its own offline state).
const (
	EventDesktopOnline              = "desktop_online"
	EventDesktopOffline             = "desktop_offline"
	EventCalendarPermissionChanged  = "calendar_permission_changed"
	EventCalendarDataChanged        = "calendar_data_changed"
)

// Recurrence frequency enum (spec §5.5.10).
const (
	FrequencyDaily   = "daily"
	FrequencyWeekly  = "weekly"
	FrequencyMonthly = "monthly"
	FrequencyYearly  = "yearly"
)

// MaxFrameBodyBytes caps a single JSON frame body. The 4-byte length prefix
// must encode a value ≤ this constant; values outside (0, MaxFrameBodyBytes]
// cause the codec to close the connection. See spec §5.1.
const MaxFrameBodyBytes = 4 * 1024 * 1024

// Frame is the outermost envelope read from / written to the sock. Both sides
// see this exact shape; the inner Payload is decoded based on Type.
type Frame struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// RPCRequest is the payload of a `desktop_rpc_request` frame (spec §5.1).
//
// Direction is bidirectional: daemon → desktop for calendar.* methods,
// desktop → daemon for system.* methods (see spec §4.1.1 reconciliation
// and §5.2 system.capabilities).
type RPCRequest struct {
	RequestID string          `json:"request_id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	TimeoutMs int             `json:"timeout_ms"`
	SessionID string          `json:"session_id,omitempty"`
	Agent     string          `json:"agent,omitempty"`
	Source    string          `json:"source,omitempty"`
	TS        string          `json:"ts"`
}

// RPCResult is the payload of a `desktop_rpc_result` frame (spec §5.1).
// Exactly one of Result or Error is populated, determined by OK.
type RPCResult struct {
	RequestID string          `json:"request_id"`
	OK        bool            `json:"ok"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *RPCError       `json:"error,omitempty"`
}

// RPCError is the structured error body inside a failed RPCResult (spec §5.3).
// Code is one of the ErrCode* constants. Retriable should be true only for
// genuinely transient failures (e.g. EventKit lock contention); permission
// denied / not_found / invalid_argument etc. must always be false.
type RPCError struct {
	Code      string          `json:"code"`
	Message   string          `json:"message"`
	Retriable bool            `json:"retriable"`
	Details   json.RawMessage `json:"details,omitempty"`
}

// DesktopEvent is the payload of a `desktop_event` frame (spec §3.5). Events
// flow desktop → daemon and have no request_id — the daemon EventBus is the
// downstream consumer.
type DesktopEvent struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data,omitempty"`
	TS    string          `json:"ts"`
}

// SystemCapabilitiesResult is the structured result returned by responders of
// the `system.capabilities` method (spec §5.2). Field ordering must match the
// spec example so byte-equal fixture round-trip tests pass.
type SystemCapabilitiesResult struct {
	Version  string   `json:"version"`
	Methods  []string `json:"methods"`
	Platform Platform `json:"platform"`
}

// Platform metadata included in system.capabilities (spec §5.2). When the
// responder is the daemon, AppVersion is the shan binary version string
// (matches `shan --version`); when the responder is Desktop, AppVersion is
// the bundle CFBundleShortVersionString.
type Platform struct {
	OS         string `json:"os"`
	OSVersion  string `json:"os_version"`
	AppVersion string `json:"app_version"`
}

// SystemPingParams is the params object for `system.ping` (spec §5.2).
// Echo is optional; an empty string is valid.
type SystemPingParams struct {
	Echo string `json:"echo,omitempty"`
}

// SystemPingResult is the result object for `system.ping` (spec §5.2).
type SystemPingResult struct {
	Pong       string `json:"pong"`
	ServerTime string `json:"server_time"`
}
