package skills

import (
	"context"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Marketplace retry policy. ClawHub (clawhub.ai) and the static GitHub registry
// occasionally return transient 5xx/429 under load (a 50-request load test saw
// ~22% 503s); with no client retry a single upstream blip surfaced as a
// user-visible "marketplace unavailable". These bound the in-client retry that
// absorbs those blips. Symptom when they bind: browse/install fails with
// "status 503" only after exhausting attempts (vs. immediately before).
// Override the catalog-GET values via config.yaml
// (skills.marketplace.max_attempts / .retry_base_backoff_secs); the install
// (zip) path uses these constants directly.
const (
	defaultMarketplaceMaxAttempts = 3
	defaultMarketplaceRetryBase   = 1 * time.Second
	// marketplaceRetryMaxDelay caps a single backoff sleep. This is a hard
	// safety ceiling (overflow / hostile-or-huge Retry-After guard), NOT a
	// workload cap an operator tunes — backoff *shape* is tuned via
	// retry_base_backoff_secs; this only bounds one sleep. A server Retry-After
	// above 30s is re-honored on the next attempt anyway, so clamping here is
	// harmless. Deliberately a const, not a viper knob.
	marketplaceRetryMaxDelay = 30 * time.Second
	// marketplaceCatalogRetryBudget bounds the TOTAL wall-clock of a catalog
	// GET across all attempts (incl. backoff), so a hard outage where every
	// attempt hangs to the 15s client timeout fails in ~one extra attempt's
	// time rather than maxAttempts × 15s. Fast transient 503s (the common case)
	// retry well within it. Applied only when the caller's ctx has no shorter
	// deadline.
	marketplaceCatalogRetryBudget = 30 * time.Second
)

// isRetryableStatus reports whether an HTTP status is a transient upstream
// failure worth retrying. Mirrors the gateway's isRetryableLLMError set.
// 4xx (404/401/422) are caller/payload errors and are NOT retried.
func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

// parseRetryAfter returns the Retry-After delay if the header carries a plain
// integer number of seconds (the common 503-under-load form). HTTP-date form
// and malformed/absent values return 0 (fall back to exponential backoff).
func parseRetryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

// retryDelay computes the sleep before retry number `retryIndex` (1-based):
// exponential base*2^(retryIndex-1) with ±20% jitter (jitter avoids a
// thundering-herd of daemons retrying a recovering server in lockstep),
// raised to honor a server Retry-After, and capped at marketplaceRetryMaxDelay.
func retryDelay(retryIndex int, base time.Duration, retryAfter time.Duration) time.Duration {
	if base <= 0 {
		base = defaultMarketplaceRetryBase
	}
	d := base
	for i := 1; i < retryIndex; i++ {
		d *= 2
		if d >= marketplaceRetryMaxDelay {
			break
		}
	}
	// ±20% jitter.
	d = time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
	if retryAfter > d {
		d = retryAfter
	}
	if d > marketplaceRetryMaxDelay {
		d = marketplaceRetryMaxDelay
	}
	return d
}

// drainClose discards a small bounded prefix of a to-be-retried response body
// (so the underlying connection can be reused) and closes it.
func drainClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	_ = resp.Body.Close()
}

// doGETWithRetry issues GET rawURL on hc, retrying transient failures (network
// errors on the idempotent GET, plus 429/5xx) with exponential backoff + jitter,
// up to maxAttempts and within an overall `budget` (0 = no budget). It returns
// the response for the caller to status-check and read — including a final
// still-failing retryable status (e.g. 503), so the caller's own "status %d"
// error wrapping (and the daemon's 404-vs-503 split) is preserved. Retried
// responses are drained+closed internally. The backoff sleep is ctx-aware, so
// retries never exceed the caller's deadline (or the budget).
//
// This is deliberately NOT built on the generic uploads.doWithRetry[T] /
// images.doWithRetry: those return an ErrTransient sentinel, whereas this must
// hand back the raw *http.Response so each caller preserves the upstream status
// code that the daemon greps to map 404 vs 503. Keep that contract if
// consolidating.
func doGETWithRetry(ctx context.Context, hc *http.Client, rawURL string, maxAttempts int, base time.Duration, budget time.Duration) (*http.Response, error) {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	// budget bounds the TOTAL wall-clock of the GET *including the caller's body
	// read*, so it must NOT be torn down with a bare `defer cancel()`: that
	// cancels the request context the instant this function returns — i.e.
	// BEFORE the caller reads resp.Body — and a large, not-yet-buffered body then
	// fails with "context canceled" (small bodies survive only by luck of
	// transport buffering). Instead cancel fires on every error return (via the
	// deferred guard below) and, on success, is transferred to the returned
	// body's Close (cancelReadCloser) so the deadline still covers the read.
	cancel := context.CancelFunc(func() {})
	if budget > 0 {
		ctx, cancel = context.WithTimeout(ctx, budget)
	}
	transferred := false
	defer func() {
		if !transferred {
			cancel()
		}
	}()
	var retryAfter time.Duration
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			// time.NewTimer (+Stop) rather than time.After so a ctx-cancel
			// during the sleep doesn't leak a pending timer until it fires.
			timer := time.NewTimer(retryDelay(attempt-1, base, retryAfter))
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			}
			retryAfter = 0
		}

		req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
		if err != nil {
			return nil, err // bad URL — not retryable
		}
		resp, err := hc.Do(req)
		if err != nil {
			lastErr = err
			// Any transport error on an idempotent GET is retryable unless the
			// caller's ctx (or the budget) is what cancelled it.
			if attempt < maxAttempts && ctx.Err() == nil {
				continue
			}
			return nil, err
		}
		if attempt < maxAttempts && isRetryableStatus(resp.StatusCode) {
			retryAfter = parseRetryAfter(resp.Header)
			drainClose(resp)
			continue
		}
		// 2xx, non-retryable status, or final attempt. Keep the budget context
		// alive across the caller's body read: transfer cancel to Body.Close so
		// the deadline still bounds the read but is not torn down on return.
		if budget > 0 {
			resp.Body = &cancelReadCloser{ReadCloser: resp.Body, cancel: cancel}
			transferred = true
		}
		return resp, nil
	}
	return nil, lastErr // unreachable: the loop always returns on the last attempt
}

// cancelReadCloser transfers ownership of a context cancel func to an
// http.Response body, so the budget context doGETWithRetry created stays live
// while the caller reads the body and is canceled only when the body is closed.
// Callers of doGETWithRetry MUST close the returned body (all do via
// `defer resp.Body.Close()`); if one ever failed to, the context would simply
// linger until the budget deadline fired — a leak bounded by the budget, not a
// correctness bug.
type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelReadCloser) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}
