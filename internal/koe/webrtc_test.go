package koe

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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

	ek, err := mintEphemeralAt(context.Background(), srv.URL, "dev-key", "gpt-realtime-mini-2025-12-15")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if ek != "ek_test123" {
		t.Errorf("ek = %q, want ek_test123", ek)
	}
}
