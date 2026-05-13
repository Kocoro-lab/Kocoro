package daemon

import (
	"context"
	"encoding/json"
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
const (
	CapDeliveryAck         = "delivery_ack"
	CapInlineDocumentB64   = "inline_document_b64"
	CapInlineExtractedText = "inline_extracted_text"
)

var Capabilities = []string{
	CapDeliveryAck,
	CapInlineDocumentB64,
	CapInlineExtractedText,
}

type Client struct {
	endpoint      string
	apiKey        string
	conn          *websocket.Conn
	writeMu       sync.Mutex
	onMsg         func(MessagePayload) string // returns reply text
	onSystem      func(string)                // system notifications
	sem           chan struct{}
	pendingClaims sync.Map   // map[string]chan bool
	activeMsgs    sync.Map   // map[string]context.CancelFunc
	eventSeqs     sync.Map   // map[string]*atomic.Int64
	connected     atomic.Bool
	activeAgent   atomic.Value // stores string
	startTime     time.Time
	broker        *ApprovalBroker
	eventBus      *EventBus
}

// SetEventBus sets the event bus for emitting daemon events.
func (c *Client) SetEventBus(bus *EventBus) {
	c.eventBus = bus
}

func NewClient(endpoint, apiKey string, onMsg func(MessagePayload) string, onSystem func(string)) *Client {
	return &Client{
		endpoint:  endpoint,
		apiKey:    apiKey,
		onMsg:     onMsg,
		onSystem:  onSystem,
		sem:       make(chan struct{}, MaxConcurrentAgents),
		startTime: time.Now(),
	}
}

func (c *Client) Connect(ctx context.Context) error {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+c.apiKey)
	header.Set("User-Agent", fmt.Sprintf("kocoro/%s (%s; %s)", Version, runtime.GOOS, runtime.GOARCH))
	header.Set("X-Kocoro-Daemon-Version", Version)
	if len(Capabilities) > 0 {
		header.Set("X-Kocoro-Capabilities", strings.Join(Capabilities, ","))
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}
	conn, _, err := dialer.DialContext(ctx, c.endpoint, header)
	if err != nil {
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
	return c.sendEnvelope(DaemonMessage{Type: MsgTypeClaim, MessageID: messageID})
}

func (c *Client) sendProgress(messageID string) error {
	return c.sendEnvelope(DaemonMessage{Type: MsgTypeProgress, MessageID: messageID})
}

// sendDeliveryAck signals to Cloud that the inbound message reached a
// terminal state (success or error reply already delivered to the
// user). Cloud drops the entry from its replay buffer so a subsequent
// disconnect+reconnect doesn't re-deliver the same message. Called
// only on SendReply success — if the reply itself failed to flush,
// the user wasn't informed and Cloud must replay on reconnect.
//
// Empty messageID is a no-op so callers don't have to guard.
func (c *Client) sendDeliveryAck(messageID string) error {
	if messageID == "" {
		return nil
	}
	return c.sendEnvelope(DaemonMessage{Type: MsgTypeDeliveryAck, MessageID: messageID})
}

// SendProgressWithWorkflow sends a progress heartbeat with a workflow_id payload.
// This tells Cloud to start streaming card replies for the originating channel.
func (c *Client) SendProgressWithWorkflow(messageID, workflowID string) error {
	payload, _ := json.Marshal(map[string]string{"workflow_id": workflowID})
	return c.sendEnvelope(DaemonMessage{Type: MsgTypeProgress, MessageID: messageID, Payload: payload})
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
	return c.sendEnvelope(DaemonMessage{
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
	return c.sendEnvelope(DaemonMessage{Type: MsgTypeReply, MessageID: messageID, Payload: payloadBytes})
}

// SendProactive sends an unsolicited message to all channels mapped to the agent.
// This is fire-and-forget — no claim/ack cycle.
func (c *Client) SendProactive(agentName, text, sessionID string) error {
	if agentName == "" || text == "" {
		return nil
	}
	payload, err := json.Marshal(ProactivePayload{
		AgentName: agentName,
		Text:      text,
		Format:    FormatText,
		SessionID: sessionID,
	})
	if err != nil {
		return fmt.Errorf("marshal proactive payload: %w", err)
	}
	return c.sendEnvelope(DaemonMessage{
		Type:    MsgTypeProactive,
		Payload: payload,
	})
}

func (c *Client) sendDisconnect() error {
	return c.sendEnvelope(DaemonMessage{Type: MsgTypeDisconnect})
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
	return c.sendEnvelope(DaemonMessage{
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
	return c.sendEnvelope(DaemonMessage{
		Type:    MsgTypeApprovalResolved,
		Payload: payload,
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
			// Emit before Resolve so Ptfrog dismisses the card before seeing the reply.
			resolvedBy := resp.ResolvedBy
			if resolvedBy == "" {
				resolvedBy = "external"
			}
			if c.eventBus != nil {
				payload, _ := json.Marshal(map[string]string{
					"request_id":  resp.RequestID,
					"decision":    string(resp.Decision),
					"resolved_by": resolvedBy,
				})
				c.eventBus.Emit(Event{Type: EventApprovalResolved, Payload: payload})
			}
			if c.broker != nil {
				c.broker.Resolve(resp.RequestID, resp.Decision)
			}
		case MsgTypeSystem:
			if c.onSystem != nil {
				var text string
				if err := json.Unmarshal(sm.Payload, &text); err == nil {
					c.onSystem(text)
				}
			}
		default:
			log.Printf("daemon: unknown message type: %s", sm.Type)
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

	// Send reply, then ack on success so Cloud can drop the inbound
	// message from its replay buffer. Reply failure must skip the ack so
	// the un-delivered message is replayed on the next reconnect — the
	// user wasn't informed yet.
	if err := c.SendReply(sm.MessageID, ReplyPayload{
		Channel:  payload.Channel,
		ThreadID: payload.ThreadID,
		Text:     result,
		Format:   FormatText,
	}); err != nil {
		log.Printf("daemon: SendReply failed for message %s: %v", sm.MessageID, err)
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
				"message_id": sm.MessageID,
				"source":     source,
				"error":      fmt.Sprintf("reply delivery failed: %v", err),
			})
			c.eventBus.Emit(Event{Type: EventAgentError, Payload: errPayload})
		}
		return
	}
	if err := c.sendDeliveryAck(sm.MessageID); err != nil {
		log.Printf("daemon: delivery_ack failed for message %s: %v", sm.MessageID, err)
	}
}

// RunWithReconnect connects to the server and reconnects on failure with
// exponential backoff. It blocks until the context is cancelled.
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
