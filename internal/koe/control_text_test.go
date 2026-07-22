package koe

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCallTextEndpoint(t *testing.T) {
	var got []string
	s := NewControlServer(nil, nil, nil)
	s.SetTextHandler(func(text string) error {
		got = append(got, text)
		if text == "no-call" {
			return ErrNoActiveCall
		}
		return nil
	})
	// The route gates on the emitted call_state authority before consulting the
	// handler; put the server on-call so the handler-level cases below run.
	s.EmitCallState("on_call")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	post := func(body string) (int, string) {
		resp, err := http.Post(srv.URL+"/call/text", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST /call/text %q: %v", body, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}

	// 202 on success — the handler receives the text.
	if code, _ := post(`{"text":"hello there"}`); code != http.StatusAccepted {
		t.Fatalf("valid text: status=%d, want 202", code)
	}
	if len(got) != 1 || got[0] != "hello there" {
		t.Fatalf("handler saw %v, want [hello there]", got)
	}

	// 400 on empty text (and on a missing text field / empty body).
	for _, body := range []string{`{"text":""}`, `{}`, ``} {
		if code, _ := post(body); code != http.StatusBadRequest {
			t.Fatalf("empty text %q: status=%d, want 400", body, code)
		}
	}

	// 400 when the text exceeds the byte cap.
	oversized := `{"text":"` + strings.Repeat("a", maxInjectTextBytes+1) + `"}`
	if code, _ := post(oversized); code != http.StatusBadRequest {
		t.Fatalf("oversized text: status=%d, want 400", code)
	}
	// Exactly at the cap is allowed.
	if code, _ := post(`{"text":"` + strings.Repeat("a", maxInjectTextBytes) + `"}`); code != http.StatusAccepted {
		t.Fatalf("text at cap: status=%d, want 202", code)
	}

	// 409 when the handler reports no active call.
	if code, body := post(`{"text":"no-call"}`); code != http.StatusConflict || !strings.Contains(body, "no_active_call") {
		t.Fatalf("no-call text: status=%d body=%s, want 409 no_active_call", code, body)
	}
}

// TestCallTextGatedByCallStateAuthority pins POST /call/text to the same store
// the SSE call_state events (and thus Desktop) read: outside an emitted
// connecting/on_call span the route must 409 no_active_call without consulting
// the handler — the handler's private call flag is not the authority.
func TestCallTextGatedByCallStateAuthority(t *testing.T) {
	delivered := 0
	s := NewControlServer(nil, nil, nil)
	// A handler that would accept — simulating a stale seam view.
	s.SetTextHandler(func(text string) error { delivered++; return nil })
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	post := func() (int, string) {
		resp, err := http.Post(srv.URL+"/call/text", "application/json", strings.NewReader(`{"text":"hi"}`))
		if err != nil {
			t.Fatalf("POST /call/text: %v", err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}

	// Fresh server: no call span has been emitted → refuse.
	if code, body := post(); code != http.StatusConflict || !strings.Contains(body, "no_active_call") {
		t.Fatalf("text before any call: status=%d body=%s, want 409 no_active_call", code, body)
	}
	if delivered != 0 {
		t.Fatalf("text handler ran outside a call")
	}

	// Emitted call span → the handler decides.
	s.EmitCallState("on_call")
	if code, _ := post(); code != http.StatusAccepted {
		t.Fatalf("text during on_call: status=%d, want 202", code)
	}
	if delivered != 1 {
		t.Fatalf("text handler ran %d times during call, want 1", delivered)
	}

	// "ended" normalizes back to idle → refuse again.
	s.EmitCallState("ended")
	if code, body := post(); code != http.StatusConflict || !strings.Contains(body, "no_active_call") {
		t.Fatalf("text after ended: status=%d body=%s, want 409 no_active_call", code, body)
	}
	if delivered != 1 {
		t.Fatalf("text handler ran after call ended")
	}
}

// TestCallTextNilHandlerConflict verifies that with no text handler wired the route
// reports 409 no_active_call (the injection facility is not plumbed to a session).
func TestCallTextNilHandlerConflict(t *testing.T) {
	s := NewControlServer(nil, nil, nil)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/call/text", "application/json", strings.NewReader(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("POST /call/text: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "no_active_call") {
		t.Fatalf("nil text handler: status=%d body=%s, want 409 no_active_call", resp.StatusCode, body)
	}
}
