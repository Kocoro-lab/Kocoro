package daemon

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agenttypes"
	_ "modernc.org/sqlite"
)

func newQueueHandlerServer(t *testing.T) (*Server, *SessionCache, func()) {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "mailbox.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	store, err := NewMailboxStore(db)
	if err != nil {
		db.Close()
		t.Fatalf("schema: %v", err)
	}
	sc := NewSessionCacheWithMailbox(dir, store, 100)
	bus := NewEventBus()
	deps := &ServerDeps{
		ShannonDir:   dir,
		SessionCache: sc,
		EventBus:     bus,
	}
	srv := NewServer(0, nil, deps, "test")
	return srv, sc, func() {
		db.Close()
	}
}

func TestHandleGetQueue_ReturnsDTO(t *testing.T) {
	srv, sc, cleanup := newQueueHandlerServer(t)
	defer cleanup()

	sc.EnqueueMessage("r1", agenttypes.QueuedMessage{
		ID: "m1", Text: "hello world", Source: "ws",
		Editable: true, EnqueuedAt: time.Now(),
		// internal fields that MUST NOT leak through DTO
		CloudMsgID: "cloud-secret",
		SessionID:  "sess-secret",
	})

	req := httptest.NewRequest(http.MethodGet, "/queue?route_key=r1", nil)
	rec := httptest.NewRecorder()
	srv.handleGetQueue(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, banned := range []string{"cloud-secret", "sess-secret", "cloud_msg_id", "session_id"} {
		if strings.Contains(body, banned) {
			t.Errorf("response leaks %q: %s", banned, body)
		}
	}
	if !strings.Contains(body, "\"preview\":\"hello world\"") {
		t.Errorf("preview missing or wrong: %s", body)
	}
}

func TestHandleGetQueue_MissingRouteKey(t *testing.T) {
	srv, _, cleanup := newQueueHandlerServer(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/queue", nil)
	rec := httptest.NewRecorder()
	srv.handleGetQueue(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing route_key: want 400, got %d", rec.Code)
	}
}

func TestHandleDeleteQueue_HappyPath(t *testing.T) {
	srv, sc, cleanup := newQueueHandlerServer(t)
	defer cleanup()

	sc.EnqueueMessage("r1", agenttypes.QueuedMessage{
		ID: "m1", Editable: true, EnqueuedAt: time.Now(),
	})

	req := httptest.NewRequest(http.MethodDelete, "/queue/m1?route_key=r1", nil)
	req.SetPathValue("id", "m1")
	rec := httptest.NewRecorder()
	srv.handleDeleteQueueItem(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", rec.Code, rec.Body.String())
	}
	if sc.MailboxLen("r1") != 0 {
		t.Errorf("queue still holds items after delete")
	}
}

func TestHandleDeleteQueue_RefusesNonEditable(t *testing.T) {
	srv, sc, cleanup := newQueueHandlerServer(t)
	defer cleanup()

	sc.EnqueueMessage("r1", agenttypes.QueuedMessage{
		ID: "m1", Editable: false, EnqueuedAt: time.Now(),
	})

	req := httptest.NewRequest(http.MethodDelete, "/queue/m1?route_key=r1", nil)
	req.SetPathValue("id", "m1")
	rec := httptest.NewRecorder()
	srv.handleDeleteQueueItem(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("non-editable: want 403, got %d", rec.Code)
	}
	if sc.MailboxLen("r1") != 1 {
		t.Error("forbidden retract must not mutate mailbox")
	}
}

func TestHandleDeleteQueue_NotFound(t *testing.T) {
	srv, _, cleanup := newQueueHandlerServer(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodDelete, "/queue/missing?route_key=r1", nil)
	req.SetPathValue("id", "missing")
	rec := httptest.NewRecorder()
	srv.handleDeleteQueueItem(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("missing id: want 404, got %d", rec.Code)
	}
}

func TestHandleDeleteQueue_EmitsRemovedEvent(t *testing.T) {
	srv, sc, cleanup := newQueueHandlerServer(t)
	defer cleanup()

	sub := srv.deps.EventBus.Subscribe()
	defer srv.deps.EventBus.Unsubscribe(sub)

	sc.EnqueueMessage("r1", agenttypes.QueuedMessage{
		ID: "m1", Editable: true, EnqueuedAt: time.Now(),
	})

	req := httptest.NewRequest(http.MethodDelete, "/queue/m1?route_key=r1", nil)
	req.SetPathValue("id", "m1")
	rec := httptest.NewRecorder()
	srv.handleDeleteQueueItem(rec, req)

	// Drain bus, accept up to 50ms.
	deadline := time.After(50 * time.Millisecond)
	for {
		select {
		case ev := <-sub:
			if ev.Type == EventQueueRemoved {
				var payload struct {
					MessageID string `json:"message_id"`
				}
				if err := json.Unmarshal(ev.Payload, &payload); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if payload.MessageID != "m1" {
					t.Errorf("event message_id: got %q", payload.MessageID)
				}
				return
			}
		case <-deadline:
			t.Fatal("no queue.removed event within 50ms")
		}
	}
}
