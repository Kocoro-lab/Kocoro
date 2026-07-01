package koe

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// subscriberCount is a test-only accessor (compiled only in tests).
func (s *ControlServer) subscriberCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subscribers)
}

func TestControlServerStartEnd(t *testing.T) {
	var started, ended, interrupted bool
	s := NewControlServer(
		func(StartCallRequest) { started = true },
		func() { ended = true },
		func() { interrupted = true },
	)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/call/start", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /call/start: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !started {
		t.Error("POST /call/start did not invoke onStart")
	}
	if !strings.Contains(string(body), `"status":"ok"`) {
		t.Errorf("start response = %s", body)
	}

	resp2, err := http.Post(srv.URL+"/call/end", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /call/end: %v", err)
	}
	resp2.Body.Close()
	if !ended {
		t.Error("POST /call/end did not invoke onEnd")
	}

	resp3, err := http.Post(srv.URL+"/call/interrupt", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /call/interrupt: %v", err)
	}
	resp3.Body.Close()
	if !interrupted {
		t.Error("POST /call/interrupt did not invoke onInterrupt")
	}
}

func TestControlServerStartCarriesContext(t *testing.T) {
	got := make(chan StartCallRequest, 1)
	s := NewControlServer(func(req StartCallRequest) { got <- req }, nil, nil)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := `{"command":"start_call","cwd":"/Users/hu/project","foreground_hint":{"pid":123,"app_name":"Mail","bundle_id":"com.apple.mail"}}`
	resp, err := http.Post(srv.URL+"/call/start", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /call/start: %v", err)
	}
	resp.Body.Close()

	req := <-got
	if req.CWD != "/Users/hu/project" || req.ForegroundHint == nil {
		t.Fatalf("start context = %+v", req)
	}
	if req.ForegroundHint.PID != 123 || req.ForegroundHint.AppName != "Mail" || req.ForegroundHint.BundleID != "com.apple.mail" {
		t.Fatalf("foreground hint = %+v", req.ForegroundHint)
	}
}

func TestControlServerSSEDelivers(t *testing.T) {
	s := NewControlServer(nil, nil, nil)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer resp.Body.Close()

	// Wait for the subscriber to register, then emit each event variant.
	deadline := time.Now().Add(2 * time.Second)
	for s.subscriberCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	s.EmitVoiceState("listening")
	s.EmitControlApp("open_settings")
	s.EmitCallState("on_call")

	want := []string{
		`{"type":"voice_state","state":"listening"}`,
		`{"type":"control_app","action":"open_settings"}`,
		`{"type":"call_state","state":"on_call"}`,
	}
	br := bufio.NewReader(resp.Body)
	var got []string
	readDeadline := time.Now().Add(3 * time.Second)
	for len(got) < len(want) && time.Now().Before(readDeadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			break
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			got = append(got, strings.TrimSpace(data))
		}
	}
	for i, w := range want {
		if i >= len(got) || got[i] != w {
			t.Errorf("SSE line %d = %q, want %q (all got: %v)", i, safeIdx(got, i), w, got)
		}
	}
}

func safeIdx(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return "<missing>"
}
