package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/guicontrol"
)

const computerUseHTTPPresenceToken = "computer-use-http-presence"

type computerUseHTTPFixture struct {
	server      *Server
	coordinator *guicontrol.Coordinator
	now         time.Time
	lease       guicontrol.WorkflowLease
}

func newComputerUseHTTPFixture(t *testing.T) *computerUseHTTPFixture {
	t.Helper()
	fixture := &computerUseHTTPFixture{
		now: time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC),
	}
	fixture.coordinator = guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{
		InstanceID: "cui_http_fixture",
		Now:        func() time.Time { return fixture.now },
		NewID:      func(prefix string) string { return prefix + "_http_fixture" },
		LeaseTTL:   30 * time.Second,
	})
	lease, err := fixture.coordinator.BeginWorkflow(guicontrol.WorkflowRequest{
		SessionID:            "session_http_fixture",
		TurnID:               "turn_http_fixture",
		SourceKind:           "desktop",
		SourceLabel:          "Kocoro Desktop",
		RequestedAppBundleID: "com.apple.Notes",
		RequestedAppName:     "Notes",
		AllowedAppBundleIDs:  []string{"com.apple.Notes"},
		PolicySnapshotID:     "policy_http_fixture",
	})
	if err != nil {
		t.Fatalf("BeginWorkflow: %v", err)
	}
	fixture.lease = lease
	fixture.server = NewServer(0, nil, nil, "test")
	fixture.server.SetComputerUseCoordinator(fixture.coordinator)
	t.Setenv(localPresenceEnv, computerUseHTTPPresenceToken)
	return fixture
}

func (fixture *computerUseHTTPFixture) request(
	t *testing.T,
	method string,
	path string,
	body string,
	token string,
	origin string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set(localPresenceHeader, token)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(rec, req)
	return rec
}

func requireComputerUseHTTPError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d: %s", rec.Code, status, rec.Body.Bytes())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if code == "" {
		if body["error"] != "local presence confirmation required" || len(body) != 1 {
			t.Fatalf("unauthorized body = %#v", body)
		}
		return
	}
	if body["code"] != code || body["error"] == "" || len(body) != 2 {
		t.Fatalf("error body = %#v, want stable code %q", body, code)
	}
}

func TestNewServerDefaultsToProcessComputerUseCoordinator(t *testing.T) {
	server := NewServer(0, nil, nil, "test")
	if server.computerUseCoordinator != guicontrol.ProcessCoordinator() {
		t.Fatal("NewServer did not bind the process-wide GUI control coordinator")
	}
}

func TestComputerUseControlPlaneRequiresLocalPresenceEvenWithLocalCORS(t *testing.T) {
	fixture := newComputerUseHTTPFixture(t)
	requests := []struct {
		name, method, path, body string
	}{
		{name: "activity", method: http.MethodGet, path: "/local/computer-use/activity"},
		{name: "control", method: http.MethodPost, path: "/local/computer-use/control", body: `{"lease_id":"cul_http_fixture","action":"stop","idempotency_key":"stop_http"}`},
		{name: "heartbeat", method: http.MethodPost, path: "/local/computer-use/heartbeat", body: `{"schema_version":1,"lease_id":"cul_http_fixture"}`},
	}
	for _, request := range requests {
		t.Run(request.name+"/missing", func(t *testing.T) {
			rec := fixture.request(t, request.method, request.path, request.body, "", "")
			requireComputerUseHTTPError(t, rec, http.StatusForbidden, "")
		})
		t.Run(request.name+"/wrong", func(t *testing.T) {
			rec := fixture.request(t, request.method, request.path, request.body, "wrong", "")
			requireComputerUseHTTPError(t, rec, http.StatusForbidden, "")
		})
		t.Run(request.name+"/cors-is-not-auth", func(t *testing.T) {
			rec := fixture.request(t, request.method, request.path, request.body, "", "http://localhost:5173")
			requireComputerUseHTTPError(t, rec, http.StatusForbidden, "")
			if rec.Header().Get("Access-Control-Allow-Origin") != "http://localhost:5173" {
				t.Fatal("local CORS response header missing")
			}
		})
	}
	if snapshot := fixture.coordinator.Snapshot(); snapshot.Active == nil || snapshot.Active.LeaseState != guicontrol.ComputerUseLeaseActive {
		t.Fatalf("unauthorized request changed coordinator state: %+v", snapshot)
	}
}

func TestComputerUseControlPlaneRoutesRejectWrongMethods(t *testing.T) {
	fixture := newComputerUseHTTPFixture(t)
	for _, request := range []struct {
		method, path, allow string
	}{
		{method: http.MethodPost, path: "/local/computer-use/activity", allow: http.MethodGet},
		{method: http.MethodGet, path: "/local/computer-use/control", allow: http.MethodPost},
		{method: http.MethodGet, path: "/local/computer-use/heartbeat", allow: http.MethodPost},
	} {
		rec := fixture.request(t, request.method, request.path, "", computerUseHTTPPresenceToken, "")
		requireComputerUseHTTPError(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
		if rec.Header().Get("Allow") != request.allow {
			t.Fatalf("%s %s Allow = %q, want %q", request.method, request.path, rec.Header().Get("Allow"), request.allow)
		}
	}

	unauthorized := fixture.request(t, http.MethodDelete, "/local/computer-use/activity", "", "", "http://localhost:5173")
	requireComputerUseHTTPError(t, unauthorized, http.StatusForbidden, "")
}

func TestComputerUseActivityReturnsAuthoritativeSnapshot(t *testing.T) {
	fixture := newComputerUseHTTPFixture(t)
	rec := fixture.request(t, http.MethodGet, "/local/computer-use/activity", "", computerUseHTTPPresenceToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.Bytes())
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
	var got guicontrol.ComputerUseActivitySnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode activity snapshot: %v", err)
	}
	want := fixture.coordinator.Snapshot()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %+v, want authoritative %+v", got, want)
	}
}

func TestComputerUseActivityLazilyExpiresStaleLease(t *testing.T) {
	fixture := newComputerUseHTTPFixture(t)
	fixture.now = fixture.now.Add(31 * time.Second)
	rec := fixture.request(t, http.MethodGet, "/local/computer-use/activity", "", computerUseHTTPPresenceToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.Bytes())
	}
	var snapshot guicontrol.ComputerUseActivitySnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Active != nil || snapshot.Revision != 2 {
		t.Fatalf("stale GET snapshot = %+v; want idle revision 2", snapshot)
	}
	_, err := fixture.coordinator.BeginWorkflow(guicontrol.WorkflowRequest{
		SessionID: "session_http_fixture", TurnID: "turn_http_fixture",
	})
	var expired *guicontrol.LeaseExpiredError
	if !errors.As(err, &expired) {
		t.Fatalf("GET did not tombstone expired turn: %T %v", err, err)
	}
}

func TestComputerUseControlUsesStrictCodecAndStableErrors(t *testing.T) {
	for _, test := range []struct {
		name, payload string
	}{
		{name: "unknown", payload: `{"lease_id":"cul_http_fixture","action":"pause","idempotency_key":"strict","extra":true}`},
		{name: "duplicate", payload: `{"lease_id":"cul_http_fixture","action":"pause","action":"stop","idempotency_key":"strict"}`},
		{name: "trailing", payload: `{"lease_id":"cul_http_fixture","action":"pause","idempotency_key":"strict"}{}`},
		{name: "missing", payload: `{}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newComputerUseHTTPFixture(t)
			rec := fixture.request(t, http.MethodPost, "/local/computer-use/control", test.payload, computerUseHTTPPresenceToken, "")
			requireComputerUseHTTPError(t, rec, http.StatusBadRequest, "invalid_computer_use_control_request")
		})
	}

	t.Run("stale lease", func(t *testing.T) {
		fixture := newComputerUseHTTPFixture(t)
		rec := fixture.request(t, http.MethodPost, "/local/computer-use/control",
			`{"lease_id":"cul_stale","action":"stop","idempotency_key":"stale"}`,
			computerUseHTTPPresenceToken, "")
		requireComputerUseHTTPError(t, rec, http.StatusConflict, "computer_use_stale_lease")
	})
	t.Run("invalid transition", func(t *testing.T) {
		fixture := newComputerUseHTTPFixture(t)
		rec := fixture.request(t, http.MethodPost, "/local/computer-use/control",
			`{"lease_id":"cul_http_fixture","action":"resume","idempotency_key":"resume_active"}`,
			computerUseHTTPPresenceToken, "")
		requireComputerUseHTTPError(t, rec, http.StatusConflict, "computer_use_invalid_transition")
	})
	t.Run("expired", func(t *testing.T) {
		fixture := newComputerUseHTTPFixture(t)
		fixture.now = fixture.now.Add(31 * time.Second)
		rec := fixture.request(t, http.MethodPost, "/local/computer-use/control",
			`{"lease_id":"cul_http_fixture","action":"stop","idempotency_key":"expired"}`,
			computerUseHTTPPresenceToken, "")
		requireComputerUseHTTPError(t, rec, http.StatusGone, "computer_use_lease_expired")
	})
	t.Run("idempotency conflict", func(t *testing.T) {
		fixture := newComputerUseHTTPFixture(t)
		first := fixture.request(t, http.MethodPost, "/local/computer-use/control",
			`{"lease_id":"cul_http_fixture","action":"pause","idempotency_key":"same"}`,
			computerUseHTTPPresenceToken, "")
		if first.Code != http.StatusOK {
			t.Fatalf("first status = %d: %s", first.Code, first.Body.Bytes())
		}
		conflict := fixture.request(t, http.MethodPost, "/local/computer-use/control",
			`{"lease_id":"cul_http_fixture","action":"take_over","idempotency_key":"same"}`,
			computerUseHTTPPresenceToken, "")
		requireComputerUseHTTPError(t, conflict, http.StatusConflict, "computer_use_idempotency_conflict")
	})
}

func TestComputerUseControlResponseIsIdempotent(t *testing.T) {
	fixture := newComputerUseHTTPFixture(t)
	if _, err := fixture.coordinator.BeginAction(context.Background(), guicontrol.ActionRequest{
		LeaseID: fixture.lease.LeaseID, TurnID: fixture.lease.TurnID,
		ToolUseID: "toolu_http_stop", ActionKind: "click",
		Effect: guicontrol.ComputerUseActionMutation, TargetBundleID: "com.apple.Notes",
	}); err != nil {
		t.Fatalf("BeginAction: %v", err)
	}
	sub := fixture.server.eventBus.Subscribe()
	defer fixture.server.eventBus.Unsubscribe(sub)
	payload := `{"lease_id":"cul_http_fixture","action":"stop","idempotency_key":"stop_once"}`
	first := fixture.request(t, http.MethodPost, "/local/computer-use/control", payload, computerUseHTTPPresenceToken, "")
	second := fixture.request(t, http.MethodPost, "/local/computer-use/control", payload, computerUseHTTPPresenceToken, "")
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("statuses = %d, %d: first=%s second=%s", first.Code, second.Code, first.Body.Bytes(), second.Body.Bytes())
	}
	if !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Fatalf("idempotent response changed\nfirst: %s\nsecond: %s", first.Body.Bytes(), second.Body.Bytes())
	}
	var response guicontrol.ComputerUseControlResponse
	if err := json.Unmarshal(first.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Accepted || response.LeaseID != fixture.lease.LeaseID || response.LeaseState != guicontrol.ComputerUseLeaseStopping {
		t.Fatalf("control response = %+v", response)
	}
	if response.Quiesced {
		t.Fatal("Stop response claimed quiescence before executor acknowledgement")
	}
	event := waitBusEvent(t, sub, EventComputerUseActivity)
	var activity guicontrol.ComputerUseActivityEvent
	if err := json.Unmarshal(event.Payload, &activity); err != nil {
		t.Fatalf("decode activity event: %v", err)
	}
	if activity.CoordinatorInstanceID != "cui_http_fixture" || activity.Revision != response.Revision ||
		activity.LeaseState != guicontrol.ComputerUseLeaseStopping {
		t.Fatalf("activity event = %+v, control response = %+v", activity, response)
	}
}

func TestComputerUseTakeOverHTTPWaitsForExecutorQuiescence(t *testing.T) {
	fixture := newComputerUseHTTPFixture(t)
	action, err := fixture.coordinator.BeginAction(context.Background(), guicontrol.ActionRequest{
		LeaseID: fixture.lease.LeaseID, TurnID: fixture.lease.TurnID,
		ToolUseID: "toolu_takeover", ActionKind: "drag",
		Effect: guicontrol.ComputerUseActionMutation, TargetBundleID: "com.apple.Notes",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(
		http.MethodPost, "/local/computer-use/control",
		strings.NewReader(`{"lease_id":"cul_http_fixture","action":"take_over","idempotency_key":"takeover_wait"}`))
	req.Header.Set(localPresenceHeader, computerUseHTTPPresenceToken)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		fixture.server.Handler().ServeHTTP(rec, req)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("Take Over returned before executor cleanup acknowledgement")
	case <-time.After(20 * time.Millisecond):
	}
	verified := guicontrol.ComputerUseResultVerified
	if err := fixture.coordinator.FinishAction(guicontrol.ActionFinish{
		LeaseID: fixture.lease.LeaseID, ActionID: action.ActionID,
		Phase: guicontrol.ComputerUsePhaseIdle, Result: &verified,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Take Over did not return after executor cleanup acknowledgement")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.Bytes())
	}
	var response guicontrol.ComputerUseControlResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Accepted || !response.Quiesced ||
		response.LeaseState != guicontrol.ComputerUseLeasePaused {
		t.Fatalf("Take Over response = %+v", response)
	}
}

func TestComputerUseIdleStopReturnsTerminalAndClearsSnapshot(t *testing.T) {
	fixture := newComputerUseHTTPFixture(t)
	payload := `{"lease_id":"cul_http_fixture","action":"stop","idempotency_key":"stop_idle"}`
	rec := fixture.request(t, http.MethodPost, "/local/computer-use/control", payload, computerUseHTTPPresenceToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.Bytes())
	}
	var response guicontrol.ComputerUseControlResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Accepted || response.LeaseState != guicontrol.ComputerUseLeaseTerminal {
		t.Fatalf("idle stop response = %+v", response)
	}
	if snapshot := fixture.coordinator.Snapshot(); snapshot.Active != nil || snapshot.Revision != response.Revision {
		t.Fatalf("idle stop snapshot = %+v", snapshot)
	}
}

func TestComputerUseResumeReportsCancellationBarrier(t *testing.T) {
	fixture := newComputerUseHTTPFixture(t)
	action, err := fixture.coordinator.BeginAction(context.Background(), guicontrol.ActionRequest{
		LeaseID: fixture.lease.LeaseID, TurnID: fixture.lease.TurnID,
		ToolUseID: "toolu_http_pause", ActionKind: "click",
		Effect: guicontrol.ComputerUseActionMutation, TargetBundleID: "com.apple.Notes",
	})
	if err != nil {
		t.Fatal(err)
	}
	pause := fixture.request(t, http.MethodPost, "/local/computer-use/control",
		`{"lease_id":"cul_http_fixture","action":"pause","idempotency_key":"pause_barrier"}`,
		computerUseHTTPPresenceToken, "")
	if pause.Code != http.StatusOK {
		t.Fatalf("pause status = %d: %s", pause.Code, pause.Body.Bytes())
	}
	resumePayload := `{"lease_id":"cul_http_fixture","action":"resume","idempotency_key":"resume_barrier"}`
	blocked := fixture.request(t, http.MethodPost, "/local/computer-use/control", resumePayload, computerUseHTTPPresenceToken, "")
	requireComputerUseHTTPError(t, blocked, http.StatusConflict, "computer_use_action_in_progress")

	cancelled := guicontrol.ComputerUseResultCancelled
	if err := fixture.coordinator.FinishAction(guicontrol.ActionFinish{
		LeaseID: fixture.lease.LeaseID, ActionID: action.ActionID, Result: &cancelled,
	}); err != nil {
		t.Fatal(err)
	}
	resumed := fixture.request(t, http.MethodPost, "/local/computer-use/control", resumePayload, computerUseHTTPPresenceToken, "")
	if resumed.Code != http.StatusOK {
		t.Fatalf("resume after cancellation ack status = %d: %s", resumed.Code, resumed.Body.Bytes())
	}
}

func TestComputerUseHeartbeatUsesSharedCodecAndStableMappings(t *testing.T) {
	for _, test := range []struct {
		name, payload string
	}{
		{name: "unknown", payload: `{"schema_version":1,"lease_id":"cul_http_fixture","extra":true}`},
		{name: "duplicate", payload: `{"schema_version":1,"lease_id":"cul_http_fixture","lease_id":"cul_other"}`},
		{name: "trailing", payload: `{"schema_version":1,"lease_id":"cul_http_fixture"}[]`},
		{name: "schema", payload: `{"schema_version":2,"lease_id":"cul_http_fixture"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newComputerUseHTTPFixture(t)
			rec := fixture.request(t, http.MethodPost, "/local/computer-use/heartbeat", test.payload, computerUseHTTPPresenceToken, "")
			requireComputerUseHTTPError(t, rec, http.StatusBadRequest, "invalid_computer_use_heartbeat_request")
		})
	}

	t.Run("success", func(t *testing.T) {
		fixture := newComputerUseHTTPFixture(t)
		fixture.now = fixture.now.Add(5 * time.Second)
		rec := fixture.request(t, http.MethodPost, "/local/computer-use/heartbeat",
			`{"schema_version":1,"lease_id":"cul_http_fixture"}`,
			computerUseHTTPPresenceToken, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.Bytes())
		}
		response, err := guicontrol.DecodeComputerUseHeartbeatResponse(rec.Body.Bytes())
		if err != nil {
			t.Fatalf("strict heartbeat response decode: %v", err)
		}
		if response.CoordinatorInstanceID != "cui_http_fixture" || response.Revision != 1 ||
			response.LeaseID != fixture.lease.LeaseID || response.LeaseState != guicontrol.ComputerUseLeaseActive ||
			response.HeartbeatAt != "2026-07-22T12:00:05Z" || response.ExpiresAt != "2026-07-22T12:00:35Z" {
			t.Fatalf("heartbeat response = %+v", response)
		}
	})

	t.Run("stale", func(t *testing.T) {
		fixture := newComputerUseHTTPFixture(t)
		rec := fixture.request(t, http.MethodPost, "/local/computer-use/heartbeat",
			`{"schema_version":1,"lease_id":"cul_stale"}`,
			computerUseHTTPPresenceToken, "")
		requireComputerUseHTTPError(t, rec, http.StatusConflict, "computer_use_stale_lease")
	})
	t.Run("expired", func(t *testing.T) {
		fixture := newComputerUseHTTPFixture(t)
		fixture.now = fixture.now.Add(31 * time.Second)
		rec := fixture.request(t, http.MethodPost, "/local/computer-use/heartbeat",
			`{"schema_version":1,"lease_id":"cul_http_fixture"}`,
			computerUseHTTPPresenceToken, "")
		requireComputerUseHTTPError(t, rec, http.StatusGone, "computer_use_lease_expired")
	})
}

func TestComputerUseControlPlaneReturnsStableUnavailableWhenCoordinatorMissing(t *testing.T) {
	fixture := newComputerUseHTTPFixture(t)
	fixture.server.SetComputerUseCoordinator(nil)
	for _, request := range []struct {
		method, path, body string
	}{
		{method: http.MethodGet, path: "/local/computer-use/activity"},
		{method: http.MethodPost, path: "/local/computer-use/control", body: `{"lease_id":"cul_http_fixture","action":"stop","idempotency_key":"stop"}`},
		{method: http.MethodPost, path: "/local/computer-use/heartbeat", body: `{"schema_version":1,"lease_id":"cul_http_fixture"}`},
	} {
		rec := fixture.request(t, request.method, request.path, request.body, computerUseHTTPPresenceToken, "")
		requireComputerUseHTTPError(t, rec, http.StatusServiceUnavailable, "computer_use_control_unavailable")
	}
}

func TestComputerUseExpiryLoopExpiresAndStopsWithServerLifecycle(t *testing.T) {
	var clockMu sync.Mutex
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{
		InstanceID: "cui_expiry_loop",
		Now: func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return now
		},
		NewID:    func(prefix string) string { return prefix + "_expiry_loop" },
		LeaseTTL: 10 * time.Millisecond,
	})
	if _, err := coordinator.BeginWorkflow(guicontrol.WorkflowRequest{
		SessionID: "session_expiry_loop", TurnID: "turn_expiry_loop",
	}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(0, nil, nil, "test")
	server.SetComputerUseCoordinator(coordinator)
	server.computerUseExpiryInterval = time.Millisecond
	clockMu.Lock()
	now = now.Add(20 * time.Millisecond)
	clockMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		server.runComputerUseExpiryLoop(ctx)
		close(done)
	}()
	deadline := time.After(time.Second)
	for coordinator.Snapshot().Active != nil {
		select {
		case <-deadline:
			t.Fatal("expiry loop did not expire stale lease")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("expiry loop did not stop after lifecycle cancellation")
	}

	next, err := coordinator.BeginWorkflow(guicontrol.WorkflowRequest{
		SessionID: "session_after_loop", TurnID: "turn_after_loop",
	})
	if err != nil {
		t.Fatal(err)
	}
	clockMu.Lock()
	now = now.Add(20 * time.Millisecond)
	clockMu.Unlock()
	time.Sleep(5 * time.Millisecond)
	if snapshot := coordinator.Snapshot(); snapshot.Active == nil || snapshot.Active.LeaseID != next.LeaseID {
		t.Fatalf("cancelled expiry loop continued mutating coordinator: %+v", snapshot)
	}
}

func TestComputerUseCoordinatorErrorsHaveStableHTTPMappings(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{name: "stale", err: &guicontrol.StaleLeaseError{LeaseID: "cul_stale"}, status: http.StatusConflict, code: "computer_use_stale_lease"},
		{name: "expired", err: &guicontrol.LeaseExpiredError{LeaseID: "cul_expired"}, status: http.StatusGone, code: "computer_use_lease_expired"},
		{name: "stopped", err: &guicontrol.StoppedTurnError{TurnID: "turn_stopped"}, status: http.StatusConflict, code: "computer_use_stopped"},
		{name: "transition", err: &guicontrol.InvalidTransitionError{State: guicontrol.ComputerUseLeaseStopping}, status: http.StatusConflict, code: "computer_use_invalid_transition"},
		{name: "idempotency", err: &guicontrol.IdempotencyConflictError{Key: "duplicate"}, status: http.StatusConflict, code: "computer_use_idempotency_conflict"},
		{name: "action in progress", err: &guicontrol.ActionInProgressError{LeaseID: "cul_active", ActionID: "cua_active"}, status: http.StatusConflict, code: "computer_use_action_in_progress"},
		{name: "unexpected", err: errors.New("private detail"), status: http.StatusInternalServerError, code: "computer_use_control_failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeComputerUseCoordinatorError(rec, test.err)
			requireComputerUseHTTPError(t, rec, test.status, test.code)
			if strings.Contains(rec.Body.String(), "private detail") {
				t.Fatal("internal coordinator error leaked through HTTP response")
			}
		})
	}
}

func TestCapabilitiesAdvertiseComputerUseControlV1(t *testing.T) {
	for _, capability := range Capabilities {
		if capability == CapComputerUseControlV1 {
			return
		}
	}
	t.Fatalf("default Capabilities = %v, missing %q", Capabilities, CapComputerUseControlV1)
}
