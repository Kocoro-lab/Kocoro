package koe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// captureAuthServer records the Authorization header of the last request and
// answers the mint endpoint so MintViaDaemon completes.
func captureAuthServer(t *testing.T, gotAuth *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":"ek_test"}`))
	}))
}

func TestDaemonClient_AttachesBearerWhenTokenSet(t *testing.T) {
	var gotAuth string
	srv := captureAuthServer(t, &gotAuth)
	defer srv.Close()

	c := NewDaemonClient(srv.URL)
	c.SetToken("s3cr3t")
	if _, err := c.MintViaDaemon(context.Background(), "gpt-realtime"); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if gotAuth != "Bearer s3cr3t" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer s3cr3t")
	}
}

func TestDaemonClient_NoAuthHeaderWhenTokenEmpty(t *testing.T) {
	var gotAuth string
	srv := captureAuthServer(t, &gotAuth)
	defer srv.Close()

	c := NewDaemonClient(srv.URL) // no SetToken → localhost path, no bearer
	if _, err := c.MintViaDaemon(context.Background(), "gpt-realtime"); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization = %q, want empty (no token configured)", gotAuth)
	}
}
