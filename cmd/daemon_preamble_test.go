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

	"github.com/Kocoro-lab/ShanClaw/internal/daemon"
)

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

func TestDaemonEventHandler_AutoApprovePromptsForPerCallTool(t *testing.T) {
	for _, tool := range []string{"publish_to_web", "generate_image", "edit_image"} {
		t.Run(tool, func(t *testing.T) {
			reqCh := make(chan daemon.ApprovalRequest, 1)
			var broker *daemon.ApprovalBroker
			broker = daemon.NewApprovalBroker(func(req daemon.ApprovalRequest) error {
				reqCh <- req
				go broker.Resolve(req.RequestID, daemon.DecisionAllow)
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
				t.Fatalf("per-call approval tool %s should prompt via broker and allow when user allows", tool)
			}
			select {
			case req := <-reqCh:
				if req.Tool != tool || req.MessageID != "msg-123" || req.Agent != "Default" {
					t.Fatalf("unexpected approval request: %+v", req)
				}
			case <-time.After(time.Second):
				t.Fatalf("approval broker was not called for %s", tool)
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
