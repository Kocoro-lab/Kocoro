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
