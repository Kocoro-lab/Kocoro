package skills

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newCacheTestServer(hits *int32, status func() int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		if code := status(); code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		_, _ = w.Write([]byte("payload"))
	}))
}

func TestClawHubCache_HitWithinTTL(t *testing.T) {
	var hits int32
	ts := newCacheTestServer(&hits, func() int { return http.StatusOK })
	defer ts.Close()

	c := NewClawHubMarketplaceClient(ts.URL, 0)
	c.retryBase = fastBase // default cache TTL (60s) applies

	for i := 0; i < 3; i++ {
		b, err := c.clawhubGet(context.Background(), ts.URL, 1<<20)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if string(b) != "payload" {
			t.Fatalf("call %d: body = %q", i, b)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("hits = %d, want 1 (2nd/3rd served from cache)", got)
	}
}

func TestClawHubCache_DisabledWhenTTLZero(t *testing.T) {
	var hits int32
	ts := newCacheTestServer(&hits, func() int { return http.StatusOK })
	defer ts.Close()

	c := NewClawHubMarketplaceClient(ts.URL, 0)
	c.retryBase = fastBase
	c.SetClawHubCacheTTL(0) // disable

	for i := 0; i < 3; i++ {
		if _, err := c.clawhubGet(context.Background(), ts.URL, 1<<20); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("hits = %d, want 3 (cache disabled)", got)
	}
}

func TestClawHubCache_StaleOnError(t *testing.T) {
	var hits int32
	var down int32
	ts := newCacheTestServer(&hits, func() int {
		if atomic.LoadInt32(&down) == 1 {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	})
	defer ts.Close()

	c := NewClawHubMarketplaceClient(ts.URL, 0)
	c.retryBase = fastBase
	c.maxAttempts = 2
	c.SetClawHubCacheTTL(time.Millisecond) // tiny TTL so the entry expires quickly

	// Prime the cache.
	if _, err := c.clawhubGet(context.Background(), ts.URL, 1<<20); err != nil {
		t.Fatalf("prime: %v", err)
	}
	time.Sleep(5 * time.Millisecond) // let the fresh window lapse
	atomic.StoreInt32(&down, 1)

	// Upstream now 503s; the expired-but-cached body is served as stale.
	b, err := c.clawhubGet(context.Background(), ts.URL, 1<<20)
	if err != nil {
		t.Fatalf("stale serve should succeed, got: %v", err)
	}
	if string(b) != "payload" {
		t.Fatalf("stale body = %q, want primed payload", b)
	}

	// Within the stale cooldown, a further call is served without hitting upstream.
	hitsAfterStale := atomic.LoadInt32(&hits)
	if _, err := c.clawhubGet(context.Background(), ts.URL, 1<<20); err != nil {
		t.Fatalf("cooldown serve: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != hitsAfterStale {
		t.Fatalf("cooldown should suppress upstream calls: hits went %d → %d", hitsAfterStale, got)
	}
}

func TestClawHubCache_4xxNotCached(t *testing.T) {
	var hits int32
	var code int32 = http.StatusNotFound
	ts := newCacheTestServer(&hits, func() int { return int(atomic.LoadInt32(&code)) })
	defer ts.Close()

	c := NewClawHubMarketplaceClient(ts.URL, 0)
	c.retryBase = fastBase

	// 404 surfaces and is NOT cached.
	if _, err := c.clawhubGet(context.Background(), ts.URL, 1<<20); err == nil || !strings.Contains(err.Error(), "status 404") {
		t.Fatalf("want status 404 error, got %v", err)
	}
	// Upstream recovers → next call must re-fetch (no cached 404) and succeed.
	atomic.StoreInt32(&code, http.StatusOK)
	if b, err := c.clawhubGet(context.Background(), ts.URL, 1<<20); err != nil || string(b) != "payload" {
		t.Fatalf("recovery fetch: body=%q err=%v", b, err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("hits = %d, want 2 (404 not served from cache)", got)
	}
}
