package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agenttypes"
)

func TestHandleCancel_LegacyFireAndForget(t *testing.T) {
	srv, sc, cleanup := newQueueHandlerServer(t)
	defer cleanup()

	// Register a fake route so CancelRoute has something to cancel.
	sc.mu.Lock()
	sc.routes["agent:test"] = &routeEntry{cancelPending: false}
	sc.mu.Unlock()

	body := mustMarshal(t, map[string]any{
		"route_key": "agent:test",
	})
	req := httptest.NewRequest(http.MethodPost, "/cancel", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleCancel(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["reason"] != "user_cancel" {
		t.Errorf("default reason: want user_cancel, got %v", resp["reason"])
	}
	if resp["restored"] != false {
		t.Errorf("restored: want false, got %v", resp["restored"])
	}
}

func TestHandleCancel_UnknownReasonReturns400(t *testing.T) {
	srv, _, cleanup := newQueueHandlerServer(t)
	defer cleanup()

	body := mustMarshal(t, map[string]any{
		"route_key": "agent:test",
		"reason":    "rogue",
	})
	req := httptest.NewRequest(http.MethodPost, "/cancel", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleCancel(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown reason: want 400, got %d", rec.Code)
	}
}

func TestHandleCancel_RestoreLastNoActiveRun(t *testing.T) {
	srv, _, cleanup := newQueueHandlerServer(t)
	defer cleanup()

	body := mustMarshal(t, map[string]any{
		"route_key":    "agent:nonexistent",
		"restore_last": true,
	})
	req := httptest.NewRequest(http.MethodPost, "/cancel", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleCancel(rec, req)

	// No active run → 200 with restored=false (idempotent semantics).
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["restored"] != false {
		t.Errorf("restored should be false when no run exists, got %v", resp["restored"])
	}
}

func TestHandleCancel_MissingRouteKey(t *testing.T) {
	srv, _, cleanup := newQueueHandlerServer(t)
	defer cleanup()

	body := mustMarshal(t, map[string]any{
		"reason": "user_cancel",
	})
	req := httptest.NewRequest(http.MethodPost, "/cancel", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleCancel(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing key: want 400, got %d", rec.Code)
	}
}

func TestCancelRouteWithReason_StampsPendingOnIdleRoute(t *testing.T) {
	sc := NewSessionCache(t.TempDir())
	sc.mu.Lock()
	sc.routes["r1"] = &routeEntry{}
	sc.mu.Unlock()

	sc.CancelRouteWithReason("r1", agenttypes.ReasonInterrupt)

	sc.mu.Lock()
	pending := sc.routes["r1"].cancelPending
	reason := sc.routes["r1"].pendingReason
	sc.mu.Unlock()

	if !pending {
		t.Error("cancelPending should be set when no active cancel handle exists")
	}
	if reason != agenttypes.ReasonInterrupt {
		t.Errorf("pendingReason = %v, want Interrupt", reason)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
