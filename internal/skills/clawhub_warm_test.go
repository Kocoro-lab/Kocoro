package skills

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// clawhubListBody returns a minimal but valid /api/v1/skills list response with
// n items, so FetchClawHubPage's no-query branch maps real entries.
func clawhubListBody(n int) string {
	items := make([]string, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, fmt.Sprintf(
			`{"slug":"skill-%d","displayName":"Skill %d","summary":"s","stats":{"downloads":%d,"stars":0},"latestVersion":{"version":"1.0.0"}}`,
			i, i, n-i))
	}
	return `{"items":[` + strings.Join(items, ",") + `],"nextCursor":null}`
}

// clawhubPageBody returns a /api/v1/skills page with items skill-<start> ..
// skill-<start+n-1> and the given opaque next cursor ("" → null / exhausted).
func clawhubPageBody(start, n int, nextCursor string) string {
	items := make([]string, 0, n)
	for i := start; i < start+n; i++ {
		items = append(items, fmt.Sprintf(
			`{"slug":"skill-%d","displayName":"Skill %d","summary":"s","stats":{"downloads":%d,"stars":0},"latestVersion":{"version":"1.0.0"}}`,
			i, i, 1000-i))
	}
	nc := "null"
	if nextCursor != "" {
		nc = `"` + nextCursor + `"`
	}
	return `{"items":[` + strings.Join(items, ",") + `],"nextCursor":` + nc + `}`
}

// newPaginatedClawHub stands up a mock ClawHub whose /api/v1/skills endpoint
// serves 5-item pages across cursors "" -> p1 -> p2 -> (end), i.e. skill-0..14.
func newPaginatedClawHub() *httptest.Server {
	pages := map[string]string{
		"":   clawhubPageBody(0, 5, "p1"),
		"p1": clawhubPageBody(5, 5, "p2"),
		"p2": clawhubPageBody(10, 5, ""),
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := pages[r.URL.Query().Get("cursor")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
}

// TestClawHub_ExcludeInstalledFillsPage proves exclude_installed filtering plus
// bounded page refill: an entirely-installed first page is skipped and the page
// is refilled from the next upstream page, with a page-aligned resume cursor so
// nothing is duplicated or lost. "Installed" is filtered here (clawhub.ai has no
// knowledge of local installs), so this is the whole feature's core.
func TestClawHub_ExcludeInstalledFillsPage(t *testing.T) {
	ts := newPaginatedClawHub()
	defer ts.Close()

	c := NewClawHubMarketplaceClient(ts.URL, 0)
	c.retryBase = fastBase
	c.maxAttempts = 1
	c.SetClawHubCacheTTL(0)

	// Every skill on page 0 (skill-0..4) is installed. size=5 must skip page 0
	// entirely and refill from page 1 → skill-5..9.
	installed := map[string]bool{"skill-0": true, "skill-1": true, "skill-2": true, "skill-3": true, "skill-4": true}
	entries, next, _, err := c.FetchClawHubPageExcludingInstalled(context.Background(), "", "downloads", "", 5, installed, 5)
	if err != nil {
		t.Fatalf("exclude fetch: %v", err)
	}
	if len(entries) != 5 {
		t.Fatalf("got %d entries, want 5 (page-0 all installed → refilled from page 1)", len(entries))
	}
	for _, e := range entries {
		if installed[e.Slug] {
			t.Fatalf("installed skill %q leaked into results", e.Slug)
		}
	}
	// Resume cursor is the page-1 boundary (p2), never a mid-page split.
	if next != "p2" {
		t.Fatalf("next cursor = %q, want p2 (page-aligned resume)", next)
	}
}

// TestClawHub_ExcludeInstalledMaxPagesBounded proves the fill loop is bounded:
// with maxPages=1 and an all-installed first page it returns zero entries plus
// the next cursor (the client keeps paging) rather than fanning out unboundedly.
func TestClawHub_ExcludeInstalledMaxPagesBounded(t *testing.T) {
	ts := newPaginatedClawHub()
	defer ts.Close()

	c := NewClawHubMarketplaceClient(ts.URL, 0)
	c.retryBase = fastBase
	c.maxAttempts = 1
	c.SetClawHubCacheTTL(0)

	installed := map[string]bool{"skill-0": true, "skill-1": true, "skill-2": true, "skill-3": true, "skill-4": true}
	entries, next, _, err := c.FetchClawHubPageExcludingInstalled(context.Background(), "", "downloads", "", 5, installed, 1)
	if err != nil {
		t.Fatalf("exclude fetch: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("got %d entries, want 0 (maxPages=1, page-0 all installed)", len(entries))
	}
	if next != "p1" {
		t.Fatalf("next cursor = %q, want p1 (caller resumes, bounded fan-out)", next)
	}
}

// TestClawHub_WarmAndViewAgnosticFallback proves the fix for the intermittent
// "registry unreachable" (503) on the default marketplace view: after a warm
// populates the view-agnostic last-good first page, a DEFAULT first-page browse
// whose exact-URL cache is cold (different size than warmed) is served from that
// slot instead of surfacing the upstream 503 — while deep pages still error.
func TestClawHub_WarmAndViewAgnosticFallback(t *testing.T) {
	var hits int32
	var down int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if atomic.LoadInt32(&down) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(clawhubListBody(20)))
	}))
	defer ts.Close()

	c := NewClawHubMarketplaceClient(ts.URL, 0)
	c.retryBase = fastBase
	c.maxAttempts = 1       // surface a 503 immediately (no retry wait)
	c.SetClawHubCacheTTL(0) // disable exact-URL cache so ONLY the golden slot can rescue

	// Warm the default page. Populates the view-agnostic last-good first page.
	if err := c.WarmClawHub(context.Background()); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if _, ok := c.lastGoodFirstPage(); !ok {
		t.Fatal("warm did not populate the last-good first page")
	}

	// Upstream now 503s.
	atomic.StoreInt32(&down, 1)

	// Default first page with a DIFFERENT size (cold exact-URL key): served from
	// the golden slot despite the 503, with next="" (single stale page).
	entries, next, stale, err := c.FetchClawHubPage(context.Background(), "", "downloads", "", 40)
	if err != nil {
		t.Fatalf("default first-page browse should fall back, got error: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("fallback returned no entries")
	}
	if next != "" {
		t.Fatalf("fallback next cursor = %q, want empty (single stale page)", next)
	}
	// This fetch reports stale so the handler can set X-Cache-Stale.
	if !stale {
		t.Fatal("golden-slot fallback fetch should report stale=true")
	}

	// A DEEP page (cursor set) has no fallback and must propagate the 503.
	if _, _, _, err := c.FetchClawHubPage(context.Background(), "", "downloads", "somecursor", 20); err == nil {
		t.Fatal("deep-page browse during outage should error, got nil")
	}

	// Recovery: a fresh default browse reports stale=false.
	atomic.StoreInt32(&down, 0)
	_, _, stale, err = c.FetchClawHubPage(context.Background(), "", "downloads", "", 20)
	if err != nil {
		t.Fatalf("recovery browse: %v", err)
	}
	if stale {
		t.Fatal("a fresh default browse should report stale=false")
	}
}

// TestClawHub_FallbackNotForDefinitive4xx confirms finding #1's fix: a
// definitive 4xx (endpoint gone / bad param) is NOT masked by the golden slot —
// it must surface immediately rather than serve up-to-30-min-stale data.
func TestClawHub_FallbackNotForDefinitive4xx(t *testing.T) {
	var code int32 = http.StatusOK
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c := atomic.LoadInt32(&code); c != http.StatusOK {
			w.WriteHeader(int(c))
			return
		}
		_, _ = w.Write([]byte(clawhubListBody(20)))
	}))
	defer ts.Close()

	c := NewClawHubMarketplaceClient(ts.URL, 0)
	c.retryBase = fastBase
	c.maxAttempts = 1
	c.SetClawHubCacheTTL(0)

	// Warm so the golden slot IS populated — proving the 404 surfaces despite an
	// available fallback.
	if err := c.WarmClawHub(context.Background()); err != nil {
		t.Fatalf("warm: %v", err)
	}

	atomic.StoreInt32(&code, http.StatusNotFound)
	_, _, stale, err := c.FetchClawHubPage(context.Background(), "", "downloads", "", 40)
	if err == nil {
		t.Fatal("definitive 404 must surface, not be masked by the golden slot")
	}
	if !strings.Contains(err.Error(), "status 404") {
		t.Fatalf("want status 404 error, got: %v", err)
	}
	if stale {
		t.Fatal("a surfaced 404 must not report stale")
	}

	// A 403 (WAF/geo block) / 410 (gone) is likewise definitive and must surface,
	// NOT be masked by the golden slot — the prior blacklist wrongly treated any
	// non-{400,404,409,422} status as transient.
	atomic.StoreInt32(&code, http.StatusForbidden)
	if _, _, _, err := c.FetchClawHubPage(context.Background(), "", "downloads", "", 40); err == nil ||
		!strings.Contains(err.Error(), "status 403") {
		t.Fatalf("definitive 403 must surface, got: %v", err)
	}

	// A transient 503, by contrast, IS masked by the golden slot.
	atomic.StoreInt32(&code, http.StatusServiceUnavailable)
	if _, _, stale, err := c.FetchClawHubPage(context.Background(), "", "downloads", "", 40); err != nil {
		t.Fatalf("transient 503 should fall back to the golden slot, got: %v", err)
	} else if !stale {
		t.Fatal("503 golden-slot fallback should report stale=true")
	}
}

// TestClawHub_NoFallbackBeforeWarm confirms the honest limitation: with an empty
// slot (never warmed / never a prior success), a default browse during an outage
// still errors — there is nothing to serve.
func TestClawHub_NoFallbackBeforeWarm(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	c := NewClawHubMarketplaceClient(ts.URL, 0)
	c.retryBase = fastBase
	c.maxAttempts = 1
	c.SetClawHubCacheTTL(0)

	if _, _, _, err := c.FetchClawHubPage(context.Background(), "", "downloads", "", 20); err == nil {
		t.Fatal("cold default browse during outage should error before any warm")
	}
}
