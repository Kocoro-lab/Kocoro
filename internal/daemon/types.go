package daemon

import (
	"encoding/json"
	"time"
)

// Server -> Daemon message types
const (
	MsgTypeConnected                = "connected"
	MsgTypeMessage                  = "message"
	MsgTypeClaimAck                 = "claim_ack"
	MsgTypeSystem                   = "system"
	MsgTypeReplyDeliveryResult      = "reply_delivery_result"
	MsgTypeChannelStateEvent        = "channel_state_event"
	MsgTypeRemoteRequest            = "remote_request"
	MsgTypeRemoteRunRequest         = "remote_run_request"
	MsgTypeRemoteRunCancel          = "remote_run_cancel"
	MsgTypeRemoteApproval           = "remote_approval_response"
	MsgTypePairingCodeResponse      = "remote_pairing_code_response"
	MsgTypeRemotePairingsResponse   = "remote_pairings_response"
	MsgTypeRemoteHostRevokeResponse = "remote_host_revoke_response"
)

// Daemon -> Server message types
const (
	MsgTypeClaim               = "claim"
	MsgTypeReply               = "reply"
	MsgTypeProgress            = "progress"
	MsgTypeDisconnect          = "disconnect"
	MsgTypeEvent               = "event"
	MsgTypeProactive           = "proactive"
	MsgTypeDeliveryAck         = "delivery_ack"
	MsgTypeRemoteResponse      = "remote_response"
	MsgTypeRemoteEvent         = "remote_event"
	MsgTypeRemoteRunEvent      = "remote_run_event"
	MsgTypePairingCodeReq      = "remote_pairing_code_request"
	MsgTypeRemotePairingsReq   = "remote_pairings_request"
	MsgTypeRemoteHostRevokeReq = "remote_host_revoke_request"
)

// Approval protocol (bidirectional relay via Cloud)
const (
	MsgTypeApprovalRequest  = "approval_request"
	MsgTypeApprovalResponse = "approval_response"
	MsgTypeApprovalResolved = "approval_resolved"
)

// ApprovalRequest is sent by daemon when a tool needs user approval.
//
// MessageID carries the WebSocket envelope's `message_id` (the inbound claim's
// ID) — Cloud reads it from DaemonMessage.MessageID to look up channel/thread
// context for the approval card. Marked `json:"-"` so it does not leak into
// the payload body; Client.SendApprovalRequest is responsible for copying it
// onto the envelope at send time. When MessageID is empty, Cloud will
// fail-closed (see shannon-cloud `handleApprovalRequest`).
type ApprovalRequest struct {
	MessageID string `json:"-"`
	// SessionID lets Desktop click-through from an inbox card into the
	// originating agent session. Populated from the resolved RunAgent
	// session ID; empty for paths that approve before session resolution.
	SessionID string `json:"session_id,omitempty"`
	// Source is the canonical RunAgentRequest.Source bucket (slack, wecom,
	// schedule, kocoro, ...). Distinct from Channel below.
	Source    string `json:"source,omitempty"`
	Channel   string `json:"channel"`
	ThreadID  string `json:"thread_id"`
	RequestID string `json:"request_id"`
	Tool      string `json:"tool"`
	// Title is a short human label parsed from args.description (the
	// approval-card description required by internal/agent/approval_description.go);
	// falls back to Tool when args has no description.
	Title string `json:"title,omitempty"`
	Args  string `json:"args"`
	Agent string `json:"agent"`
	// Flags is an optional, additive list of policy hints for the UI. Older
	// clients can safely ignore it. Currently emitted:
	//   - "always_allow_disabled": tool is in agent.DisallowsAutoApproval (paid
	//     or permanent public output). UI should disable / hide the
	//     "Always Allow" button so non-technical users don't click it
	//     expecting persistence; daemon still rejects persistence at every
	//     other gate as defense-in-depth.
	Flags []string `json:"flags,omitempty"`
}

// ApprovalFlagAlwaysAllowDisabled is the canonical token used by the daemon
// to tell UI clients to disable the "Always Allow" affordance for a tool
// whose nature (paid quota, permanent public output) requires fresh consent
// every call. See ApprovalRequest.Flags.
const ApprovalFlagAlwaysAllowDisabled = "always_allow_disabled"

// ApprovalResponse is received from the client (via Cloud relay).
type ApprovalResponse struct {
	RequestID  string           `json:"request_id"`
	Decision   ApprovalDecision `json:"decision"`              // "allow", "deny", "always_allow"
	ResolvedBy string           `json:"resolved_by,omitempty"` // populated by Cloud
}

// ApprovalResolvedPayload is sent daemon→Cloud when Ptfrog resolves first.
type ApprovalResolvedPayload struct {
	RequestID  string           `json:"request_id"`
	Decision   ApprovalDecision `json:"decision"`
	ResolvedBy string           `json:"resolved_by"` // "ptfrog", "slack", "line"
}

// Channel types
const (
	ChannelSlack    = "slack"
	ChannelLINE     = "line"
	ChannelTeams    = "teams"
	ChannelWeChat   = "wechat"
	ChannelWeCom    = "wecom"
	ChannelWeb      = "web"
	ChannelFeishu   = "feishu"
	ChannelLark     = "lark"
	ChannelDiscord  = "discord"
	ChannelTelegram = "telegram"
	ChannelSchedule = "schedule"
	ChannelSystem   = "system"
	// ChannelKoe is the voice front-brain (Koe). daemon-LOCAL transport (NOT in
	// cloudSourceSet) but messaging-platform ROUTING (in IsMessagingPlatform):
	// thread keyed by a per-call burst-id. Carrier-neutral — future carriers
	// append a suffix ("koe-reachy") and are matched by isKoeSource, not by ==.
	ChannelKoe = "koe"
)

// Reply format types
const (
	FormatText     = "text"
	FormatMarkdown = "markdown"
)

// ServerMessage is the envelope for all server-to-daemon messages.
type ServerMessage struct {
	Type      string          `json:"type"`
	MessageID string          `json:"message_id,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// DaemonMessage is the envelope for all daemon-to-server messages.
type DaemonMessage struct {
	Type      string          `json:"type"`
	MessageID string          `json:"message_id,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// RemoteRequest is a Cloud-relayed, authenticated request from a paired
// controller such as Kocoro iOS. It intentionally models a narrow HTTP-like
// subset so the daemon can reuse existing local handlers while still enforcing
// an allowlist before any handler is invoked.
type RemoteRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Body    json.RawMessage   `json:"body,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// RemoteResponse is the daemon's response to a RemoteRequest.
type RemoteResponse struct {
	Status  int               `json:"status"`
	Body    []byte            `json:"body,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Error   string            `json:"error,omitempty"`
}

// RemoteEvent forwards a daemon EventBus event to Shannon Cloud so mobile
// clients can subscribe without connecting to localhost.
type RemoteEvent struct {
	ID      uint64          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// RemoteRunRequest starts an asynchronous mobile-originated local agent run.
type RemoteRunRequest struct {
	RunID           string                `json:"run_id"`
	Text            string                `json:"text"`
	Content         []RequestContentBlock `json:"content,omitempty"`
	SessionID       string                `json:"session_id,omitempty"`
	Agent           string                `json:"agent,omitempty"`
	NewSession      bool                  `json:"new_session,omitempty"`
	ClientMessageID string                `json:"client_message_id,omitempty"`
	Files           []RemoteFile          `json:"files,omitempty"`
}

type RemoteRunCancel struct {
	RunID string `json:"run_id"`
}

type RemoteApprovalResponse struct {
	RunID      string           `json:"run_id"`
	RequestID  string           `json:"request_id"`
	Decision   ApprovalDecision `json:"decision"`
	ResolvedBy string           `json:"resolved_by,omitempty"`
}

type RemoteRunEvent struct {
	RunID     string          `json:"run_id"`
	Seq       int64           `json:"seq,omitempty"`
	Type      string          `json:"type"`
	SessionID string          `json:"session_id,omitempty"`
	RequestID string          `json:"request_id,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type PairingCodeRequest struct {
	DeviceID    string `json:"device_id,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Platform    string `json:"platform,omitempty"`
}

type PairingCodeResponse struct {
	Code      string `json:"code,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	Error     string `json:"error,omitempty"`
}

type RemotePairingsRequest struct{}

type RemotePairingController struct {
	ID                 string     `json:"id"`
	ControllerDeviceID string     `json:"controller_device_id"`
	DisplayName        string     `json:"display_name,omitempty"`
	Platform           string     `json:"platform,omitempty"`
	AppVersion         string     `json:"app_version,omitempty"`
	PairedAt           time.Time  `json:"paired_at"`
	LastSeenAt         time.Time  `json:"last_seen_at"`
	RevokedAt          *time.Time `json:"revoked_at,omitempty"`
}

type RemotePairingsResponse struct {
	Controllers []RemotePairingController `json:"controllers"`
	Error       string                    `json:"error,omitempty"`
}

type RemoteHostRevokeRequest struct{}

type RemoteHostRevokeResponse struct {
	Error string `json:"error,omitempty"`
}

// MessagePayload is what the daemon's agent loop processes.
type MessagePayload struct {
	Channel   string                `json:"channel"`
	ThreadID  string                `json:"thread_id"`
	Sender    string                `json:"sender"`
	Text      string                `json:"text"`
	Content   []RequestContentBlock `json:"content,omitempty"` // multimodal content blocks (reserved for Cloud)
	AgentName string                `json:"agent_name,omitempty"`
	MessageID string                `json:"-"` // set locally from envelope, not from JSON
	Timestamp string                `json:"timestamp"`
	Source    string                `json:"source,omitempty"` // populated by Cloud; "slack", "line", "webhook"
	CWD       string                `json:"cwd,omitempty"`    // project path override from Cloud/Desktop
	Files     []RemoteFile          `json:"files,omitempty"`  // file attachments from messaging platforms

	// Participants is the live conversation roster (display names) Cloud
	// fetched from the platform — Bot Framework /pagedmembers for Teams,
	// channels.members for Slack, etc. Empty when the surface has no roster
	// (1:1 chats, webview, tui). The daemon renders this into the sticky
	// context line "Conversation participants:" so the agent knows the full
	// set of names it is allowed to @-mention. Mirrors shannon-cloud's same
	// field; keep the json tag byte-identical.
	Participants []string `json:"participants,omitempty"`

	// IMStatusContext is an opaque blob produced by Cloud that identifies the
	// inbound message on its IM platform. Daemon NEVER decodes this — it is
	// stored on route state and echoed verbatim inside MESSAGE_LIFECYCLE events
	// so Cloud can call platform reaction/status APIs. Empty for non-IM sources
	// (TUI, CLI, scheduled, webhook, etc.).
	IMStatusContext json.RawMessage `json:"im_status_context,omitempty"`

	// ThreadHistory is a Cloud-provided snapshot of the conversation thread this
	// message belongs to (e.g. a Slack thread with several participants / bots).
	// When present, cmd/daemon.go loads it as the run's SessionHistory so the
	// agent sees the FULL thread context, not just messages addressed to it —
	// and because it OVERWRITES the session each turn (not appends), there is no
	// duplication across turns. Empty for every existing flow (macOS shared
	// Slack, Feishu, etc.), which therefore keep their per-session accumulation
	// behavior unchanged.
	ThreadHistory []ThreadHistoryMessage `json:"thread_history,omitempty"`
}

// ThreadHistoryMessage is one prior message in a Cloud-provided thread snapshot.
// Role is "user" or "assistant" (from the bound bot's perspective — its own
// posts are "assistant", everyone else's are "user"). Consecutive same-role
// messages are pre-merged by Cloud so the sequence alternates.
type ThreadHistoryMessage struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// RemoteFile describes a file attachment forwarded by Cloud from a messaging platform.
//
// The mirror struct on the Cloud side is FileAttachment
// (go/orchestrator/internal/daemon/types.go). When Cloud has performed
// server-side extraction or base64 inlining, it populates ExtractedText or
// DocumentB64 in addition to (or instead of) URL — see plan §4.3 for the
// daemon-side priority order. Older daemons that don't decode these fields
// silently fall back to the URL download path, preserving backward compat.
type RemoteFile struct {
	Name       string `json:"name"`
	MimeType   string `json:"mimetype"`
	Size       int64  `json:"size"`
	URL        string `json:"url"`
	AuthHeader string `json:"auth_header"`

	// ExtractedText is plain-text content cloud extracted from the source file
	// (DOCX/XLSX/PPTX/CSV/TXT/JSON/large-PDF fallback path). Non-empty means
	// daemon skips URL download and emits a `text` content block.
	ExtractedText string `json:"extracted_text,omitempty"`
	// DocumentB64 is base64-encoded raw bytes for formats Anthropic accepts
	// natively (currently application/pdf only). Non-empty means daemon emits
	// a `document` content block + companion `text` hint. JSON tag is
	// "document_b64" — matches plan §4.3 byte-for-byte.
	DocumentB64 string `json:"document_b64,omitempty"`
	// ExtractionNote carries cloud-side metadata about how the file was
	// processed (e.g. "extracted via python-docx; tables→markdown",
	// "truncated: sheet_limit, original_sheets=120, included_sheets=100").
	// Daemon currently records it in audit but does not surface it to the LLM;
	// reserved for richer Phase-2 user feedback.
	ExtractionNote string `json:"extraction_note,omitempty"`
}

// ReplyPayload is sent back after agent completes.
type ReplyPayload struct {
	Channel  string `json:"channel"`
	ThreadID string `json:"thread_id"`
	Text     string `json:"text"`
	Format   string `json:"format,omitempty"`
	// Mentions is an optional disambiguation anchor list for outbound @mentions
	// on platforms that require a user identifier (Teams, Slack, ...). When the
	// agent embeds "@name" in Text but the channel roster has duplicate display
	// names, Mentions would pin which person each "@name" refers to (by
	// Email/UPN). Mirrors shannon-cloud ReplyPayload.Mentions byte-for-byte.
	//
	// RESERVED — daemon does not currently populate this field. The agent emits
	// `@<display name>` as free text and SendReply (client.go) constructs the
	// payload without a Mentions value, so duplicate-display-name disambiguation
	// is Cloud-side-only today (resolves unambiguously by name or degrades to
	// plain text). The wire field is in place so a future revision can add a
	// producer (e.g. parsing a `@[name](mailto:upn)` markdown-link syntax in
	// the agent's final text) without a second contract change.
	Mentions []Mention `json:"mentions,omitempty"`
}

// Mention is a disambiguation anchor for outbound @mentions on platforms that
// require a user identifier (Teams, Slack, ...). Name is the display name the
// agent wrote inline; Email is the optional UPN/email the agent has seen in
// the conversation, used to resolve duplicates. Mirrors shannon-cloud
// Mention byte-for-byte.
type Mention struct {
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
}

// ReplyDeliveryResultPayload mirrors the Cloud-side struct; the envelope's
// MessageID is the original inbound message_id the reply was for.
type ReplyDeliveryResultPayload struct {
	OK            bool   `json:"ok"`
	Channel       string `json:"channel"`
	ThreadID      string `json:"thread_id,omitempty"`
	PlatformMsgID string `json:"platform_msg_id,omitempty"`
	// Error is the raw platform error (e.g. "slack API error: not_in_channel");
	// formatDeliveryFailure renders the classified Reason instead. Kept for the
	// wire contract + future logging.
	Error  string `json:"error,omitempty"`
	Reason string `json:"reason,omitempty"` // classified, user-facing
	Class  string `json:"class,omitempty"`  // ClassPermanent / ClassTransient
}

// Delivery classification values carried in ReplyDeliveryResultPayload.Class
// (mirror Cloud's ClassifyDeliveryError output).
const (
	ClassPermanent = "permanent"
	ClassTransient = "transient"
)

// ChannelStateEventPayload is the body of a MsgTypeChannelStateEvent frame: a
// live membership/binding/transport change forwarded by Cloud.
type ChannelStateEventPayload struct {
	Axis      string `json:"axis"`
	Platform  string `json:"platform"`
	ChannelID string `json:"channel_id,omitempty"`
	Change    string `json:"change"`
	Actor     string `json:"actor,omitempty"`
	Reason    string `json:"reason,omitempty"`
	TS        string `json:"ts"`
}

// ProactivePayload is sent by the daemon to push an unsolicited message
// to all channels mapped to the named agent.
type ProactivePayload struct {
	AgentName string `json:"agent_name"`
	Text      string `json:"text"`
	Format    string `json:"format,omitempty"` // "text" (default) or "markdown"
	SessionID string `json:"session_id,omitempty"`
	// IMStatusContext is the opaque platform routing blob the daemon echoes
	// back so Cloud delivers this push to the originating IM thread instead of
	// broadcasting. Empty → Cloud falls back to broadcast (backward compatible:
	// old Cloud ignores the field; new Cloud sees no target from old daemons).
	IMStatusContext json.RawMessage `json:"im_status_context,omitempty"`
	// UseThread controls whether Cloud anchors this push into the originating
	// IM thread or posts at the channel top level. The json tag MUST stay
	// `use_thread,omitempty` — Cloud reads the same field name.
	//   nil   → Cloud uses current behavior (thread-anchor). Old daemons omit
	//           the field, so nil preserves backward compatibility.
	//   *true → anchor into the thread.
	//   *false→ post top-level / independent (still targeted to the channel).
	UseThread *bool `json:"use_thread,omitempty"`
}

// DaemonEventPayload carries a single agent loop event to Cloud.
type DaemonEventPayload struct {
	EventType string                 `json:"event_type"`
	Message   string                 `json:"message"`
	Data      map[string]interface{} `json:"data,omitempty"`
	Seq       int64                  `json:"seq"`
	Timestamp string                 `json:"ts"`
}

// ClaimAckPayload is sent to confirm or deny a claim.
type ClaimAckPayload struct {
	Granted bool `json:"granted"`
}

// IsSystemChannel returns true for channels that don't expect agent processing.
func IsSystemChannel(channel string) bool {
	return channel == ChannelSystem
}
