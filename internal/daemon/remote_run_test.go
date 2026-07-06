package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

func TestRemotePairingErrorExplainsCloudTimeout(t *testing.T) {
	status, message := remotePairingError(context.DeadlineExceeded)
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", status, http.StatusBadGateway)
	}
	if message == context.DeadlineExceeded.Error() {
		t.Fatalf("message should explain the Cloud pairing timeout, got %q", message)
	}
}

func TestRemotePairingCodeRequiresLocalPresence(t *testing.T) {
	t.Setenv(localPresenceEnv, "secret")
	srv := NewServer(0, &Client{}, nil, "test")
	req := httptest.NewRequest(http.MethodPost, "/remote/pairing-code", nil)
	rr := httptest.NewRecorder()

	srv.handleRemotePairingCode(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 body=%s", rr.Code, rr.Body.String())
	}
}

func TestRemotePairingCodeAcceptsLocalPresence(t *testing.T) {
	t.Setenv(localPresenceEnv, "secret")
	client := &Client{}
	client.envelopeSender = func(dm DaemonMessage) error {
		resp := PairingCodeResponse{Code: "123456", ExpiresAt: "2026-07-06T00:00:00Z"}
		if ch, ok := client.pendingPairingCodes.Load(dm.MessageID); ok {
			ch.(chan PairingCodeResponse) <- resp
		}
		return nil
	}
	srv := NewServer(0, client, nil, "test")
	req := httptest.NewRequest(http.MethodPost, "/remote/pairing-code", nil)
	req.Header.Set(localPresenceHeader, "secret")
	rr := httptest.NewRecorder()

	srv.handleRemotePairingCode(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", rr.Code, rr.Body.String())
	}
	var resp PairingCodeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Code != "123456" {
		t.Fatalf("code = %q, want 123456", resp.Code)
	}
}

func TestRemotePairingsRequireLocalPresence(t *testing.T) {
	t.Setenv(localPresenceEnv, "secret")
	srv := NewServer(0, &Client{}, nil, "test")
	req := httptest.NewRequest(http.MethodGet, "/remote/pairings", nil)
	rr := httptest.NewRecorder()

	srv.handleRemotePairings(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 body=%s", rr.Code, rr.Body.String())
	}
}

func TestRemoteRevokeRequiresLocalPresence(t *testing.T) {
	t.Setenv(localPresenceEnv, "secret")
	srv := NewServer(0, &Client{}, nil, "test")
	req := httptest.NewRequest(http.MethodPost, "/remote/revoke", nil)
	rr := httptest.NewRecorder()

	srv.handleRemoteRevoke(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 body=%s", rr.Code, rr.Body.String())
	}
}

func TestRemotePairingErrorPreservesCloudError(t *testing.T) {
	status, message := remotePairingError(errors.New("remote pairing unavailable"))
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", status, http.StatusBadGateway)
	}
	if message != "remote pairing unavailable" {
		t.Fatalf("message = %q", message)
	}
}

func TestRemoteRunEventHandlerAutoApproveSkipsBroker(t *testing.T) {
	brokerCalled := false
	broker := NewApprovalBroker(func(req ApprovalRequest) error {
		brokerCalled = true
		return nil
	})
	handler := &remoteRunEventHandler{
		broker:      broker,
		ctx:         context.Background(),
		autoApprove: true,
	}

	if !handler.OnApprovalNeeded("file_write", `{"path":"notes.txt"}`) {
		t.Fatal("autoApprove=true should approve without prompting")
	}
	if brokerCalled {
		t.Fatal("auto-approved remote run still invoked the approval broker")
	}
}

// TestRemoteRunAutoApproveEmitsObservableNotice locks in the visibility fix:
// when a remote run auto-approves a tool (auto_approve=true), the broker is
// bypassed so no approval_requested reaches the controller. The daemon must
// instead emit an approval_auto notice so the unattended execution is visible
// on the phone / in the replay buffer rather than only in the local log.
func TestRemoteRunAutoApproveEmitsObservableNotice(t *testing.T) {
	got := make(chan DaemonMessage, 1)
	client := &Client{
		envelopeSender: func(msg DaemonMessage) error {
			got <- msg
			return nil
		},
	}
	srv := NewServer(0, client, &ServerDeps{}, "test")
	handler := &remoteRunEventHandler{
		server:      srv,
		runID:       "run-auto",
		sessionID:   "sess-auto",
		ctx:         context.Background(),
		autoApprove: true,
	}

	if !handler.OnApprovalNeeded("bash", `{"command":"ls"}`) {
		t.Fatal("autoApprove=true should approve")
	}

	select {
	case dm := <-got:
		if dm.Type != MsgTypeRemoteRunEvent {
			t.Fatalf("message type = %q, want %q", dm.Type, MsgTypeRemoteRunEvent)
		}
		var evt RemoteRunEvent
		if err := json.Unmarshal(dm.Payload, &evt); err != nil {
			t.Fatalf("invalid remote run event: %v", err)
		}
		if evt.Type != "approval_auto" {
			t.Fatalf("event type = %q, want approval_auto", evt.Type)
		}
		if evt.RunID != "run-auto" || evt.SessionID != "sess-auto" {
			t.Fatalf("event ids = (%q,%q), want (run-auto,sess-auto)", evt.RunID, evt.SessionID)
		}
		if !strings.Contains(string(evt.Payload), `"tool":"bash"`) {
			t.Fatalf("payload = %s, want tool=bash", evt.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("auto-approved remote run emitted no approval_auto notice")
	}
}

func TestRemoteRunRejectsWhenConcurrencyLimitReached(t *testing.T) {
	got := make(chan DaemonMessage, 1)
	client := &Client{
		envelopeSender: func(msg DaemonMessage) error {
			got <- msg
			return nil
		},
	}
	srv := NewServer(0, client, &ServerDeps{}, "test")
	srv.remoteRunSlots = make(chan struct{}, 1)
	srv.remoteRunSlots <- struct{}{}

	srv.HandleRemoteRunRequest(context.Background(), RemoteRunRequest{
		RunID: "run-1",
		Text:  "hello",
	})

	select {
	case dm := <-got:
		if dm.Type != MsgTypeRemoteRunEvent {
			t.Fatalf("message type = %q, want %q", dm.Type, MsgTypeRemoteRunEvent)
		}
		var evt RemoteRunEvent
		if err := json.Unmarshal(dm.Payload, &evt); err != nil {
			t.Fatalf("invalid remote run event: %v", err)
		}
		if evt.RunID != "run-1" || evt.Type != "error" {
			t.Fatalf("event = %#v, want run-1 error", evt)
		}
		if !strings.Contains(string(evt.Payload), "too many remote runs") {
			t.Fatalf("payload = %s, want concurrency error", evt.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("remote run rejection event was not sent")
	}
	if _, ok := srv.remoteRuns.Load("run-1"); ok {
		t.Fatal("rejected run should not remain in remoteRuns")
	}
}

func TestRemoteRunEventOmitsOversizedDoneReply(t *testing.T) {
	got := make(chan RemoteRunEvent, 1)
	client := &Client{
		envelopeSender: func(dm DaemonMessage) error {
			if len(dm.Payload) > maxRemoteRunEventBytes {
				t.Fatalf("payload size = %d, want <= %d", len(dm.Payload), maxRemoteRunEventBytes)
			}
			var evt RemoteRunEvent
			if err := json.Unmarshal(dm.Payload, &evt); err != nil {
				return err
			}
			got <- evt
			return nil
		},
	}
	srv := NewServer(0, client, &ServerDeps{}, "test")
	result := RunAgentResult{
		Reply:     strings.Repeat("x", maxRemoteRunEventBytes),
		SessionID: "sess-1",
		Agent:     "default",
		Usage:     RunAgentUsage{TotalTokens: 42},
	}

	if err := srv.sendRemoteRunEvent(RemoteRunEvent{
		RunID:     "run-big",
		Type:      "done",
		SessionID: result.SessionID,
		Payload:   rawJSON(result),
	}); err != nil {
		t.Fatalf("sendRemoteRunEvent: %v", err)
	}

	select {
	case evt := <-got:
		if evt.Type != "done" || evt.RunID != "run-big" || evt.Seq != 1 {
			t.Fatalf("event = %+v", evt)
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(evt.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if _, ok := payload["reply"]; ok {
			t.Fatalf("oversized done payload still contains reply")
		}
		if string(payload["reply_omitted"]) != "true" {
			t.Fatalf("reply_omitted = %s, want true", payload["reply_omitted"])
		}
		if string(payload["session_id"]) != `"sess-1"` {
			t.Fatalf("session_id payload = %s", payload["session_id"])
		}
	case <-time.After(time.Second):
		t.Fatal("remote run event was not sent")
	}
}

func TestRemoteRunEventReplayAfterSendFailure(t *testing.T) {
	failing := true
	got := make(chan RemoteRunEvent, 1)
	client := &Client{
		envelopeSender: func(dm DaemonMessage) error {
			if failing {
				return errors.New("websocket down")
			}
			var evt RemoteRunEvent
			if err := json.Unmarshal(dm.Payload, &evt); err != nil {
				return err
			}
			got <- evt
			return nil
		},
	}
	srv := NewServer(0, client, &ServerDeps{}, "test")

	if err := srv.sendRemoteRunEvent(RemoteRunEvent{RunID: "run-1", Type: "run_started"}); err == nil {
		t.Fatal("expected initial send to fail")
	}
	failing = false
	srv.ReplayRemoteRunEvents()

	select {
	case evt := <-got:
		if evt.RunID != "run-1" || evt.Type != "run_started" || evt.Seq != 1 {
			t.Fatalf("replayed event = %+v", evt)
		}
	case <-time.After(time.Second):
		t.Fatal("remote run event was not replayed")
	}
}

func TestRemoteRunInjectsIntoActiveSessionInsteadOfStartingNewRun(t *testing.T) {
	got := make(chan DaemonMessage, 1)
	client := &Client{
		envelopeSender: func(msg DaemonMessage) error {
			got <- msg
			return nil
		},
	}
	injected := make(chan agent.InjectedMessage, 1)
	sc := NewSessionCache(t.TempDir())
	sc.routes["session:sess-live"] = &routeEntry{
		injectCh: injected,
		done:     make(chan struct{}),
	}
	deps := &ServerDeps{SessionCache: sc, ShannonDir: t.TempDir(), AgentsDir: t.TempDir()}
	srv := NewServer(0, client, deps, "test")

	srv.HandleRemoteRunRequest(context.Background(), RemoteRunRequest{
		RunID:           "run-inject",
		Text:            "follow up",
		SessionID:       "sess-live",
		ClientMessageID: "ios-msg-1",
	})

	select {
	case msg := <-injected:
		if msg.Text != "follow up" {
			t.Fatalf("injected text = %q", msg.Text)
		}
		if msg.ClientMessageID != "ios-msg-1" {
			t.Fatalf("client message id = %q", msg.ClientMessageID)
		}
	case <-time.After(time.Second):
		t.Fatal("remote run was not injected into active session")
	}

	select {
	case dm := <-got:
		if dm.Type != MsgTypeRemoteRunEvent {
			t.Fatalf("message type = %q, want %q", dm.Type, MsgTypeRemoteRunEvent)
		}
		var evt RemoteRunEvent
		if err := json.Unmarshal(dm.Payload, &evt); err != nil {
			t.Fatalf("invalid remote run event: %v", err)
		}
		if evt.RunID != "run-inject" || evt.Type != "injected" {
			t.Fatalf("event = %#v, want run-inject injected", evt)
		}
	case <-time.After(time.Second):
		t.Fatal("remote injected acknowledgement was not sent")
	}
	if _, ok := srv.remoteRuns.Load("run-inject"); ok {
		t.Fatal("injected follow-up should not register a separate remote run")
	}
}
