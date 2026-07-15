package koe

import (
	"os"
	"testing"
	"time"
)

// TestPerceptionClientLive is an opt-in read-only probe against a real Reachy
// daemon. Tracking must already be enabled by the app supervisor (or explicitly
// for a dev E2E); the test never mutates daemon state.
//
//	KOE_REACHY_DAEMON_URL=http://192.168.x.x:8000 \
//	  go test ./internal/koe -run TestPerceptionClientLive -v
func TestPerceptionClientLive(t *testing.T) {
	baseURL := os.Getenv("KOE_REACHY_DAEMON_URL")
	if baseURL == "" {
		t.Skip("set KOE_REACHY_DAEMON_URL for the opt-in live probe")
	}
	c, err := NewPerceptionClient(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		s := c.Poll(t.Context())
		t.Logf("sample=%d health=%s face_available=%t face_fresh=%t face_detected=%t doa_available=%t speech=%t",
			i+1, s.Health, s.Face.Available, s.Face.Fresh, s.Face.Detected, s.DOA.Available, s.DOA.SpeechDetected)
		if s.Health != PerceptionOK {
			t.Fatalf("live perception unhealthy: health=%s error=%s", s.Health, s.Error)
		}
		time.Sleep(defaultPerceptionPollInterval)
	}
}
