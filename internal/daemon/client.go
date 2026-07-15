package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// ErrWSAuthRejected is the sentinel returned by Connect / RunWithReconnect
// when Cloud answered the WS upgrade with HTTP 401. AuthManager observes
// this through onAuthFailure (set by SetOnAuthFailure) and transitions
// the daemon back to signed_out — RunWithReconnect must also abort its
// retry loop rather than burning attempts on a key that Cloud has
// already invalidated.
var ErrWSAuthRejected = errors.New("websocket auth rejected")

var errRemoteRunEventTooLarge = errors.New("remote_run_event payload too large")

// MaxConcurrentAgents limits how many agent loops can run simultaneously.
const MaxConcurrentAgents = 5

// Version is the daemon's semver string. Set from cmd.Version at startup
// (see cmd/root.go); defaults to "dev" for un-injected builds. Sent to
// Cloud as the X-Kocoro-Daemon-Version header on WS upgrade for
// telemetry and coarse-grained version-bug fallback signals. Capability
// gating uses the Capabilities slice, not this field.
var Version = "dev"

// Capabilities lists protocol features this daemon supports. Sent to
// Cloud as the comma-separated X-Kocoro-Capabilities header on WS
// upgrade. Cloud parses it to gate optional protocol features so older
// daemons aren't subjected to flows they cannot honor (e.g. per-message
// delivery_ack tracking).
//
// Empty slice → header omitted → Cloud treats the connection as legacy.
// Add a token in the same PR that lands the feature it advertises;
// advertising before implementing causes Cloud to activate flows the
// daemon cannot satisfy.
//
// "delivery_ack" — daemon emits a MsgTypeDeliveryAck envelope after
// each MsgTypeMessage reaches a terminal state (reply delivered to the
// user). Cloud uses this to drop the message from its 5-min replay
// buffer; un-acked messages are replayed on the next reconnect.
//
// "inline_document_b64" — daemon can consume RemoteFile.DocumentB64.
// Non-empty values are decoded to disk and emitted as a `document`
// content block + companion text hint. Cloud uses this token to gate
// PDF base64 inlining (plan §4.5); older daemons without this token
// receive the legacy URL-only payload.
//
// "inline_extracted_text" — daemon can consume RemoteFile.ExtractedText.
// Non-empty values are emitted as a single `text` block prefixed with
// the filename and mimetype. Cloud uses this token to gate server-side
// extraction (DOCX/XLSX/PPTX/CSV/TXT/JSON/large-PDF fallback). Older
// daemons fall back to URL download.
//
// "tool_use_id_events" — daemon emits a tool_use_id field on both the
// running (TOOL_INVOKED / tool_status status=running) and completed
// (TOOL_COMPLETED / tool_status status=completed) tool events on SSE
// and WS, so UIs running multiple bash invocations in parallel can pair
// them up. Optional for consumers; events remain backward-readable
// because older readers ignore unknown keys.
//
// "client_message_queue" — daemon owns a persistent per-route mailbox
// (SQLite-backed at ~/.shannon/sessions/mailbox.db). Durability boundary
// shifts from "ack after SendReply" to "ack after mailbox.Append".
// For active-run IM follow-ups, the daemon also forwards an in-place
// "Queued next" status event to the active channel-stream message when
// Cloud sends the source as slack/wecom/feishu/lark.
//
// "schedule_broadcast_gate" — daemon supports the Schedule.Broadcast +
// Schedule.CreatedFromSource fields and gates each schedule's reply push
// through internal/daemon/broadcast_gate.go shouldBroadcast(). Desktops
// reading this token can show the broadcast badge / picker UI; daemons
// without the token use the legacy unconditional broadcast (per agent
// binding). Both daemon shapes interoperate with the same Cloud.
//
// "im_timeline_v1" — daemon emits a single ordered timeline per IM message:
// mid-turn narration via OnPreamble (LLM_OUTPUT) interleaved with TOOL_RUNNING
// / TOOL_COMPLETED frames, and the final answer only via SendReply →
// WORKFLOW_COMPLETED (OnText no longer double-emits it as LLM_OUTPUT). Cloud
// gates timeline-mode rendering on this token; daemons without it keep the
// legacy behavior where the final answer is emitted as a trailing LLM_OUTPUT.
//
// "agent_profile_v1" — daemon includes read-only agent presentation metadata
// on GET /agents/{name}: category, description, guide_prompts, and examples.
// Desktop gates the richer agent-profile UI on this token instead of
// version-sniffing or inferring support from nullable fields.
//
// "deliverable_event_v1" — daemon emits EventDeliverable when the
// present_deliverable tool records daemon-validated metadata for a local regular
// file. Desktop gates the Deliverables sidebar live-SSE path on this token,
// then dedupes live, replayed, and persisted records by deliverable id.
const (
	CapDeliveryAck           = "delivery_ack"
	CapInlineDocumentB64     = "inline_document_b64"
	CapInlineExtractedText   = "inline_extracted_text"
	CapToolUseIDEvents       = "tool_use_id_events"
	CapClientMessageQueue    = "client_message_queue"
	CapScheduleBroadcastGate = "schedule_broadcast_gate"
	CapIMTimelineV1          = "im_timeline_v1"
	CapAgentProfileV1        = "agent_profile_v1"
	// CapAgentAvatarV1 — daemon supports avatar on PROFILE.yaml (write + Cloud
	// sync). Desktop gates avatar editing UI on this token.
	CapAgentAvatarV1 = "agent_avatar_v1"
	// CapProactiveTargeting tells Cloud the daemon may attach an IMStatusContext
	// to a ProactivePayload for precise routing. Observability only — the
	// fallback rule is "non-empty target → targeted; empty → broadcast", so the
	// token is not load-bearing for correctness.
	CapProactiveTargeting = "proactive_targeting"
	// CapProactiveThreadMode tells Cloud the daemon may attach a UseThread hint
	// to a ProactivePayload to control IM thread anchoring. Observability only —
	// Cloud reads the field directly (nil → current thread-anchor behavior), so
	// the token is not load-bearing for correctness.
	CapProactiveThreadMode   = "proactive_thread_mode"
	CapReplyDeliveryResultV1 = "reply_delivery_result_v1"
	// CapChannelStateEventV1 — daemon consumes channel_state_event frames
	// (live membership/binding/transport changes). Independent of
	// CapReplyDeliveryResultV1 so S3 can land separately from S2.
	CapChannelStateEventV1 = "channel_state_event_v1"
	CapDeliverableEventV1  = "deliverable_event_v1"
	// CapMentionRosterV1 tells Cloud the daemon (a) accepts
	// MessagePayload.Participants and renders the conversation roster into
	// sticky context as a "Conversation participants:" bulleted list the
	// prompt's @-mention path resolves against, and (b) lets the agent emit
	// inline `@<display name>` in reply text for Cloud-side resolution to a
	// platform user identifier. Observability only — `participants` and the
	// inline `@name` convention both rely on omitempty + ignore-on-decode for
	// silent degradation across mismatched versions, but the token lets Cloud
	// detect support without version sniffing (CLAUDE.md Wire Contract
	// Discipline). The ReplyPayload.Mentions structured-disambiguation field
	// is reserved for a future revision — daemon does not currently populate
	// it.
	CapMentionRosterV1 = "mention_roster_v1"
	// CapDefaultAgentSkillDenylist — daemon supports per-skill enable/disable for
	// the DEFAULT agent via config.skills.disabled + POST/DELETE /skills/disabled,
	// and annotates GET /skills with default_agent_disabled. Desktop gates its
	// default-agent skills UI on this token (old daemons → hide the UI, default
	// agent keeps loading every installed skill).
	CapDefaultAgentSkillDenylist = "default_agent_skill_denylist"
	// CapPerAgentMCPScope — daemon enforces per-agent MCP server selection:
	// named agents are limited to their mcp_servers set at tool-dispatch time
	// (not just prompt context), and the default agent honors
	// config.mcp.default_agent_disabled (POST/DELETE /mcp/default-disabled +
	// GET /config/status mcp_default_agent_disabled). Desktop gates its per-agent
	// MCP selection UI on this.
	CapPerAgentMCPScope = "per_agent_mcp_scope"
	// CapSessionsScopeAll — daemon supports the cross-agent session list/search:
	// GET /sessions?scope=all and GET /sessions/search?scope=all merge the
	// default scope with every named agent's sessions, each row carrying an
	// `agent` attribution field, and GET /sessions gains limit/offset+total/
	// has_more pagination. Desktop gates its "All agents" global session UI on
	// this token — an old daemon that predates scope=all reports no token, so
	// Desktop disables the global view rather than sniffing the response shape
	// (an unlimited single-scope response also has has_more:false, so shape
	// sniffing is ambiguous).
	CapSessionsScopeAll = "sessions_scope_all"
	// CapAgentDefaultCWDV1 — named-agent cwd writes are validated before any
	// mutation, invalid persisted cwd is surfaced as a non-fatal warning, and
	// cross-device agent sync treats cwd as device-local (never pushed or
	// overwritten by pull). Desktop gates editable default-working-folder UI on
	// this complete contract rather than probing individual behaviors.
	CapAgentDefaultCWDV1 = "agent_default_cwd_v1"
	// CapRemoteControlV1 — daemon accepts Cloud-relayed remote_request frames
	// for a narrow allowlisted local API subset and forwards EventBus events as
	// remote_event frames. Mobile clients use this to control the user's Mac via
	// Shannon Cloud without exposing localhost.
	CapRemoteControlV1 = "remote_control_v1"
	// CapRemoteSessionTimelineV1 — GET /sessions/{id}?view=remote_timeline
	// returns a byte-bounded newest-first page whose large images, thinking
	// blocks, and verbose tool payloads are explicitly projected for mobile.
	// The legacy GET /sessions/{id} response remains the complete session.
	CapRemoteSessionTimelineV1 = "remote_session_timeline_v1"
	// CapClawHubExcludeInstalled — daemon's GET /skills/clawhub accepts
	// exclude_installed=true, dropping already-installed skills from the browse/
	// search list and refilling from subsequent pages so the page stays
	// populated. Desktop gates its "hide installed" marketplace toggle on this
	// token; old daemons ignore the param (return the full list incl. installed),
	// so without the token Desktop hides the toggle rather than silently no-oping.
	CapClawHubExcludeInstalled = "clawhub_exclude_installed"
	// CapSearchV1 — daemon exposes GET /search: a session-grouped content search
	// over clean message text (tool_result/tool_use dumps excluded), returning
	// pre-segmented highlighted snippets + match counts + limit/offset paging +
	// total/has_more, scoped default|<agent>|all. Desktop gates its ⌘K content
	// search on this token; an old daemon omits it, so Desktop falls back to
	// title-only (in-memory) search rather than calling a 404 route.
	CapSearchV1 = "search_v1"
)

var Capabilities = []string{
	CapDeliveryAck,
	CapInlineDocumentB64,
	CapInlineExtractedText,
	CapToolUseIDEvents,
	CapClientMessageQueue,
	CapIMMessageLifecycleV1,
	CapIMTimelineV1,
	CapAgentProfileV1,
	CapAgentAvatarV1,
	CapScheduleBroadcastGate,
	CapProactiveTargeting,
	CapProactiveThreadMode,
	CapReplyDeliveryResultV1,
	CapChannelStateEventV1,
	CapDeliverableEventV1,
	CapMentionRosterV1,
	CapDefaultAgentSkillDenylist,
	CapPerAgentMCPScope,
	CapSessionsScopeAll,
	CapAgentDefaultCWDV1,
	CapRemoteControlV1,
	CapRemoteSessionTimelineV1,
	CapClawHubExcludeInstalled,
	CapSearchV1,
}

// envelopeSenderFn lets tests substitute sendEnvelope without standing up a
// real WebSocket. Production wiring (NewClient) defaults the field to
// c.sendEnvelope so callers see zero behavior change.
type envelopeSenderFn func(DaemonMessage) error

type Client struct {
	endpoint              string
	conn                  *websocket.Conn
	writeMu               sync.Mutex
	onMsg                 func(MessagePayload) string              // returns reply text
	onSystem              func(string)                             // system notifications
	onReplyDeliveryResult func(ReplyDeliveryResultPayload, string) // (payload, original message_id)
	onChannelStateEvent   func(ChannelStateEventPayload)
	onRemoteRequest       func(context.Context, RemoteRequest) RemoteResponse
	onRemoteRun           func(context.Context, RemoteRunRequest)
	onRemoteRunCancel     func(RemoteRunCancel)
	onRemoteApproval      func(RemoteApprovalResponse)
	onRemoteRunReplay     func()
	sem                   chan struct{}
	pendingClaims         sync.Map // map[string]chan bool
	pendingPairingCodes   sync.Map // map[string]chan PairingCodeResponse
	pendingRemotePairings sync.Map // map[string]chan RemotePairingsResponse
	pendingRemoteRevokes  sync.Map // map[string]chan RemoteHostRevokeResponse
	activeMsgs            sync.Map // map[string]context.CancelFunc
	eventSeqs             sync.Map // map[string]*atomic.Int64
	pendingReplies        sync.Map // map[string]pendingReply — per-message reply override set by onMsg during RunAgent
	connected             atomic.Bool
	activeAgent           atomic.Value // stores string
	startTime             time.Time
	broker                *ApprovalBroker
	eventBus              *EventBus
	deviceInfo            DeviceInfo

	keyMu  sync.RWMutex
	apiKey string

	// onAuthFailure fires when Cloud rejects a WS upgrade with 401 —
	// installed by AuthManager via SetOnAuthFailure. The callback runs
	// asynchronously (go cb()) so it cannot deadlock against Client locks
	// taken inside Connect / RunWithReconnect. Nil-tolerant: a daemon
	// built without an AuthManager (non-darwin legacy path) simply skips
	// the notification and lets RunWithReconnect exit on ErrWSAuthRejected.
	onAuthFailure func()

	// envelopeSender dispatches every outgoing DaemonMessage. Defaulted to
	// c.sendEnvelope in NewClient; tests inject a fake to capture wire
	// output without a real WebSocket.
	envelopeSender envelopeSenderFn
}

// SetEventBus sets the event bus for emitting daemon events.
func (c *Client) SetEventBus(bus *EventBus) {
	c.eventBus = bus
}

// SetAPIKey swaps the api_key used in the WS upgrade Authorization
// header. AuthManager calls this on login (bootstrap), sign-out (clear),
// and Bootstrap (Keychain restore). Concurrent in-flight WS upgrades
// captured the prior key at Connect-time via getAPIKey; subsequent
// reconnect attempts use the new value.
func (c *Client) SetAPIKey(key string) {
	c.keyMu.Lock()
	c.apiKey = key
	c.keyMu.Unlock()
}

// SetOnAuthFailure registers the callback invoked when Connect observes
// an HTTP 401 from the WS upgrade. Setting it to nil disables the
// notification (RunWithReconnect still aborts on ErrWSAuthRejected).
func (c *Client) SetOnAuthFailure(cb func()) {
	c.keyMu.Lock()
	c.onAuthFailure = cb
	c.keyMu.Unlock()
}

// SetOnReplyDeliveryResult registers the consumer for reply_delivery_result
// frames. Pass nil to ignore them. Wired in cmd/daemon.go to the
// SystemEventStore + ReplyRouteIndex.
func (c *Client) SetOnReplyDeliveryResult(cb func(ReplyDeliveryResultPayload, string)) {
	c.onReplyDeliveryResult = cb
}

// SetOnChannelStateEvent registers the consumer for channel_state_event frames.
// Pass nil to ignore. Wired in cmd/daemon.go to the ConnectionStateCache +
// SystemEventStore + SessionCache route resolver.
func (c *Client) SetOnChannelStateEvent(cb func(ChannelStateEventPayload)) {
	c.onChannelStateEvent = cb
}

// SetRemoteRequestHandler registers the local handler for Cloud-relayed remote
// control requests. The handler is installed by the daemon HTTP server so it
// can reuse the same local API implementation and allowlist.
func (c *Client) SetRemoteRequestHandler(cb func(context.Context, RemoteRequest) RemoteResponse) {
	c.onRemoteRequest = cb
}

func (c *Client) SetRemoteRunHandler(cb func(context.Context, RemoteRunRequest)) {
	c.onRemoteRun = cb
}

func (c *Client) SetRemoteRunCancelHandler(cb func(RemoteRunCancel)) {
	c.onRemoteRunCancel = cb
}

func (c *Client) SetRemoteApprovalHandler(cb func(RemoteApprovalResponse)) {
	c.onRemoteApproval = cb
}

func (c *Client) SetRemoteRunReplayHandler(cb func()) {
	c.onRemoteRunReplay = cb
}

func (c *Client) SetDeviceInfo(info DeviceInfo) {
	c.deviceInfo = info
}

func (c *Client) getAPIKey() string {
	c.keyMu.RLock()
	defer c.keyMu.RUnlock()
	return c.apiKey
}

func (c *Client) getOnAuthFailure() func() {
	c.keyMu.RLock()
	defer c.keyMu.RUnlock()
	return c.onAuthFailure
}

func NewClient(endpoint, apiKey string, onMsg func(MessagePayload) string, onSystem func(string)) *Client {
	c := &Client{
		endpoint:  endpoint,
		apiKey:    apiKey,
		onMsg:     onMsg,
		onSystem:  onSystem,
		sem:       make(chan struct{}, MaxConcurrentAgents),
		startTime: time.Now(),
	}
	c.envelopeSender = c.sendEnvelope
	return c
}

func (c *Client) Connect(ctx context.Context) error {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+c.getAPIKey())
	header.Set("User-Agent", fmt.Sprintf("kocoro/%s (%s; %s)", Version, runtime.GOOS, runtime.GOARCH))
	header.Set("X-Kocoro-Daemon-Version", Version)
	if len(Capabilities) > 0 {
		header.Set("X-Kocoro-Capabilities", strings.Join(Capabilities, ","))
	}
	if c.deviceInfo.DeviceID != "" {
		header.Set("X-Kocoro-Device-ID", c.deviceInfo.DeviceID)
	}
	if c.deviceInfo.DisplayName != "" {
		header.Set("X-Kocoro-Device-Name", c.deviceInfo.DisplayName)
	}
	if c.deviceInfo.Platform != "" {
		header.Set("X-Kocoro-Platform", c.deviceInfo.Platform)
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}
	conn, resp, err := dialer.DialContext(ctx, c.endpoint, header)
	if err != nil {
		// Cloud rejected the upgrade with a real HTTP response (vs a
		// pure transport error). 401 means the api_key is invalid —
		// surface ErrWSAuthRejected and notify AuthManager so it can
		// clear Keychain and transition to signed_out. RunWithReconnect
		// detects the sentinel and stops retrying.
		if resp != nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized {
				if cb := c.getOnAuthFailure(); cb != nil {
					go cb()
				}
				return fmt.Errorf("%w: %v", ErrWSAuthRejected, err)
			}
		}
		return fmt.Errorf("websocket connect: %w", err)
	}
	c.conn = conn
	return nil
}

// IsConnected reports whether the client has an active WebSocket connection.
func (c *Client) IsConnected() bool {
	return c.connected.Load()
}

// ActiveAgent returns the name of the agent currently processing a message,
// or "" if idle.
func (c *Client) ActiveAgent() string {
	if v := c.activeAgent.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// Uptime returns how long since the client was created.
func (c *Client) Uptime() time.Duration {
	return time.Since(c.startTime)
}

func (c *Client) sendEnvelope(dm DaemonMessage) error {
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}
	data, err := json.Marshal(dm)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

func (c *Client) sendClaim(messageID string) error {
	return c.envelopeSender(DaemonMessage{Type: MsgTypeClaim, MessageID: messageID})
}

func (c *Client) sendProgress(messageID string) error {
	return c.envelopeSender(DaemonMessage{Type: MsgTypeProgress, MessageID: messageID})
}

// pendingReply carries a per-message reply override set by the onMsg callback
// while RunAgent is in flight, consumed once in handleMessage:
//   - ReplyToID redirects the final reply+ack to a DIFFERENT inbound message id
//     (a run that absorbed a mid-run injected follow-up answers it under its own
//     cloud id, so the channel renders separate messages, not one merged reply).
//   - Suppress drops the reply+ack entirely (this message was injected into
//     another active run, which completes it under its own id).
type pendingReply struct {
	// ReplyToID addresses the final reply (empty = inbound id).
	ReplyToID string
	// AckIDs are acked AFTER the reply is delivered — every inbound id the run
	// absorbed but did not reply to independently (includes ReplyToID). Empty
	// means ack just the replied id.
	AckIDs []string
	// Suppress skips reply AND ack: the owning run completes + acks this id once
	// ITS reply is delivered (the ack-after-delivery invariant for injects).
	Suppress bool
}

// SetReplyPlan records how handleMessage should finalize inboundID: send the
// final reply to replyToID (empty = inboundID) and, AFTER it is delivered, ack
// every id in ackIDs — the inbound ids this run absorbed but did not reply to
// independently. Acking only post-delivery preserves the delivery-ack invariant
// for absorbed/merged messages (a reply failure replays them rather than losing
// the answer). Set by the onMsg callback before returning; consumed once in
// handleMessage.
func (c *Client) SetReplyPlan(inboundID, replyToID string, ackIDs []string) {
	if inboundID == "" {
		return
	}
	c.pendingReplies.Store(inboundID, pendingReply{ReplyToID: replyToID, AckIDs: ackIDs})
}

// SuppressReply records that inboundID's reply AND ack must be skipped in
// handleMessage — the message was injected into an active run that completes it
// (reply + ack) under its own id once that run's reply is delivered. Set by the
// onMsg callback for injected follow-ups.
func (c *Client) SuppressReply(inboundID string) {
	if inboundID == "" {
		return
	}
	c.pendingReplies.Store(inboundID, pendingReply{Suppress: true})
}

// SendDeliveryAck signals to Cloud that the inbound message reached a
// terminal state (success or error reply already delivered to the
// user). Cloud drops the entry from its replay buffer so a subsequent
// disconnect+reconnect doesn't re-deliver the same message. Called
// only on SendReply success — if the reply itself failed to flush,
// the user wasn't informed and Cloud must replay on reconnect. Exported
// so the daemon event handler can ack a superseded turn's own reply
// (OnIntermediateAnswer) under that message's cloud id.
//
// Empty messageID is a no-op so callers don't have to guard.
func (c *Client) SendDeliveryAck(messageID string) error {
	if messageID == "" {
		return nil
	}
	return c.envelopeSender(DaemonMessage{Type: MsgTypeDeliveryAck, MessageID: messageID})
}

// SendProgressWithWorkflow sends a progress heartbeat with a workflow_id payload.
// This tells Cloud to start streaming card replies for the originating channel.
func (c *Client) SendProgressWithWorkflow(messageID, workflowID string) error {
	payload, _ := json.Marshal(map[string]string{"workflow_id": workflowID})
	return c.envelopeSender(DaemonMessage{Type: MsgTypeProgress, MessageID: messageID, Payload: payload})
}

// SendEvent sends a daemon agent loop event to Cloud for channel streaming.
// Fire-and-forget: errors are returned but callers should log and continue.
func (c *Client) SendEvent(messageID string, eventType, message string, data map[string]interface{}) error {
	val, _ := c.eventSeqs.LoadOrStore(messageID, new(atomic.Int64))
	seq := val.(*atomic.Int64).Add(1)

	payload, err := json.Marshal(DaemonEventPayload{
		EventType: eventType,
		Message:   message,
		Data:      data,
		Seq:       seq,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	return c.envelopeSender(DaemonMessage{
		Type:      MsgTypeEvent,
		MessageID: messageID,
		Payload:   payload,
	})
}

// SendReply sends the final reply for a message and cancels its heartbeat.
func (c *Client) SendReply(messageID string, payload ReplyPayload) error {
	c.eventSeqs.Delete(messageID)
	if cancel, ok := c.activeMsgs.LoadAndDelete(messageID); ok {
		cancel.(context.CancelFunc)()
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.envelopeSender(DaemonMessage{Type: MsgTypeReply, MessageID: messageID, Payload: payloadBytes})
}

// SendProactive sends an unsolicited message to all channels mapped to the agent.
// This is fire-and-forget — no claim/ack cycle.
//
// Empty agentName is a valid case: it represents the default agent, which Cloud
// routes to channels whose config has no agent_name key (default-bound). Cloud
// owns the "is anyone listening" decision; daemon doesn't pre-filter.
//
// imStatusContext is the opaque routing target echoed back to Cloud for precise
// delivery to the originating IM thread; empty (nil) → Cloud falls back to
// broadcast (preserving pre-targeting behavior).
//
// useThread controls IM thread anchoring (see ProactivePayload.UseThread):
// nil → Cloud's current thread-anchor behavior; *true → thread; *false →
// top-level. Callers without a thread opinion (e.g. heartbeat) pass nil.
func (c *Client) SendProactive(agentName, text, sessionID string, imStatusContext json.RawMessage, useThread *bool) error {
	if text == "" {
		return nil
	}
	payload, err := json.Marshal(ProactivePayload{
		AgentName:       agentName,
		Text:            text,
		Format:          FormatText,
		SessionID:       sessionID,
		IMStatusContext: imStatusContext,
		UseThread:       useThread,
	})
	if err != nil {
		return fmt.Errorf("marshal proactive payload: %w", err)
	}
	return c.envelopeSender(DaemonMessage{
		Type:    MsgTypeProactive,
		Payload: payload,
	})
}

func (c *Client) sendDisconnect() error {
	return c.envelopeSender(DaemonMessage{Type: MsgTypeDisconnect})
}

// Close sends a disconnect message and closes the WebSocket connection.
func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	_ = c.sendDisconnect()
	return c.conn.Close()
}

// SetApprovalBroker sets the broker for interactive tool approval.
func (c *Client) SetApprovalBroker(b *ApprovalBroker) {
	c.broker = b
}

// ResolveApproval delivers an external decision (e.g. POST /approval from
// Desktop) to the WS broker, for approvals whose pending request lives only
// here (cloud/IM sources). Returns false if the broker is unset or the
// request was already claimed by another terminal path.
func (c *Client) ResolveApproval(requestID string, decision ApprovalDecision, beforeDeliver func()) bool {
	if c == nil || c.broker == nil {
		return false
	}
	return c.broker.Resolve(requestID, decision, beforeDeliver)
}

// SendApprovalRequest sends an approval_request message over WS.
//
// The envelope's MessageID is set from req.MessageID (the inbound claim's ID).
// Cloud reads it from the envelope, not the payload, to resolve the originating
// channel/thread for the approval card. Sending without a MessageID will be
// rejected fail-closed by Cloud.
func (c *Client) SendApprovalRequest(req ApprovalRequest) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return c.envelopeSender(DaemonMessage{
		Type:      MsgTypeApprovalRequest,
		MessageID: req.MessageID,
		Payload:   payload,
	})
}

// SendApprovalResolved sends an approval_resolved message over WS to Cloud.
func (c *Client) SendApprovalResolved(p ApprovalResolvedPayload) error {
	payload, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return c.envelopeSender(DaemonMessage{
		Type:    MsgTypeApprovalResolved,
		Payload: payload,
	})
}

// SendRemoteEvent forwards a local EventBus event to Cloud for mobile/remote
// subscribers. It is best-effort; callers should log and continue on error.
func (c *Client) SendRemoteEvent(evt Event) error {
	payload, err := json.Marshal(RemoteEvent{
		ID:      evt.ID,
		Type:    evt.Type,
		Payload: evt.Payload,
	})
	if err != nil {
		return err
	}
	return c.envelopeSender(DaemonMessage{
		Type:    MsgTypeRemoteEvent,
		Payload: payload,
	})
}

func (c *Client) SendRemoteRunEvent(evt RemoteRunEvent) error {
	payload, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	if len(payload) > maxRemoteRunEventBytes {
		return fmt.Errorf("%w: %d bytes > %d", errRemoteRunEventTooLarge, len(payload), maxRemoteRunEventBytes)
	}
	return c.envelopeSender(DaemonMessage{
		Type:    MsgTypeRemoteRunEvent,
		Payload: payload,
	})
}

func (c *Client) RequestPairingCode(ctx context.Context) (PairingCodeResponse, error) {
	if c == nil || c.envelopeSender == nil {
		return PairingCodeResponse{}, fmt.Errorf("remote pairing unavailable")
	}
	messageID := generateRequestID()
	ch := make(chan PairingCodeResponse, 1)
	c.pendingPairingCodes.Store(messageID, ch)
	defer c.pendingPairingCodes.Delete(messageID)
	payload, err := json.Marshal(PairingCodeRequest{
		DeviceID:    c.deviceInfo.DeviceID,
		DisplayName: c.deviceInfo.DisplayName,
		Platform:    c.deviceInfo.Platform,
	})
	if err != nil {
		return PairingCodeResponse{}, err
	}
	if err := c.envelopeSender(DaemonMessage{
		Type:      MsgTypePairingCodeReq,
		MessageID: messageID,
		Payload:   payload,
	}); err != nil {
		return PairingCodeResponse{}, err
	}
	select {
	case resp := <-ch:
		if resp.Error != "" {
			return resp, errors.New(resp.Error)
		}
		return resp, nil
	case <-ctx.Done():
		return PairingCodeResponse{}, ctx.Err()
	}
}

func (c *Client) RequestRemotePairings(ctx context.Context) (RemotePairingsResponse, error) {
	if c == nil || c.envelopeSender == nil {
		return RemotePairingsResponse{}, fmt.Errorf("remote pairings unavailable")
	}
	messageID := generateRequestID()
	ch := make(chan RemotePairingsResponse, 1)
	c.pendingRemotePairings.Store(messageID, ch)
	defer c.pendingRemotePairings.Delete(messageID)
	payload, err := json.Marshal(RemotePairingsRequest{})
	if err != nil {
		return RemotePairingsResponse{}, err
	}
	if err := c.envelopeSender(DaemonMessage{
		Type:      MsgTypeRemotePairingsReq,
		MessageID: messageID,
		Payload:   payload,
	}); err != nil {
		return RemotePairingsResponse{}, err
	}
	select {
	case resp := <-ch:
		if resp.Error != "" {
			return resp, errors.New(resp.Error)
		}
		return resp, nil
	case <-ctx.Done():
		return RemotePairingsResponse{}, ctx.Err()
	}
}

func (c *Client) RequestRemoteHostRevoke(ctx context.Context) (RemoteHostRevokeResponse, error) {
	if c == nil || c.envelopeSender == nil {
		return RemoteHostRevokeResponse{}, fmt.Errorf("remote host revoke unavailable")
	}
	messageID := generateRequestID()
	ch := make(chan RemoteHostRevokeResponse, 1)
	c.pendingRemoteRevokes.Store(messageID, ch)
	defer c.pendingRemoteRevokes.Delete(messageID)
	payload, err := json.Marshal(RemoteHostRevokeRequest{})
	if err != nil {
		return RemoteHostRevokeResponse{}, err
	}
	if err := c.envelopeSender(DaemonMessage{
		Type:      MsgTypeRemoteHostRevokeReq,
		MessageID: messageID,
		Payload:   payload,
	}); err != nil {
		return RemoteHostRevokeResponse{}, err
	}
	select {
	case resp := <-ch:
		if resp.Error != "" {
			return resp, errors.New(resp.Error)
		}
		return resp, nil
	case <-ctx.Done():
		return RemoteHostRevokeResponse{}, ctx.Err()
	}
}

func (c *Client) sendRemoteResponse(messageID string, resp RemoteResponse) error {
	if resp.Status == 0 {
		resp.Status = http.StatusOK
	}
	payload, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	return c.envelopeSender(DaemonMessage{
		Type:      MsgTypeRemoteResponse,
		MessageID: messageID,
		Payload:   payload,
	})
}

// Listen reads messages from the WebSocket and dispatches them.
// It blocks until the context is cancelled or the connection drops.
func (c *Client) Listen(ctx context.Context) error {
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}
	c.connected.Store(true)
	defer func() {
		c.connected.Store(false)
		if c.broker != nil {
			c.broker.CancelAll()
		}
		c.conn.Close()
	}()

	go func() {
		<-ctx.Done()
		_ = c.sendDisconnect()
		c.conn.Close()
	}()

	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read: %w", err)
		}

		var sm ServerMessage
		if err := json.Unmarshal(data, &sm); err != nil {
			log.Printf("daemon: invalid message: %v", err)
			continue
		}

		switch sm.Type {
		case MsgTypeConnected:
			log.Println("daemon: connected to Shannon Cloud")
			if c.onRemoteRunReplay != nil {
				go c.onRemoteRunReplay()
			}
		case MsgTypeMessage:
			go c.handleMessage(ctx, sm)
		case MsgTypeClaimAck:
			if ch, ok := c.pendingClaims.Load(sm.MessageID); ok {
				var ack ClaimAckPayload
				if err := json.Unmarshal(sm.Payload, &ack); err == nil {
					select {
					case ch.(chan bool) <- ack.Granted:
					default:
					}
				}
			}
		case MsgTypeApprovalResponse:
			var resp ApprovalResponse
			if err := json.Unmarshal(sm.Payload, &resp); err != nil {
				log.Printf("daemon: invalid approval_response: %v", err)
				continue
			}
			resolvedBy := resp.ResolvedBy
			if resolvedBy == "" {
				resolvedBy = "external"
			}
			// Claim under the broker's lock first; bus emit runs as Resolve's
			// beforeDeliver so it fires only when we won the claim AND lands
			// on the bus with an ID earlier than any agent event the
			// resuming Request goroutine could trigger. A concurrent
			// daemon-cleanup path (timeout / ctx-cancel / CancelAll on
			// disconnect) that already claimed makes Resolve a no-op here,
			// preserving the at-most-one terminal-event contract.
			if c.broker != nil {
				c.broker.Resolve(resp.RequestID, resp.Decision, func() {
					emitBusJSON(c.eventBus, EventApprovalResolved, map[string]any{
						"request_id":  resp.RequestID,
						"decision":    string(resp.Decision),
						"resolved_by": resolvedBy,
						"ts":          nowISO(),
					})
				})
			}
		case MsgTypeSystem:
			if c.onSystem != nil {
				var text string
				if err := json.Unmarshal(sm.Payload, &text); err == nil {
					c.onSystem(text)
				}
			}
		case MsgTypeReplyDeliveryResult:
			if c.onReplyDeliveryResult != nil {
				var p ReplyDeliveryResultPayload
				if err := json.Unmarshal(sm.Payload, &p); err != nil {
					log.Printf("daemon: invalid reply_delivery_result: %v", err)
					continue
				}
				c.onReplyDeliveryResult(p, sm.MessageID)
			}
		case MsgTypeChannelStateEvent:
			if c.onChannelStateEvent != nil {
				var p ChannelStateEventPayload
				if err := json.Unmarshal(sm.Payload, &p); err != nil {
					log.Printf("daemon: invalid channel_state_event: %v", err)
					continue
				}
				c.onChannelStateEvent(p)
			}
		case MsgTypeRemoteRequest:
			go c.handleRemoteRequest(ctx, sm)
		case MsgTypeRemoteRunRequest:
			go c.handleRemoteRun(ctx, sm)
		case MsgTypeRemoteRunCancel:
			c.handleRemoteRunCancel(sm)
		case MsgTypeRemoteApproval:
			c.handleRemoteApproval(sm)
		case MsgTypePairingCodeResponse:
			c.handlePairingCodeResponse(sm)
		case MsgTypeRemotePairingsResponse:
			c.handleRemotePairingsResponse(sm)
		case MsgTypeRemoteHostRevokeResponse:
			c.handleRemoteHostRevokeResponse(sm)
		default:
			log.Printf("daemon: unknown message type: %s", sm.Type)
		}
	}
}

func (c *Client) handleRemoteRequest(ctx context.Context, sm ServerMessage) {
	if sm.MessageID == "" {
		log.Printf("daemon: remote_request missing message_id")
		return
	}
	if c.onRemoteRequest == nil {
		_ = c.sendRemoteResponse(sm.MessageID, RemoteResponse{
			Status: http.StatusServiceUnavailable,
			Error:  "remote control handler unavailable",
		})
		return
	}
	var req RemoteRequest
	if err := json.Unmarshal(sm.Payload, &req); err != nil {
		_ = c.sendRemoteResponse(sm.MessageID, RemoteResponse{
			Status: http.StatusBadRequest,
			Error:  "invalid remote_request payload",
		})
		return
	}
	resp := c.onRemoteRequest(ctx, req)
	if err := c.sendRemoteResponse(sm.MessageID, resp); err != nil {
		log.Printf("daemon: remote_response failed for %s: %v", sm.MessageID, err)
	}
}

func (c *Client) handleRemoteRun(ctx context.Context, sm ServerMessage) {
	if c.onRemoteRun == nil {
		log.Printf("daemon: remote_run_request handler unavailable")
		return
	}
	var req RemoteRunRequest
	if err := json.Unmarshal(sm.Payload, &req); err != nil {
		log.Printf("daemon: invalid remote_run_request payload: %v", err)
		return
	}
	c.onRemoteRun(ctx, req)
}

func (c *Client) handleRemoteRunCancel(sm ServerMessage) {
	if c.onRemoteRunCancel == nil {
		return
	}
	var req RemoteRunCancel
	if err := json.Unmarshal(sm.Payload, &req); err != nil {
		log.Printf("daemon: invalid remote_run_cancel payload: %v", err)
		return
	}
	c.onRemoteRunCancel(req)
}

func (c *Client) handleRemoteApproval(sm ServerMessage) {
	if c.onRemoteApproval == nil {
		return
	}
	var resp RemoteApprovalResponse
	if err := json.Unmarshal(sm.Payload, &resp); err != nil {
		log.Printf("daemon: invalid remote_approval_response payload: %v", err)
		return
	}
	c.onRemoteApproval(resp)
}

func (c *Client) handlePairingCodeResponse(sm ServerMessage) {
	if sm.MessageID == "" {
		return
	}
	var resp PairingCodeResponse
	if err := json.Unmarshal(sm.Payload, &resp); err != nil {
		resp = PairingCodeResponse{Error: "invalid pairing response payload"}
	}
	if ch, ok := c.pendingPairingCodes.Load(sm.MessageID); ok {
		select {
		case ch.(chan PairingCodeResponse) <- resp:
		default:
		}
	}
}

func (c *Client) handleRemotePairingsResponse(sm ServerMessage) {
	if sm.MessageID == "" {
		return
	}
	var resp RemotePairingsResponse
	if err := json.Unmarshal(sm.Payload, &resp); err != nil {
		resp = RemotePairingsResponse{Error: "invalid remote pairings response payload"}
	}
	if resp.Controllers == nil {
		resp.Controllers = []RemotePairingController{}
	}
	if ch, ok := c.pendingRemotePairings.Load(sm.MessageID); ok {
		select {
		case ch.(chan RemotePairingsResponse) <- resp:
		default:
		}
	}
}

func (c *Client) handleRemoteHostRevokeResponse(sm ServerMessage) {
	if sm.MessageID == "" {
		return
	}
	var resp RemoteHostRevokeResponse
	if err := json.Unmarshal(sm.Payload, &resp); err != nil {
		resp = RemoteHostRevokeResponse{Error: "invalid remote host revoke response payload"}
	}
	if ch, ok := c.pendingRemoteRevokes.Load(sm.MessageID); ok {
		select {
		case ch.(chan RemoteHostRevokeResponse) <- resp:
		default:
		}
	}
}

func (c *Client) handleMessage(ctx context.Context, sm ServerMessage) {
	var payload MessagePayload
	if err := json.Unmarshal(sm.Payload, &payload); err != nil {
		log.Printf("daemon: invalid message payload: %v", err)
		return
	}

	// Acquire semaphore for bounded concurrency with context check.
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		log.Printf("daemon: context cancelled waiting for semaphore (message %s)", sm.MessageID)
		return
	}
	defer func() { <-c.sem }()

	// Send claim.
	claimCh := make(chan bool, 1)
	c.pendingClaims.Store(sm.MessageID, claimCh)
	defer c.pendingClaims.Delete(sm.MessageID)

	if err := c.sendClaim(sm.MessageID); err != nil {
		log.Printf("daemon: failed to send claim: %v", err)
		return
	}

	// Wait for claim ack with 5s timeout.
	select {
	case granted := <-claimCh:
		if !granted {
			log.Printf("daemon: claim denied for %s", sm.MessageID)
			return
		}
	case <-time.After(5 * time.Second):
		log.Printf("daemon: claim timeout for %s", sm.MessageID)
		return
	case <-ctx.Done():
		return
	}

	// Start heartbeat.
	heartbeatCtx, heartbeatCancel := context.WithCancel(ctx)
	c.activeMsgs.Store(sm.MessageID, heartbeatCancel)
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				_ = c.sendProgress(sm.MessageID)
			}
		}
	}()

	// Attach envelope messageID so downstream tools can reference it.
	payload.MessageID = sm.MessageID

	// Set active agent.
	agentName := payload.AgentName
	if agentName == "" {
		agentName = "(default)"
	}
	c.activeAgent.Store(agentName)

	// Run agent callback.
	result := c.onMsg(payload)

	// Cleanup.
	c.activeAgent.Store("")
	heartbeatCancel()
	c.activeMsgs.Delete(sm.MessageID)

	// Resolve the per-message reply override the onMsg callback may have set
	// during RunAgent. Suppress: this message was injected into another active
	// run that completes it under its own id — nothing to send here. ReplyToID:
	// the run absorbed an injected follow-up and answers it under that
	// follow-up's own cloud id, so the final reply+ack is addressed there
	// instead of the inbound id (separate channel messages, not one merged).
	replyID := sm.MessageID
	var ackIDs []string
	if v, ok := c.pendingReplies.LoadAndDelete(sm.MessageID); ok {
		pr := v.(pendingReply)
		if pr.Suppress {
			// Injected into an active run that completes this id (reply + ack)
			// once ITS reply is delivered. Skip BOTH here: acking now would drop
			// the inbound from Cloud's replay buffer before the owning run's reply
			// actually lands, losing the answer if that reply fails or the daemon
			// crashes first.
			return
		}
		if pr.ReplyToID != "" {
			replyID = pr.ReplyToID
		}
		// Ack every absorbed id, but only AFTER the reply below is delivered.
		ackIDs = pr.AckIDs
	}
	if len(ackIDs) == 0 {
		ackIDs = []string{replyID}
	}

	// Send reply, then ack on success so Cloud can drop the inbound
	// message from its replay buffer. Reply failure must skip the ack so
	// the un-delivered message is replayed on the next reconnect — the
	// user wasn't informed yet.
	if err := c.SendReply(replyID, ReplyPayload{
		Channel:  payload.Channel,
		ThreadID: payload.ThreadID,
		Text:     result,
		Format:   FormatText,
	}); err != nil {
		log.Printf("daemon: SendReply failed for message %s: %v", replyID, err)
		if c.eventBus != nil {
			// Match the source fallback applied at the WS callback entry point
			// (cmd/daemon.go) so consumers see a consistent source field during
			// Cloud rolling deploys where msg.Source may be empty.
			source := payload.Source
			if source == "" {
				source = payload.Channel
			}
			// Use the raw payload.AgentName (not the rewritten "(default)"
			// display value used by c.activeAgent) so consumers that route
			// on the agent identifier — including the desktop matcher that
			// expects "" / "default" for the default agent — see the wire
			// form rather than the local display string.
			errPayload, _ := json.Marshal(map[string]any{
				"agent":      payload.AgentName,
				"message_id": replyID,
				"source":     source,
				"error":      fmt.Sprintf("reply delivery failed: %v", err),
			})
			c.eventBus.Emit(Event{Type: EventAgentError, Payload: errPayload})
		}
		return
	}
	// Reply delivered — now ack every absorbed id (ack-after-delivery). The reply
	// failure path above returns early WITHOUT acking, so Cloud replays them all.
	for _, id := range ackIDs {
		if err := c.SendDeliveryAck(id); err != nil {
			log.Printf("daemon: delivery_ack failed for message %s: %v", id, err)
		}
	}
}

// RunWithReconnect connects to the server and reconnects on failure with
// exponential backoff. It blocks until the context is cancelled OR Cloud
// rejects the WS upgrade with HTTP 401 — in the latter case retrying is
// counterproductive (key is invalid; backoff would only burn attempts)
// so the loop exits and AuthManager.HandleWSAuthFailure (registered via
// SetOnAuthFailure) cleans up.
func (c *Client) RunWithReconnect(ctx context.Context) {
	backoff := time.Second
	maxBackoff := 30 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if err := c.Connect(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, ErrWSAuthRejected) {
				log.Printf("daemon: ws auth rejected by Cloud; stopping reconnect loop")
				return
			}
			log.Printf("daemon: connect failed: %v (retry in %v)", err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff = time.Second
		if err := c.Listen(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("daemon: connection lost: %v (reconnecting)", err)
		}
	}
}
