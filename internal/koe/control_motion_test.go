package koe

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMotionPlayEndpointStatusMapping(t *testing.T) {
	var playErr error
	var lastName string
	s := NewControlServer(nil, nil, nil)
	s.SetMotionHandlers(
		func(name string) error { lastName = name; return playErr },
		func() error { return nil },
		func() MotionStatus { return MotionStatus{} },
	)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	post := func(body string) (int, string) {
		resp, err := http.Post(srv.URL+"/motion/play", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST /motion/play %q: %v", body, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}

	// 202 success — the seam receives the move name.
	playErr = nil
	if code, _ := post(`{"name":"happy1"}`); code != http.StatusAccepted {
		t.Fatalf("valid play: status=%d, want 202", code)
	}
	if lastName != "happy1" {
		t.Fatalf("seam saw name=%q, want happy1", lastName)
	}

	// 400 on empty/missing name.
	for _, body := range []string{`{"name":""}`, `{}`, ``} {
		if code, _ := post(body); code != http.StatusBadRequest {
			t.Fatalf("empty name %q: status=%d, want 400", body, code)
		}
	}

	// 409 while a call is active.
	playErr = ErrCallActive
	if code, body := post(`{"name":"happy1"}`); code != http.StatusConflict || !strings.Contains(body, "call_active") {
		t.Fatalf("call-active play: status=%d body=%s, want 409 call_active", code, body)
	}

	// 404 unknown move.
	playErr = ErrUnknownMove
	if code, body := post(`{"name":"nope"}`); code != http.StatusNotFound || !strings.Contains(body, "unknown_move") {
		t.Fatalf("unknown move: status=%d body=%s, want 404 unknown_move", code, body)
	}

	// 503 bridge unavailable.
	playErr = ErrBridgeUnavailable
	if code, body := post(`{"name":"happy1"}`); code != http.StatusServiceUnavailable || !strings.Contains(body, "bridge_unavailable") {
		t.Fatalf("bridge down: status=%d body=%s, want 503 bridge_unavailable", code, body)
	}
}

// TestMotionPlayRefusedByCallStateAuthority pins the W2-1 in-call gate to the
// same store the SSE call_state events (and thus Desktop) read: once the server
// has emitted connecting/on_call, POST /motion/play must 409 call_active even if
// the wired play seam's own view of the call lags behind (live incident: a real
// robot call answered 202 because the seam's private call flag read false while
// the session was audibly on-call). "ended" reopens manual playback.
func TestMotionPlayRefusedByCallStateAuthority(t *testing.T) {
	played := 0
	s := NewControlServer(nil, nil, nil)
	// A seam that would accept — simulating the divergent state where the play
	// closure's own call check believes no call is active.
	s.SetMotionHandlers(
		func(name string) error { played++; return nil },
		nil,
		nil,
	)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	post := func() (int, string) {
		resp, err := http.Post(srv.URL+"/motion/play", "application/json", strings.NewReader(`{"name":"happy1"}`))
		if err != nil {
			t.Fatalf("POST /motion/play: %v", err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}

	// The whole emitted call span refuses manual playback: from the "connecting"
	// broadcast (before the session flags flip) through "on_call".
	for _, state := range []string{"connecting", "on_call"} {
		s.EmitCallState(state)
		code, body := post()
		if code != http.StatusConflict || !strings.Contains(body, "call_active") {
			t.Fatalf("play during %s: status=%d body=%s, want 409 call_active", state, code, body)
		}
		if played != 0 {
			t.Fatalf("play seam ran during %s", state)
		}
	}

	// "ended" normalizes the snapshot back to idle → manual playback allowed.
	s.EmitCallState("ended")
	if code, body := post(); code != http.StatusAccepted {
		t.Fatalf("play after ended: status=%d body=%s, want 202", code, body)
	}
	if played != 1 {
		t.Fatalf("play seam ran %d times after ended, want 1", played)
	}
}

func TestMotionStopEndpoint(t *testing.T) {
	var stopErr error
	stopped := 0
	s := NewControlServer(nil, nil, nil)
	s.SetMotionHandlers(nil, func() error { stopped++; return stopErr }, nil)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/motion/stop", "application/json", strings.NewReader(""))
	if err != nil {
		t.Fatalf("POST /motion/stop: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || stopped != 1 {
		t.Fatalf("stop: status=%d stopped=%d, want 202 / 1", resp.StatusCode, stopped)
	}

	// Bridge down → 503. Stop is not gated on call state.
	stopErr = ErrBridgeUnavailable
	resp2, _ := http.Post(srv.URL+"/motion/stop", "application/json", strings.NewReader(""))
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body2), "bridge_unavailable") {
		t.Fatalf("stop bridge down: status=%d body=%s, want 503", resp2.StatusCode, body2)
	}
}

func TestMotionStatusEndpointJSONShape(t *testing.T) {
	s := NewControlServer(nil, nil, nil)
	s.SetMotionHandlers(nil, nil, func() MotionStatus {
		return MotionStatus{
			Moves:           []string{"happy1", "dance_samba"},
			CurrentMove:     "dance_samba",
			IsListening:     true,
			BreathingActive: true,
			BridgeState:     "connected",
		}
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/motion/status")
	if err != nil {
		t.Fatalf("GET /motion/status: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: code=%d, want 200", resp.StatusCode)
	}
	// Exact snake_case field names — the runtime facade merges these verbatim.
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("status body not JSON: %v (%s)", err, body)
	}
	for _, key := range []string{"moves", "current_move", "is_listening", "breathing_active", "bridge_state"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("status JSON missing key %q: %s", key, body)
		}
	}
	if got["current_move"] != "dance_samba" || got["is_listening"] != true || got["bridge_state"] != "connected" {
		t.Fatalf("status JSON values = %v", got)
	}
}

func TestMotionStatusEmptyMovesSerializesArray(t *testing.T) {
	s := NewControlServer(nil, nil, nil)
	s.SetMotionHandlers(nil, nil, func() MotionStatus { return MotionStatus{BridgeState: "connecting"} })
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/motion/status")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"moves":[]`) {
		t.Fatalf("empty moves should serialize as [], got %s", body)
	}
}

// TestMotionNilHandlersNotFound verifies the routes 404 when the motion facility is
// not wired (mac / no body) — what the runtime facade reads as "Koe has no motion".
func TestMotionNilHandlersNotFound(t *testing.T) {
	s := NewControlServer(nil, nil, nil)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	cases := []struct {
		method, path, body string
	}{
		{http.MethodPost, "/motion/play", `{"name":"happy1"}`},
		{http.MethodPost, "/motion/stop", ""},
		{http.MethodGet, "/motion/status", ""},
	}
	for _, c := range cases {
		req, _ := http.NewRequest(c.method, srv.URL+c.path, strings.NewReader(c.body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", c.method, c.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("nil-seam %s %s: status=%d, want 404", c.method, c.path, resp.StatusCode)
		}
	}
}
