//go:build darwin && cgo

package koe

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSessionConfigWatcherAcksOnSessionUpdated: a clean session.updated closes the
// handshake, fires onConfigured exactly once, and wait returns nil.
func TestSessionConfigWatcherAcksOnSessionUpdated(t *testing.T) {
	var onCfg int
	w := newSessionConfigWatcher(func() { onCfg++ })
	w.observe([]byte(`{"type":"session.updated"}`))
	if err := w.wait(context.Background(), time.Second); err != nil {
		t.Fatalf("wait after session.updated = %v, want nil", err)
	}
	if onCfg != 1 {
		t.Fatalf("onConfigured called %d times, want 1", onCfg)
	}
}

// TestSessionConfigWatcherFailsOnPreAckError pins S3: an error before the ack (a
// rejected session.update) must surface as a Connect error carrying the server
// reason, NOT wedge the call in "connecting".
func TestSessionConfigWatcherFailsOnPreAckError(t *testing.T) {
	w := newSessionConfigWatcher(nil)
	w.observe([]byte(`{"type":"error","error":{"code":"invalid_value","message":"unknown model"}}`))
	err := w.wait(context.Background(), time.Second)
	if err == nil {
		t.Fatal("rejected session.update must surface as a Connect error, not a wedge")
	}
	if !strings.Contains(err.Error(), "invalid_value") {
		t.Fatalf("error should carry the server code, got %v", err)
	}
}

// TestSessionConfigWatcherIgnoresErrorAfterAck: an error AFTER session.updated is a
// mid-call error, not a config failure — it must not turn a live session into one.
func TestSessionConfigWatcherIgnoresErrorAfterAck(t *testing.T) {
	w := newSessionConfigWatcher(nil)
	w.observe([]byte(`{"type":"session.updated"}`))
	w.observe([]byte(`{"type":"error","error":{"code":"mid_call"}}`))
	if err := w.wait(context.Background(), time.Second); err != nil {
		t.Fatalf("post-ack error must not fail a configured session: %v", err)
	}
}

// TestSessionConfigWatcherTimesOut: a silent session.update (neither ack nor error)
// must time out rather than block forever.
func TestSessionConfigWatcherTimesOut(t *testing.T) {
	w := newSessionConfigWatcher(nil)
	err := w.wait(context.Background(), 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("silent session.update must time out, got %v", err)
	}
}

// TestSessionConfigWatcherCtxCancel: a cancelled ctx unblocks wait with ctx.Err().
func TestSessionConfigWatcherCtxCancel(t *testing.T) {
	w := newSessionConfigWatcher(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.wait(ctx, time.Second); err == nil {
		t.Fatal("cancelled ctx must unblock wait with an error")
	}
}

// TestSendTrackStatsSegmentsAndTotals pins the send-side accounting used to
// reconcile "gate passed N frames" with "track actually wrote M frames" — the
// counters that rule WriteSample/encode failures in or out when the server goes
// deaf mid-call (2026-07-02).
func TestSendTrackStatsSegmentsAndTotals(t *testing.T) {
	var s sendTrackStats
	s.beginSegment(10) // gate had already passed 10 frames before this segment
	s.noteWrite(nil)
	s.noteWrite(nil)
	s.noteWrite(errors.New("track closed"))
	s.noteEncodeErr()

	seg := s.segmentLine(14) // gate passed 4 more frames during the segment
	for _, want := range []string{"gate_passed=4", "written=2", "write_err=1", "encode_err=1"} {
		if !strings.Contains(seg, want) {
			t.Fatalf("segment line missing %q, got %q", want, seg)
		}
	}

	totals := s.totalsLine()
	for _, want := range []string{"written=2", "write_err=1", "encode_err=1"} {
		if !strings.Contains(totals, want) {
			t.Fatalf("totals line missing %q, got %q", want, totals)
		}
	}
}

func TestMintEphemeralRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer dev-key" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		var body struct {
			Session struct {
				Type  string `json:"type"`
				Model string `json:"model"`
			} `json:"session"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Session.Type != "realtime" || !strings.Contains(body.Session.Model, "gpt-realtime") {
			t.Errorf("bad mint body: %+v", body.Session)
		}
		json.NewEncoder(w).Encode(map[string]any{"value": "ek_test123"})
	}))
	defer srv.Close()

	ek, err := mintEphemeralAt(context.Background(), srv.URL, "dev-key", "gpt-realtime-2.1-mini")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if ek != "ek_test123" {
		t.Errorf("ek = %q, want ek_test123", ek)
	}
}
