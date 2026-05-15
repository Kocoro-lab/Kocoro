package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHandleEnqueueQueue_Accepts(t *testing.T) {
	srv, sc, cleanup := newQueueHandlerServer(t)
	defer cleanup()

	body := mustMarshal(t, map[string]any{
		"route_key": "r1",
		"text":      "hello mailbox",
		"source":    "http",
	})
	req := httptest.NewRequest(http.MethodPost, "/queue", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleEnqueueQueue(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status: %d body: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["outcome"] != "queued" {
		t.Errorf("outcome: %v", resp["outcome"])
	}
	if sc.MailboxLen("r1") != 1 {
		t.Errorf("mailbox len: %d", sc.MailboxLen("r1"))
	}
}

func TestHandleEnqueueQueue_MissingRouteAndSession(t *testing.T) {
	srv, _, cleanup := newQueueHandlerServer(t)
	defer cleanup()

	body := mustMarshal(t, map[string]any{"text": "x"})
	req := httptest.NewRequest(http.MethodPost, "/queue", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleEnqueueQueue(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing route + session: want 400, got %d", rec.Code)
	}
}

func TestHandleEnqueueQueue_EmptyPayloadRejected(t *testing.T) {
	srv, _, cleanup := newQueueHandlerServer(t)
	defer cleanup()

	body := mustMarshal(t, map[string]any{"route_key": "r1"})
	req := httptest.NewRequest(http.MethodPost, "/queue", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleEnqueueQueue(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty payload: want 400, got %d", rec.Code)
	}
}

func TestHandleEnqueueQueue_AcceptsAttachments(t *testing.T) {
	srv, sc, cleanup := newQueueHandlerServer(t)
	defer cleanup()

	body := mustMarshal(t, map[string]any{
		"route_key": "r1",
		"text":      "look at these",
		"attachments": []map[string]any{
			{"nonce": "noncea", "kind": "image"},
			{"original_url": "https://example.com/x.pdf", "kind": "document"},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/queue", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleEnqueueQueue(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status: %d body: %s", rec.Code, rec.Body.String())
	}
	snap := sc.MailboxSnapshot("r1")
	if len(snap) != 1 || len(snap[0].Attachments) != 2 {
		t.Errorf("attachments not preserved: %+v", snap)
	}
	if snap[0].Attachments[0].Kind != "image" || snap[0].Attachments[1].OriginalURL != "https://example.com/x.pdf" {
		t.Errorf("attachment fields mangled: %+v", snap[0].Attachments)
	}
}

func TestHandleEnqueueQueue_EmitsQueueAddedEvent(t *testing.T) {
	srv, _, cleanup := newQueueHandlerServer(t)
	defer cleanup()

	sub := srv.deps.EventBus.Subscribe()
	defer srv.deps.EventBus.Unsubscribe(sub)

	body := mustMarshal(t, map[string]any{
		"route_key": "r1",
		"text":      "fire event",
	})
	req := httptest.NewRequest(http.MethodPost, "/queue", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleEnqueueQueue(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status: %d body: %s", rec.Code, rec.Body.String())
	}
	select {
	case ev := <-sub:
		if ev.Type != EventQueueAdded {
			t.Errorf("event type: %s", ev.Type)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("no event received within 100ms")
	}
}
