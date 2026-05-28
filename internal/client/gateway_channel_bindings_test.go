package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestListChannelBindings_FetchAndDecode covers the cold-cache path: a GET to
// /api/v1/channels carrying the API key, decoding the wire shape into
// ChannelBinding values.
func TestListChannelBindings_FetchAndDecode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/channels" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Errorf("expected X-API-Key=test-key, got %s", r.Header.Get("X-API-Key"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[
			{"type":"slack","name":"kocoro-test-slack","enabled":true,"config":{"agent_name":"researcher"}},
			{"type":"line","name":"my-line","enabled":false}
		]`))
	}))
	defer server.Close()

	gw := NewGatewayClient(server.URL, "test-key")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := gw.ListChannelBindings(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 bindings, got %d", len(got))
	}
	if got[0].Type != "slack" || got[0].Name != "kocoro-test-slack" || !got[0].Enabled {
		t.Errorf("binding[0] decoded wrong: %+v", got[0])
	}
	if name := ChannelBindingAgentName(got[0]); name != "researcher" {
		t.Errorf("expected agent_name researcher, got %q", name)
	}
	if got[1].Type != "line" || got[1].Enabled {
		t.Errorf("binding[1] decoded wrong: %+v", got[1])
	}
}

// TestListChannelBindings_CacheHit asserts a second call within the TTL is
// served from cache and never reaches the server.
func TestListChannelBindings_CacheHit(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`[{"type":"slack","name":"s","enabled":true}]`))
	}))
	defer server.Close()

	gw := NewGatewayClient(server.URL, "k")
	ctx := context.Background()

	if _, err := gw.ListChannelBindings(ctx); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := gw.ListChannelBindings(ctx); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("expected exactly 1 HTTP fetch (second served from cache), got %d", n)
	}
}

// TestListChannelBindings_CacheExpiry forces the cached-at timestamp past the
// TTL and asserts the next call refetches.
func TestListChannelBindings_CacheExpiry(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`[{"type":"slack","name":"s","enabled":true}]`))
	}))
	defer server.Close()

	gw := NewGatewayClient(server.URL, "k")
	ctx := context.Background()

	if _, err := gw.ListChannelBindings(ctx); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Age the cache past the TTL.
	gw.bindingsMu.Lock()
	gw.bindingsCachedAt = time.Now().Add(-channelBindingsTTL - time.Second)
	gw.bindingsMu.Unlock()

	if _, err := gw.ListChannelBindings(ctx); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("expected 2 HTTP fetches (cache expired), got %d", n)
	}
}

// TestListChannelBindings_ServerError asserts a non-200 surfaces as an error
// and does not poison the cache (a later successful call still fetches).
func TestListChannelBindings_ServerError(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("boom"))
			return
		}
		w.Write([]byte(`[{"type":"slack","name":"s","enabled":true}]`))
	}))
	defer server.Close()

	gw := NewGatewayClient(server.URL, "k")
	ctx := context.Background()

	if _, err := gw.ListChannelBindings(ctx); err == nil {
		t.Fatal("expected error on 500")
	}
	// The failed fetch must not have written the cache — flip the server to
	// success and confirm the next call actually re-fetches and succeeds.
	fail.Store(false)
	got, err := gw.ListChannelBindings(ctx)
	if err != nil {
		t.Fatalf("recovery call: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("expected 1 binding after recovery, got %d", len(got))
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("expected 2 fetches (error not cached), got %d", n)
	}
}

// TestListChannelBindings_EmptyIsNotError covers the legitimate "no IM bound"
// state — an empty array decodes to a nil/empty slice with no error.
func TestListChannelBindings_EmptyIsNotError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	}))
	defer server.Close()

	gw := NewGatewayClient(server.URL, "k")
	got, err := gw.ListChannelBindings(context.Background())
	if err != nil {
		t.Fatalf("empty bindings must not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 bindings, got %d", len(got))
	}
}
