package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestRunWithReconnect_CancelledContextExitsImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := NewClient("ws://localhost:99999/nonexistent", "key", func(msg MessagePayload) string { return "" }, nil)

	done := make(chan struct{})
	go func() {
		client.RunWithReconnect(ctx)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunWithReconnect did not exit within 2s after cancel")
	}
}

func TestClient_SendEnvelope_WritesToConn(t *testing.T) {
	received := make(chan DaemonMessage, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var dm DaemonMessage
		if err := conn.ReadJSON(&dm); err != nil {
			return
		}
		received <- dm
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	c := NewClient(wsURL, "", nil, nil)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.sendEnvelope(DaemonMessage{Type: MsgTypeClaim, MessageID: "msg-123"}); err != nil {
		t.Fatal(err)
	}

	select {
	case dm := <-received:
		if dm.Type != MsgTypeClaim || dm.MessageID != "msg-123" {
			t.Errorf("got type=%q id=%q, want type=%q id=%q", dm.Type, dm.MessageID, MsgTypeClaim, "msg-123")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not receive message")
	}
}

// TestConnect_AdvertisesVersionAndUserAgent confirms every WS upgrade sends
// X-Kocoro-Daemon-Version and a User-Agent containing the version, OS, and
// arch. Cloud reads these headers for telemetry and version-bug fallback
// signals; without them, Cloud cannot tell which daemon build is connected.
func TestConnect_AdvertisesVersionAndUserAgent(t *testing.T) {
	prev := Version
	Version = "0.1.3-test"
	defer func() { Version = prev }()

	captured := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := r.Header.Clone()
		captured <- hdr
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	c := NewClient(wsURL, "", nil, nil)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	select {
	case hdr := <-captured:
		if got := hdr.Get("X-Kocoro-Daemon-Version"); got != "0.1.3-test" {
			t.Errorf("X-Kocoro-Daemon-Version = %q, want %q", got, "0.1.3-test")
		}
		ua := hdr.Get("User-Agent")
		if !strings.Contains(ua, "kocoro/0.1.3-test") {
			t.Errorf("User-Agent = %q, want to contain %q", ua, "kocoro/0.1.3-test")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not see upgrade request")
	}
}

// TestConnect_AdvertisesCapabilities confirms the X-Kocoro-Capabilities
// header carries every token in the package-level Capabilities slice and is
// omitted entirely when the slice is empty (so Cloud's "no header = legacy"
// invariant holds for daemons that haven't enabled any opt-in features).
func TestConnect_AdvertisesCapabilities(t *testing.T) {
	tests := []struct {
		name        string
		caps        []string
		wantHeader  string
		wantPresent bool
	}{
		{name: "empty slice omits header", caps: nil, wantHeader: "", wantPresent: false},
		{name: "single capability", caps: []string{"delivery_ack"}, wantHeader: "delivery_ack", wantPresent: true},
		{name: "multiple capabilities preserved in order", caps: []string{"delivery_ack", "future_feature"}, wantHeader: "delivery_ack,future_feature", wantPresent: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev := Capabilities
			Capabilities = tt.caps
			defer func() { Capabilities = prev }()

			captured := make(chan http.Header, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured <- r.Header.Clone()
				upgrader := websocket.Upgrader{}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
			}))
			defer srv.Close()

			wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
			c := NewClient(wsURL, "", nil, nil)
			if err := c.Connect(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer c.Close()

			select {
			case hdr := <-captured:
				_, present := hdr["X-Kocoro-Capabilities"]
				if present != tt.wantPresent {
					t.Errorf("header presence = %v, want %v", present, tt.wantPresent)
				}
				if got := hdr.Get("X-Kocoro-Capabilities"); got != tt.wantHeader {
					t.Errorf("X-Kocoro-Capabilities = %q, want %q", got, tt.wantHeader)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("server did not see upgrade request")
			}
		})
	}
}

func TestClient_ConnectionState(t *testing.T) {
	c := NewClient("ws://localhost:1/x", "", nil, nil)
	if c.IsConnected() {
		t.Error("should not be connected initially")
	}
	if c.Uptime() < 0 {
		t.Error("uptime should be non-negative")
	}
	if c.ActiveAgent() != "" {
		t.Error("no active agent initially")
	}
}

func TestClient_ClaimHandshake_Granted(t *testing.T) {
	var receivedClaim DaemonMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		// Send a message to the daemon.
		payload, _ := json.Marshal(MessagePayload{Channel: "slack", Text: "hi", ThreadID: "t1"})
		conn.WriteJSON(ServerMessage{Type: MsgTypeMessage, MessageID: "msg-001", Payload: payload})

		// Read the claim.
		conn.ReadJSON(&receivedClaim)

		// Grant the claim.
		ackPayload, _ := json.Marshal(ClaimAckPayload{Granted: true})
		conn.WriteJSON(ServerMessage{Type: MsgTypeClaimAck, MessageID: "msg-001", Payload: ackPayload})

		// Read messages until we get a reply (may get progress first).
		for {
			var reply DaemonMessage
			if err := conn.ReadJSON(&reply); err != nil {
				return
			}
			if reply.Type == MsgTypeReply {
				var rp ReplyPayload
				json.Unmarshal(reply.Payload, &rp)
				if rp.Text != "agent-result" {
					t.Errorf("reply text = %q, want %q", rp.Text, "agent-result")
				}
				return
			}
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	onMsgCalled := make(chan struct{})
	c := NewClient(wsURL, "", func(msg MessagePayload) string {
		close(onMsgCalled)
		return "agent-result"
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	go c.Listen(ctx)

	select {
	case <-onMsgCalled:
	case <-ctx.Done():
		t.Fatal("onMsg was never called")
	}

	if receivedClaim.Type != MsgTypeClaim || receivedClaim.MessageID != "msg-001" {
		t.Errorf("expected claim for msg-001, got %+v", receivedClaim)
	}
}

func TestClient_ClaimHandshake_Denied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		payload, _ := json.Marshal(MessagePayload{Channel: "slack", Text: "hi"})
		conn.WriteJSON(ServerMessage{Type: MsgTypeMessage, MessageID: "msg-002", Payload: payload})

		var dm DaemonMessage
		conn.ReadJSON(&dm)

		ackPayload, _ := json.Marshal(ClaimAckPayload{Granted: false})
		conn.WriteJSON(ServerMessage{Type: MsgTypeClaimAck, MessageID: "msg-002", Payload: ackPayload})

		// Keep connection open briefly so the client can process the denial.
		time.Sleep(500 * time.Millisecond)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	onMsgCalled := false
	c := NewClient(wsURL, "", func(msg MessagePayload) string {
		onMsgCalled = true
		return ""
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	go c.Listen(ctx)
	time.Sleep(500 * time.Millisecond)
	cancel()

	if onMsgCalled {
		t.Error("onMsg should NOT be called when claim is denied")
	}
}

func TestClient_GracefulDisconnect(t *testing.T) {
	msgs := make(chan DaemonMessage, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, _ := upgrader.Upgrade(w, r, nil)
		defer conn.Close()
		for {
			var dm DaemonMessage
			if err := conn.ReadJSON(&dm); err != nil {
				return
			}
			msgs <- dm
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	c := NewClient(wsURL, "", func(msg MessagePayload) string { return "" }, nil)

	ctx, cancel := context.WithCancel(context.Background())
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	go c.Listen(ctx)
	time.Sleep(100 * time.Millisecond)

	cancel()
	time.Sleep(200 * time.Millisecond)

	// Check if disconnect was the last message
	var lastMsg DaemonMessage
	for {
		select {
		case m := <-msgs:
			lastMsg = m
		default:
			goto done
		}
	}
done:
	if lastMsg.Type != MsgTypeDisconnect {
		t.Errorf("expected disconnect message, got type=%q", lastMsg.Type)
	}
}

func TestClient_SystemMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		payload := json.RawMessage(`"Quota warning: 90% used"`)
		conn.WriteJSON(ServerMessage{Type: MsgTypeSystem, Payload: payload})
		time.Sleep(500 * time.Millisecond)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	systemCh := make(chan string, 1)
	c := NewClient(wsURL, "", func(msg MessagePayload) string { return "" }, func(text string) {
		systemCh <- text
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	go c.Listen(ctx)

	select {
	case msg := <-systemCh:
		if msg != "Quota warning: 90% used" {
			t.Errorf("system message = %q, want %q", msg, "Quota warning: 90% used")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("system message not received")
	}
}

func TestClient_SendEvent_WireFormat(t *testing.T) {
	received := make(chan DaemonMessage, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var dm DaemonMessage
		if err := conn.ReadJSON(&dm); err != nil {
			return
		}
		received <- dm
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	c := NewClient(wsURL, "", nil, nil)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.SendEvent("msg-100", "tool_start", "running web_search", map[string]interface{}{"tool": "web_search"}); err != nil {
		t.Fatal(err)
	}

	select {
	case dm := <-received:
		if dm.Type != MsgTypeEvent {
			t.Errorf("type = %q, want %q", dm.Type, MsgTypeEvent)
		}
		if dm.MessageID != "msg-100" {
			t.Errorf("message_id = %q, want %q", dm.MessageID, "msg-100")
		}
		var ep DaemonEventPayload
		if err := json.Unmarshal(dm.Payload, &ep); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if ep.EventType != "tool_start" {
			t.Errorf("event_type = %q, want %q", ep.EventType, "tool_start")
		}
		if ep.Seq != 1 {
			t.Errorf("seq = %d, want 1", ep.Seq)
		}
		if ep.Timestamp == "" {
			t.Error("timestamp should not be empty")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not receive event")
	}
}

func TestClient_SendEvent_SeqIncrementsPerMessage(t *testing.T) {
	received := make(chan DaemonMessage, 3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for i := 0; i < 3; i++ {
			var dm DaemonMessage
			if err := conn.ReadJSON(&dm); err != nil {
				return
			}
			received <- dm
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	c := NewClient(wsURL, "", nil, nil)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Send 2 events for msg-A, 1 for msg-B.
	c.SendEvent("msg-A", "step", "one", nil)
	c.SendEvent("msg-A", "step", "two", nil)
	c.SendEvent("msg-B", "step", "one", nil)

	seqs := make(map[string][]int64)
	for i := 0; i < 3; i++ {
		select {
		case dm := <-received:
			var ep DaemonEventPayload
			json.Unmarshal(dm.Payload, &ep)
			seqs[dm.MessageID] = append(seqs[dm.MessageID], ep.Seq)
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for events")
		}
	}

	if got := seqs["msg-A"]; len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("msg-A seqs = %v, want [1 2]", got)
	}
	if got := seqs["msg-B"]; len(got) != 1 || got[0] != 1 {
		t.Errorf("msg-B seqs = %v, want [1]", got)
	}
}

func TestClient_SendReply_CleansUpSeq(t *testing.T) {
	c := NewClient("ws://localhost:1/x", "", nil, nil)
	// Pre-populate a seq counter.
	c.eventSeqs.Store("msg-cleanup", new(atomic.Int64))

	// SendReply will fail (no connection) but should still clean up eventSeqs.
	_ = c.SendReply("msg-cleanup", ReplyPayload{Text: "done"})

	if _, loaded := c.eventSeqs.Load("msg-cleanup"); loaded {
		t.Error("eventSeqs entry should have been deleted by SendReply")
	}
}

// Locks down the cross-repo contract with shannon-cloud: approval_request must
// carry the inbound claim's WS envelope MessageID. Cloud reads it from the
// envelope (not the payload) to look up channel/thread context for the
// approval card. Empty MessageID triggers Cloud's fail-closed drop.
func TestClient_SendApprovalRequest_PutsMessageIDOnEnvelope(t *testing.T) {
	received := make(chan DaemonMessage, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var dm DaemonMessage
		if err := conn.ReadJSON(&dm); err != nil {
			return
		}
		received <- dm
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	c := NewClient(wsURL, "", nil, nil)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	req := ApprovalRequest{
		MessageID: "claim-msg-42",
		Channel:   "slack",
		ThreadID:  "C123-1700000000.000050",
		RequestID: "apr_test",
		Tool:      "bash",
		Args:      `{"command":"ls"}`,
		Agent:     "test-agent",
	}
	if err := c.SendApprovalRequest(req); err != nil {
		t.Fatal(err)
	}

	select {
	case dm := <-received:
		if dm.Type != MsgTypeApprovalRequest {
			t.Errorf("envelope Type = %q, want %q", dm.Type, MsgTypeApprovalRequest)
		}
		if dm.MessageID != "claim-msg-42" {
			t.Errorf("envelope MessageID = %q, want %q (Cloud will fail-closed without this)",
				dm.MessageID, "claim-msg-42")
		}
		// Confirm MessageID is NOT also serialized into the payload — that
		// would suggest the field accidentally lost its `json:"-"` tag.
		if strings.Contains(string(dm.Payload), "claim-msg-42") {
			t.Errorf("payload should not echo MessageID; got: %s", string(dm.Payload))
		}
		// Sanity-check the payload still carries what Cloud expects.
		var payload ApprovalRequest
		if err := json.Unmarshal(dm.Payload, &payload); err != nil {
			t.Fatalf("payload not valid ApprovalRequest JSON: %v", err)
		}
		if payload.RequestID != "apr_test" || payload.Tool != "bash" || payload.Agent != "test-agent" {
			t.Errorf("unexpected payload contents: %+v", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not receive approval_request envelope")
	}
}

// TestCapabilities_AdvertisesDeliveryAck guards the contract the cloud
// agent's tracker depends on: the daemon must announce "delivery_ack"
// in its handshake whenever it ships ack-emission code. Drop this token
// only by also removing the sendDeliveryAck call site — otherwise Cloud
// stops tracking but the daemon keeps emitting acks Cloud will warn on.
func TestCapabilities_AdvertisesDeliveryAck(t *testing.T) {
	found := false
	for _, c := range Capabilities {
		if c == "delivery_ack" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("default Capabilities = %v, want to contain %q", Capabilities, "delivery_ack")
	}
}

// TestSendDeliveryAck_EmptyMessageIDIsNoOp confirms a missing inbound
// MessageID short-circuits before sendEnvelope. The wire protocol
// requires non-empty MessageID for delivery_ack (Cloud warns and drops
// otherwise), so silently no-op'ing is the right behavior — the caller
// path inside handleMessage always has a MessageID, but defensive
// callers shouldn't have to guard.
func TestSendDeliveryAck_EmptyMessageIDIsNoOp(t *testing.T) {
	c := NewClient("ws://localhost:1/x", "", nil, nil)
	if err := c.sendDeliveryAck(""); err != nil {
		t.Errorf("sendDeliveryAck(\"\") = %v, want nil (no-op)", err)
	}
}

// TestClient_DeliveryAck_SentAfterReplySuccess confirms the daemon emits
// delivery_ack with the inbound MessageID after a successful SendReply.
// This is the contract the cloud agent's 5-min replay buffer relies on —
// without the ack, Cloud replays on reconnect and the user gets a
// duplicate response.
func TestClient_DeliveryAck_SentAfterReplySuccess(t *testing.T) {
	gotAck := make(chan DaemonMessage, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		// Send a message to the daemon.
		payload, _ := json.Marshal(MessagePayload{Channel: "slack", Text: "hi"})
		conn.WriteJSON(ServerMessage{Type: MsgTypeMessage, MessageID: "msg-ack-1", Payload: payload})

		// Read the claim and grant it.
		var dm DaemonMessage
		conn.ReadJSON(&dm)
		ackPayload, _ := json.Marshal(ClaimAckPayload{Granted: true})
		conn.WriteJSON(ServerMessage{Type: MsgTypeClaimAck, MessageID: "msg-ack-1", Payload: ackPayload})

		// Walk daemon-emitted frames until we see the delivery_ack. Reply
		// MUST come first (ack-on-success is the contract); progress
		// frames may interleave from the heartbeat.
		sawReply := false
		for {
			var frame DaemonMessage
			if err := conn.ReadJSON(&frame); err != nil {
				return
			}
			switch frame.Type {
			case MsgTypeReply:
				sawReply = true
			case MsgTypeDeliveryAck:
				if !sawReply {
					t.Errorf("delivery_ack arrived before reply")
				}
				gotAck <- frame
				return
			}
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	c := NewClient(wsURL, "", func(msg MessagePayload) string {
		return "agent-result"
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	go c.Listen(ctx)

	select {
	case ack := <-gotAck:
		if ack.MessageID != "msg-ack-1" {
			t.Errorf("ack.MessageID = %q, want %q", ack.MessageID, "msg-ack-1")
		}
		if len(ack.Payload) != 0 {
			t.Errorf("ack.Payload = %q, want empty (Cloud parses message_id from envelope)", string(ack.Payload))
		}
	case <-ctx.Done():
		t.Fatal("daemon never sent delivery_ack")
	}
}
