package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRemoteRequest_AllowsStatus(t *testing.T) {
	srv := NewServer(0, &Client{}, nil, "test")
	resp := srv.HandleRemoteRequest(context.Background(), RemoteRequest{
		Method: http.MethodGet,
		Path:   "/status",
	})
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d, error = %q", resp.Status, resp.Error)
	}
	var body map[string]any
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("invalid JSON body: %v", err)
	}
	if body["version"] != "test" {
		t.Fatalf("version = %v, want test", body["version"])
	}
}

func TestRemoteRequest_DeniesMessageAutoApprovalPath(t *testing.T) {
	srv := NewServer(0, nil, nil, "test")
	resp := srv.HandleRemoteRequest(context.Background(), RemoteRequest{
		Method: http.MethodPost,
		Path:   "/message",
		Body:   json.RawMessage(`{"text":"run something"}`),
	})
	if resp.Status != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", resp.Status, http.StatusForbidden)
	}
}

func TestRemoteRequest_DeniesLocalMutationAndApprovalPaths(t *testing.T) {
	srv := NewServer(0, nil, nil, "test")
	tests := []struct {
		name   string
		method string
		path   string
		body   json.RawMessage
	}{
		{
			name:   "approval",
			method: http.MethodPost,
			path:   "/approval",
			body:   json.RawMessage(`{"request_id":"req-1","decision":"allow"}`),
		},
		{
			name:   "cancel",
			method: http.MethodPost,
			path:   "/cancel",
			body:   json.RawMessage(`{"session_id":"sess-1"}`),
		},
		{
			name:   "approval list",
			method: http.MethodGet,
			path:   "/approvals",
		},
		{
			name:   "queue list",
			method: http.MethodGet,
			path:   "/queue",
		},
		{
			name:   "queue enqueue",
			method: http.MethodPost,
			path:   "/queue",
			body:   json.RawMessage(`{"session_id":"sess-1","text":"run this next"}`),
		},
		{
			name:   "queue delete",
			method: http.MethodDelete,
			path:   "/queue/msg-1",
		},
		{
			name:   "shutdown",
			method: http.MethodPost,
			path:   "/shutdown",
		},
		{
			name:   "config",
			method: http.MethodPost,
			path:   "/config",
			body:   json.RawMessage(`{}`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := srv.HandleRemoteRequest(context.Background(), RemoteRequest{
				Method: tt.method,
				Path:   tt.path,
				Body:   tt.body,
			})
			if resp.Status != http.StatusForbidden {
				t.Fatalf("%s %s status = %d, want %d", tt.method, tt.path, resp.Status, http.StatusForbidden)
			}
		})
	}
}

func TestRemoteResponse_AllowsNonJSONBody(t *testing.T) {
	var sent DaemonMessage
	client := &Client{
		envelopeSender: func(msg DaemonMessage) error {
			sent = msg
			return nil
		},
	}
	if err := client.sendRemoteResponse("req-1", RemoteResponse{
		Status:  http.StatusInternalServerError,
		Body:    []byte("plain text error"),
		Headers: map[string]string{"Content-Type": "text/plain; charset=utf-8"},
	}); err != nil {
		t.Fatalf("sendRemoteResponse returned error: %v", err)
	}
	if sent.Type != MsgTypeRemoteResponse || sent.MessageID != "req-1" {
		t.Fatalf("sent envelope = %#v", sent)
	}
	var resp RemoteResponse
	if err := json.Unmarshal(sent.Payload, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if string(resp.Body) != "plain text error" {
		t.Fatalf("body = %q", string(resp.Body))
	}
}

func TestRemoteResponseBody_RejectsOversizeBeforeCloudEnvelope(t *testing.T) {
	body, tooLarge := readRemoteResponseBody(strings.NewReader(strings.Repeat("x", maxRemoteResponseBodyBytes+1)))
	if !tooLarge {
		t.Fatal("oversize remote response body was not rejected")
	}
	if len(body) != 0 {
		t.Fatalf("oversize body should not be returned, got %d bytes", len(body))
	}
}

func TestRemoteEventForwarderSendsEventBusEvents(t *testing.T) {
	client := NewClient("ws://localhost:1/x", "", nil, nil)
	got := make(chan DaemonMessage, 1)
	client.envelopeSender = func(dm DaemonMessage) error {
		got <- dm
		return nil
	}
	srv := NewServer(0, client, nil, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.forwardRemoteEvents(ctx)

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				srv.EventBus().Emit(Event{Type: EventAgentReply, Payload: json.RawMessage(`{"text":"done"}`)})
			}
		}
	}()

	select {
	case dm := <-got:
		cancel()
		<-done
		if dm.Type != MsgTypeRemoteEvent {
			t.Fatalf("message type = %q, want %q", dm.Type, MsgTypeRemoteEvent)
		}
		var evt RemoteEvent
		if err := json.Unmarshal(dm.Payload, &evt); err != nil {
			t.Fatalf("invalid remote_event payload: %v", err)
		}
		if evt.Type != EventAgentReply {
			t.Fatalf("event type = %q, want %q", evt.Type, EventAgentReply)
		}
	case <-time.After(time.Second):
		t.Fatal("remote_event was not sent")
	}
}

func TestRemoteEventForwarderSkipsLocalApprovalEvents(t *testing.T) {
	client := NewClient("ws://localhost:1/x", "", nil, nil)
	got := make(chan DaemonMessage, 2)
	client.envelopeSender = func(dm DaemonMessage) error {
		got <- dm
		return nil
	}
	srv := NewServer(0, client, nil, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.forwardRemoteEvents(ctx)
	deadline := time.After(time.Second)
	for !srv.EventBus().HasSubscribers() {
		select {
		case <-deadline:
			t.Fatal("remote event forwarder did not subscribe")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	srv.EventBus().Emit(Event{Type: EventApprovalRequest, Payload: json.RawMessage(`{"request_id":"req-1"}`)})
	srv.EventBus().Emit(Event{Type: EventApprovalResolved, Payload: json.RawMessage(`{"request_id":"req-1"}`)})
	srv.EventBus().Emit(Event{Type: EventApprovalNotice, Payload: json.RawMessage(`{"message":"notice"}`)})
	srv.EventBus().Emit(Event{Type: EventAgentReply, Payload: json.RawMessage(`{"text":"done"}`)})

	select {
	case dm := <-got:
		var evt RemoteEvent
		if err := json.Unmarshal(dm.Payload, &evt); err != nil {
			t.Fatalf("invalid remote_event payload: %v", err)
		}
		if evt.Type != EventAgentReply {
			t.Fatalf("forwarded event type = %q, want only %q", evt.Type, EventAgentReply)
		}
	case <-time.After(time.Second):
		t.Fatal("allowed event was not forwarded")
	}

	select {
	case dm := <-got:
		t.Fatalf("unexpected extra forwarded event: %+v", dm)
	case <-time.After(50 * time.Millisecond):
	}
}
