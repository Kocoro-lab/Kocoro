package koe

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestPerceptionClientPollsFaceAndDOA(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/media/tracking/face", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","face_target":{"detected":true,"x":0.25,"y":-0.1,"roll":0.02,"ts":42.5}}`))
	})
	mux.HandleFunc("/api/state/doa", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"angle":1.5,"speech_detected":true}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	now := time.Unix(100, 0)
	c, err := NewPerceptionClient(srv.URL, WithPerceptionNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	s := c.Poll(t.Context())
	if s.Health != PerceptionOK {
		t.Fatalf("health = %s, error = %q", s.Health, s.Error)
	}
	if !s.Face.Available || !s.Face.Fresh || !s.Face.Detected || s.Face.X != 0.25 {
		t.Fatalf("face = %+v", s.Face)
	}
	if !s.DOA.Available || !s.DOA.Fresh || !s.DOA.SpeechDetected || s.DOA.Angle != 1.5 {
		t.Fatalf("doa = %+v", s.DOA)
	}
}

func TestPerceptionClientDetectsStoppedFaceTimestamp(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/media/tracking/face", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","face_target":{"detected":false,"x":null,"y":null,"roll":null,"ts":42.5}}`))
	})
	mux.HandleFunc("/api/state/doa", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"angle":1.5,"speech_detected":false}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	now := time.Unix(100, 0)
	c, err := NewPerceptionClient(srv.URL, WithPerceptionNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Poll(t.Context()).Health; got != PerceptionOK {
		t.Fatalf("first health = %s", got)
	}
	now = now.Add(defaultFaceStaleTimeout + time.Millisecond)
	s := c.Poll(t.Context())
	if s.Health != PerceptionFaceStale || s.Face.Fresh {
		t.Fatalf("stale snapshot = %+v", s)
	}
}

func TestPerceptionClientFailsClosedOnNullAndInvalidPayloads(t *testing.T) {
	tests := []struct {
		name string
		face string
		doa  string
		want PerceptionHealth
	}{
		{"tracking disabled", `{"status":"ok","face_target":{"detected":false,"x":null,"y":null,"roll":null,"ts":null}}`, `{"angle":1.5,"speech_detected":false}`, PerceptionFaceStale},
		{"doa unavailable", `{"status":"ok","face_target":{"detected":false,"x":null,"y":null,"roll":null,"ts":1}}`, `null`, PerceptionDOAUnavailable},
		{"face out of range", `{"status":"ok","face_target":{"detected":true,"x":2,"y":0,"roll":0,"ts":1}}`, `{"angle":1.5,"speech_detected":false}`, PerceptionInvalidPayload},
		{"doa out of range", `{"status":"ok","face_target":{"detected":false,"x":null,"y":null,"roll":null,"ts":1}}`, `{"angle":4,"speech_detected":false}`, PerceptionInvalidPayload},
		{"missing face target", `{"status":"ok"}`, `{"angle":1.5,"speech_detected":false}`, PerceptionInvalidPayload},
		{"missing doa field", `{"status":"ok","face_target":{"detected":false,"x":null,"y":null,"roll":null,"ts":1}}`, `{"angle":1.5}`, PerceptionInvalidPayload},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/api/media/tracking/face", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.face))
			})
			mux.HandleFunc("/api/state/doa", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.doa))
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			c, err := NewPerceptionClient(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			if got := c.Poll(t.Context()).Health; got != tc.want {
				t.Fatalf("health = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestNewPerceptionClientRejectsBadBaseURL(t *testing.T) {
	for _, raw := range []string{"", "127.0.0.1:8000", "file:///tmp/daemon", "http://127.0.0.1:8000/prefix"} {
		if _, err := NewPerceptionClient(raw); err == nil {
			t.Errorf("NewPerceptionClient(%q) should fail", raw)
		}
	}
}

func TestPerceptionClientBoundsSlowPollAndRecovers(t *testing.T) {
	var slow atomic.Bool
	slow.Store(true)
	var active atomic.Int32
	var maxActive atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/media/tracking/face", func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for current > maxActive.Load() && !maxActive.CompareAndSwap(maxActive.Load(), current) {
		}
		if slow.Load() {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","face_target":{"detected":false,"x":null,"y":null,"roll":null,"ts":10}}`))
	})
	mux.HandleFunc("/api/state/doa", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"angle":1.5,"speech_detected":false}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, err := NewPerceptionClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	c.http.Timeout = 20 * time.Millisecond
	if got := c.Poll(t.Context()).Health; got != PerceptionDaemonUnreachable {
		t.Fatalf("slow poll health = %s", got)
	}
	slow.Store(false)
	if got := c.Poll(t.Context()).Health; got != PerceptionOK {
		t.Fatalf("recovery health = %s", got)
	}
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("overlapping requests = %d, want 1", got)
	}
}
