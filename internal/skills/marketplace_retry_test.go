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

// fastBase keeps backoff negligible so retry tests don't sleep for real.
const fastBase = time.Millisecond

func TestIsRetryableStatus(t *testing.T) {
	retryable := []int{429, 500, 502, 503, 504}
	for _, c := range retryable {
		if !isRetryableStatus(c) {
			t.Errorf("status %d should be retryable", c)
		}
	}
	for _, c := range []int{200, 201, 301, 400, 401, 403, 404, 409, 422} {
		if isRetryableStatus(c) {
			t.Errorf("status %d should NOT be retryable", c)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := map[string]time.Duration{
		"":            0,
		"5":           5 * time.Second,
		"0":           0,
		"-3":          0,
		"abc":         0,
		"Wed, 21 Oct": 0, // HTTP-date form deliberately unsupported → 0
		"  7  ":       7 * time.Second,
	}
	for in, want := range cases {
		h := http.Header{}
		if in != "" {
			h.Set("Retry-After", in)
		}
		if got := parseRetryAfter(h); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestDoGETWithRetry_RetriesThenSucceeds(t *testing.T) {
	var hits int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()

	resp, err := doGETWithRetry(context.Background(), http.DefaultClient, ts.URL, 3, fastBase, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("hits = %d, want 3 (2 retries)", got)
	}
}

func TestDoGETWithRetry_ExhaustedReturnsLastResponse(t *testing.T) {
	var hits int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	// On exhaustion the helper returns the final (still-503) response so the
	// caller can produce its own "status %d" error — it does NOT swallow it.
	resp, err := doGETWithRetry(context.Background(), http.DefaultClient, ts.URL, 3, fastBase, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("hits = %d, want 3", got)
	}
}

func TestDoGETWithRetry_NoRetryOn404(t *testing.T) {
	var hits int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	resp, err := doGETWithRetry(context.Background(), http.DefaultClient, ts.URL, 3, fastBase, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("hits = %d, want 1 (404 is not retried)", got)
	}
}

func TestDoGETWithRetry_ContextCancelStopsRetry(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	if _, err := doGETWithRetry(ctx, http.DefaultClient, ts.URL, 5, time.Second, 0); err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestDoGETWithRetry_BudgetBoundsTotal(t *testing.T) {
	var hits int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		time.Sleep(100 * time.Millisecond) // slower than the budget below
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	// 20 attempts would run for seconds without a budget; a 50ms budget must
	// abort the whole call (here mid first request) well before that.
	start := time.Now()
	_, err := doGETWithRetry(context.Background(), http.DefaultClient, ts.URL, 20, fastBase, 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected error when budget is exceeded")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("budget not enforced: call took %v", elapsed)
	}
	if got := atomic.LoadInt32(&hits); got >= 20 {
		t.Fatalf("budget should have cut retries short, but got %d attempts", got)
	}
}

// TestClawHubFetch_RetryExhaustedPreservesStatusString proves the ClawHub path
// retries transient 503s AND that the surfaced error still contains "status
// 503" — the substring the daemon greps to split 404 vs 503.
func TestClawHubFetch_RetryExhaustedPreservesStatusString(t *testing.T) {
	var hits int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	client := NewClawHubMarketplaceClient(ts.URL, 0)
	client.maxAttempts = 3
	client.retryBase = fastBase

	_, _, err := client.FetchClawHubPage(context.Background(), "", "", "", 10)
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if !strings.Contains(err.Error(), "status 503") {
		t.Fatalf("error %q must contain \"status 503\" (daemon 404/503 split depends on it)", err)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("hits = %d, want 3", got)
	}
}

func TestClawHubFetch_NoRetryOn404PreservesStatusString(t *testing.T) {
	var hits int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	client := NewClawHubMarketplaceClient(ts.URL, 0)
	client.maxAttempts = 3
	client.retryBase = fastBase

	_, _, err := client.FetchClawHubPage(context.Background(), "", "", "", 10)
	if err == nil || !strings.Contains(err.Error(), "status 404") {
		t.Fatalf("expected error containing \"status 404\", got %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("hits = %d, want 1 (404 is not retried)", got)
	}
}

// TestMarketplaceLoad_RetriesRegistryThenSucceeds proves the static-registry
// fetch path retries transient failures and Load succeeds (rather than falling
// back to stale) once a retry lands a good response.
func TestMarketplaceLoad_RetriesRegistryThenSucceeds(t *testing.T) {
	var hits int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"version":1,"skills":[{"slug":"demo","name":"demo","description":"d","author":"a","repo":"r"}]}`))
	}))
	defer ts.Close()

	client := NewMarketplaceClient(ts.URL, 0)
	client.maxAttempts = 3
	client.retryBase = fastBase

	idx, err := client.Load(context.Background())
	if err != nil {
		t.Fatalf("Load should succeed after retries: %v", err)
	}
	if len(idx.Skills) != 1 || idx.Skills[0].Slug != "demo" {
		t.Fatalf("unexpected index: %+v", idx)
	}
	if client.IsStale() {
		t.Error("Load succeeded via retry; should not be marked stale")
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("hits = %d, want 3", got)
	}
}

func TestSetRetryPolicy_IgnoresNonPositive(t *testing.T) {
	client := NewClawHubMarketplaceClient("https://example.test", 0)
	client.SetRetryPolicy(0, 0) // both non-positive → keep defaults
	if client.maxAttempts != defaultMarketplaceMaxAttempts {
		t.Errorf("maxAttempts = %d, want default %d", client.maxAttempts, defaultMarketplaceMaxAttempts)
	}
	if client.retryBase != defaultMarketplaceRetryBase {
		t.Errorf("retryBase = %v, want default %v", client.retryBase, defaultMarketplaceRetryBase)
	}
	client.SetRetryPolicy(5, 2*time.Second)
	if client.maxAttempts != 5 || client.retryBase != 2*time.Second {
		t.Errorf("override not applied: attempts=%d base=%v", client.maxAttempts, client.retryBase)
	}
}
