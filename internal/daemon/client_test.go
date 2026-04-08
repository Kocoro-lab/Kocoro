package daemon

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestClient_Dedup_DropsDuplicateMessage(t *testing.T) {
	var callCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		payload, _ := json.Marshal(MessagePayload{Channel: "slack", Text: "hello", ThreadID: "t1"})

		// Send the same message_id twice.
		for i := 0; i < 2; i++ {
			conn.WriteJSON(ServerMessage{Type: MsgTypeMessage, MessageID: "dup-001", Payload: payload})

			// Read the claim (only first message should send one).
			var dm DaemonMessage
			conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			if err := conn.ReadJSON(&dm); err != nil {
				// Second iteration: no claim expected (deduped), timeout is fine.
				continue
			}

			if dm.Type == MsgTypeClaim {
				// Grant the claim.
				ackPayload, _ := json.Marshal(ClaimAckPayload{Granted: true})
				conn.WriteJSON(ServerMessage{Type: MsgTypeClaimAck, MessageID: "dup-001", Payload: ackPayload})

				// Drain until reply.
				for {
					var reply DaemonMessage
					conn.SetReadDeadline(time.Now().Add(2 * time.Second))
					if err := conn.ReadJSON(&reply); err != nil {
						return
					}
					if reply.Type == MsgTypeReply {
						return
					}
				}
			}
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	c := NewClient(wsURL, "", func(msg MessagePayload) string {
		callCount.Add(1)
		return fmt.Sprintf("result-%d", callCount.Load())
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	go c.Listen(ctx)

	// Wait enough time for both messages to be processed.
	time.Sleep(1 * time.Second)
	cancel()

	if got := callCount.Load(); got != 1 {
		t.Errorf("onMsg called %d times, want 1 (duplicate should be deduped)", got)
	}
}

func TestClient_Dedup_AllowsNewMessageID(t *testing.T) {
	var callCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		for i := 1; i <= 2; i++ {
			msgID := fmt.Sprintf("unique-%03d", i)
			payload, _ := json.Marshal(MessagePayload{Channel: "slack", Text: fmt.Sprintf("msg%d", i)})

			conn.WriteJSON(ServerMessage{Type: MsgTypeMessage, MessageID: msgID, Payload: payload})

			// Read claim.
			var dm DaemonMessage
			conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			if err := conn.ReadJSON(&dm); err != nil {
				return
			}

			if dm.Type != MsgTypeClaim {
				t.Errorf("expected claim, got %s", dm.Type)
				return
			}

			// Grant.
			ackPayload, _ := json.Marshal(ClaimAckPayload{Granted: true})
			conn.WriteJSON(ServerMessage{Type: MsgTypeClaimAck, MessageID: msgID, Payload: ackPayload})

			// Drain until reply.
			for {
				var reply DaemonMessage
				conn.SetReadDeadline(time.Now().Add(3 * time.Second))
				if err := conn.ReadJSON(&reply); err != nil {
					return
				}
				if reply.Type == MsgTypeReply {
					break
				}
			}
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	c := NewClient(wsURL, "", func(msg MessagePayload) string {
		callCount.Add(1)
		return "ok"
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	go c.Listen(ctx)

	// Wait for both messages to complete.
	time.Sleep(2 * time.Second)

	if got := callCount.Load(); got != 2 {
		t.Errorf("onMsg called %d times, want 2 (two distinct message IDs)", got)
	}
}

func TestClient_Dedup_SweeperCleansExpired(t *testing.T) {
	c := NewClient("ws://localhost:1/x", "", nil, nil)

	// Insert an expired "done" entry (timestamp well beyond TTL).
	c.processedMsgs.Store("expired-msg", processedEntry{
		status:    "done",
		timestamp: time.Now().Add(-(dedupTTL + time.Minute)),
	})

	// Insert a fresh "done" entry (within TTL).
	c.processedMsgs.Store("fresh-msg", processedEntry{
		status:    "done",
		timestamp: time.Now(),
	})

	c.sweepProcessedMsgs()

	if _, ok := c.processedMsgs.Load("expired-msg"); ok {
		t.Error("expired entry should have been removed by sweeper")
	}
	if _, ok := c.processedMsgs.Load("fresh-msg"); !ok {
		t.Error("fresh entry should have been retained by sweeper")
	}
}
