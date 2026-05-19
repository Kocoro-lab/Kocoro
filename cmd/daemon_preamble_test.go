package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/daemon"
)

func TestEmitInjectedMessageReceivedEventIncludesActiveSessionAndMessageID(t *testing.T) {
	bus := daemon.NewEventBus()
	ch := bus.Subscribe()
	defer bus.Unsubscribe(ch)

	cache := daemon.NewSessionCache(t.TempDir())
	defer cache.CloseAll()

	routeKey, ok := cache.RegisterAdHocSessionRoute("sess-123", nil, make(chan struct{}), make(chan agent.InjectedMessage, 1), "")
	if !ok {
		t.Fatal("failed to register active session route")
	}
	defer cache.UnregisterAdHocSessionRoute(routeKey)

	emitInjectedMessageReceivedEvent(bus, cache, daemon.RunAgentRequest{
		Agent:    "default",
		Source:   "slack",
		Sender:   "U123",
		Text:     "follow up",
		RouteKey: routeKey,
	}, "msg-456")

	select {
	case evt := <-ch:
		if evt.Type != daemon.EventMessageReceived {
			t.Fatalf("event type = %q, want %q", evt.Type, daemon.EventMessageReceived)
		}
		var payload struct {
			Agent     string `json:"agent"`
			Source    string `json:"source"`
			Sender    string `json:"sender"`
			SessionID string `json:"session_id"`
			MessageID string `json:"message_id"`
			Text      string `json:"text"`
			Queued    bool   `json:"queued"`
		}
		if err := json.Unmarshal(evt.Payload, &payload); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if payload.Agent != "default" || payload.Source != "slack" || payload.Sender != "U123" ||
			payload.SessionID != "sess-123" || payload.MessageID != "msg-456" || payload.Text != "follow up" || !payload.Queued {
			t.Fatalf("unexpected payload: %+v", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for injected message_received event")
	}
}

func TestDaemonEventHandler_OnPreamble_DropsEmptyText(t *testing.T) {
	received := make(chan daemon.DaemonMessage, 2)
	srv := httptestNewWebSocketServer(t, received)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	client := daemon.NewClient(wsURL, "", nil, nil)
	if err := client.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	handler := &daemonEventHandler{wsClient: client, messageID: "msg-123"}
	handler.OnPreamble("")

	select {
	case dm := <-received:
		t.Fatalf("empty preamble should not be forwarded, got message type=%q payload=%s", dm.Type, string(dm.Payload))
	case <-time.After(100 * time.Millisecond):
	}

	handler.OnPreamble("Reading the four files.")

	select {
	case dm := <-received:
		if dm.Type != daemon.MsgTypeEvent || dm.MessageID != "msg-123" {
			t.Fatalf("unexpected daemon message: %+v", dm)
		}
		var payload daemon.DaemonEventPayload
		if err := json.Unmarshal(dm.Payload, &payload); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if payload.EventType != "LLM_OUTPUT" || payload.Message != "Reading the four files." {
			t.Fatalf("unexpected payload: %+v", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not receive non-empty preamble")
	}
}

// TestDaemonEventHandler_AutoApproveAllowsAllTools pins the 2026-05-18
// policy: with daemon.auto_approve=true, every tool is auto-approved with
// no broker round-trip (unattended deny-list is empty). Previously
// publish_to_web / generate_image / edit_image still prompted via the
// broker; the product call moved them off the gate. The plumbing remains
// so a future tool can re-occupy the slot.
func TestDaemonEventHandler_AutoApproveAllowsAllTools(t *testing.T) {
	for _, tool := range []string{
		"publish_to_web", "generate_image", "edit_image",
		"bash", "file_write",
	} {
		t.Run(tool, func(t *testing.T) {
			brokerCalled := false
			broker := daemon.NewApprovalBroker(func(req daemon.ApprovalRequest) error {
				brokerCalled = true
				return nil
			})
			handler := &daemonEventHandler{
				broker:      broker,
				ctx:         context.Background(),
				messageID:   "msg-123",
				channel:     "wecom",
				threadID:    "thread-1",
				agent:       "Default",
				autoApprove: true,
			}

			if !handler.OnApprovalNeeded(tool, `{"path":"report.html"}`) {
				t.Fatalf("auto_approve=true should auto-approve %s without prompting", tool)
			}
			if brokerCalled {
				t.Fatalf("%s was auto-approved but broker was still invoked", tool)
			}
		})
	}
}

func TestDaemonEventHandler_AutoApproveSkipsBrokerWhenNotPerCallOnly(t *testing.T) {
	brokerCalled := false
	broker := daemon.NewApprovalBroker(func(req daemon.ApprovalRequest) error {
		brokerCalled = true
		return nil
	})
	handler := &daemonEventHandler{
		broker:      broker,
		ctx:         context.Background(),
		autoApprove: true,
	}

	if !handler.OnApprovalNeeded("file_read", `{"path":"notes.txt"}`) {
		t.Fatal("non-per-call tool should still be auto-approved")
	}
	if brokerCalled {
		t.Fatal("non-per-call auto-approved tool should not prompt via broker")
	}
}

func httptestNewWebSocketServer(t *testing.T, received chan<- daemon.DaemonMessage) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var dm daemon.DaemonMessage
			if err := conn.ReadJSON(&dm); err != nil {
				return
			}
			received <- dm
		}
	}))
}
