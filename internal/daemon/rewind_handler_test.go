package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleRewind_MissingMessageID(t *testing.T) {
	srv, _, cleanup := newQueueHandlerServer(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/sessions/sess-1/rewind", nil)
	req.SetPathValue("id", "sess-1")
	rec := httptest.NewRecorder()
	srv.handleRewind(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing message_id: want 400, got %d", rec.Code)
	}
}

func TestHandleRewind_MissingSessionID(t *testing.T) {
	srv, _, cleanup := newQueueHandlerServer(t)
	defer cleanup()

	// Path value omitted — mimics misconfigured client.
	req := httptest.NewRequest(http.MethodPost, "/sessions//rewind?message_id=m1", nil)
	rec := httptest.NewRecorder()
	srv.handleRewind(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing session id: want 400, got %d", rec.Code)
	}
}

func TestHandleRewind_SessionNotFound(t *testing.T) {
	srv, _, cleanup := newQueueHandlerServer(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/sessions/nope/rewind?message_id=m1", nil)
	req.SetPathValue("id", "nope")
	rec := httptest.NewRecorder()
	srv.handleRewind(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown session: want 404, got %d body: %s", rec.Code, rec.Body.String())
	}
}
