package skills

import (
	"sync"
	"time"
)

// defaultClawHubCacheTTL is how long a ClawHub catalog response (browse page,
// search, detail, file list, file body) is reused before re-hitting clawhub.ai.
// Short by design: it absorbs request bursts and repeat browsing (the 50-request
// load test that exposed clawhub.ai's ~22% 503s) without letting the catalog go
// noticeably stale. Tunable via skills.marketplace.clawhub_cache_ttl_secs.
const defaultClawHubCacheTTL = 60 * time.Second

// clawhubCacheMaxEntries caps the per-URL ClawHub response cache. Each entry is
// one browse page / search / detail / file-list / file body keyed by full URL.
// 256 covers heavy browsing (many pages × sorts × queries) within the short TTL;
// symptom if too low: cache thrash under very wide browsing (more upstream
// calls, never incorrect results). It's a flat-memory guard, not a workload
// knob — raise here if a deployment browses far more than 256 distinct URLs
// within one TTL window.
const clawhubCacheMaxEntries = 256

// clawhubFirstPageMaxAge bounds how old the view-agnostic last-good default
// browse page (MarketplaceClient.firstPage) may be and still be served as an
// outage fallback. Workload: a daemon that warmed the default page then sat
// idle for a long stretch while clawhub.ai went down — serving a page up to
// 30min old beats the "registry unreachable" error. Symptom if it binds:
// during a sustained clawhub outage the default view shows the error again
// once the last-good page ages past this. Not a hot-path knob; deliberately a
// const (raise here if a deployment wants a longer outage grace).
const clawhubFirstPageMaxAge = 30 * time.Minute

type clawhubCacheEntry struct {
	data       []byte
	fetchedAt  time.Time
	retryAfter time.Time // stale cooldown: serve data without re-hitting upstream until this
}

// clawhubCache is a small TTL cache of raw ClawHub response bodies keyed by full
// request URL (which already encodes q/sort/cursor/limit/slug/owner/version/path).
// On an upstream failure a still-cached body is served as stale for a cooldown
// window, mirroring the static-registry stale-on-error behavior. ttl<=0 disables
// the cache entirely (used by tests that assert raw upstream hit counts).
type clawhubCache struct {
	mu       sync.Mutex
	ttl      time.Duration
	cooldown time.Duration
	m        map[string]*clawhubCacheEntry
}

func newClawHubCache(ttl, cooldown time.Duration) *clawhubCache {
	return &clawhubCache{ttl: ttl, cooldown: cooldown, m: map[string]*clawhubCacheEntry{}}
}

// lookup returns cached data when it should be served without an upstream call:
// either fresh (within ttl) or within an active stale-cooldown after a failure.
func (c *clawhubCache) lookup(url string) ([]byte, bool) {
	if c == nil || c.ttl <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.m[url]
	if e == nil {
		return nil, false
	}
	if time.Since(e.fetchedAt) < c.ttl {
		return e.data, true // fresh
	}
	if !e.retryAfter.IsZero() && time.Now().Before(e.retryAfter) {
		return e.data, true // expired but within stale cooldown
	}
	return nil, false
}

// store caches a freshly-fetched body, evicting the oldest entry when full.
func (c *clawhubCache) store(url string, data []byte) {
	if c == nil || c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.m[url]; !exists && len(c.m) >= clawhubCacheMaxEntries {
		var oldestKey string
		var oldestAt time.Time
		for k, e := range c.m {
			if oldestKey == "" || e.fetchedAt.Before(oldestAt) {
				oldestKey, oldestAt = k, e.fetchedAt
			}
		}
		delete(c.m, oldestKey)
	}
	c.m[url] = &clawhubCacheEntry{data: data, fetchedAt: time.Now()}
}

// staleOnError, called after an upstream failure, returns the cached body (if
// any) and arms a stale-cooldown so subsequent requests keep serving it without
// hammering the failing upstream.
func (c *clawhubCache) staleOnError(url string) ([]byte, bool) {
	if c == nil || c.ttl <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.m[url]
	if e == nil {
		return nil, false
	}
	e.retryAfter = time.Now().Add(c.cooldown)
	return e.data, true
}
