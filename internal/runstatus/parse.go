package runstatus

import (
	"encoding/json"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// Detail carries structured metadata extracted from gateway error
// responses. Used by formatFriendly() to substitute concrete values
// (reset_at, window, auto_refill_started) into templated messages.
// Zero values are common and safe — every field is best-effort.
type Detail struct {
	Window            string    // "monthly" / "daily" / "minute" / "hour"
	ResetAt           time.Time // RFC3339 in body; zero when absent
	AutoRefillStarted bool      // credits_exhausted with refill in progress
}

// classifyAPIError maps a gateway *client.APIError to (code, detail).
// Only 429s carry structured detail today (parsed by parse429).
func classifyAPIError(e *client.APIError) (Code, *Detail) {
	switch e.StatusCode {
	case 429:
		return parse429(e.Body)
	case 529:
		return CodeProviderOverloaded, nil
	case 500, 502, 503:
		return CodeServiceTemporaryError, nil
	}
	return CodeUnexpected, nil
}

// parse429 disambiguates the four 429 response shapes the gateway emits
// (per shannon-cloud middleware/quota.go, ratelimit.go, openai/handler.go):
//
//	A. Token quota exceeded: {error: "Token quota exceeded", window, reset_at}
//	B. Credits exhausted:    {error: "credits_exhausted", auto_refill_started, ...}
//	C. Rate throttle:        {error: "Rate limit exceeded", window}
//	D. Upstream Anthropic:   {error: {type: "rate_limit_error", ...}}  (object, not string)
//
// Cases A/B/C have `error` as a string; D wraps it in an object. We
// route by the JSON shape of the `error` field.
//
// Unparseable / unknown bodies fall back to CodeRateLimited (the
// pre-this-PR behavior, equivalent to "transient retry").
func parse429(body string) (Code, *Detail) {
	var raw struct {
		Error             json.RawMessage `json:"error"`
		Window            string          `json:"window"`
		ResetAt           string          `json:"reset_at"`
		AutoRefillStarted bool            `json:"auto_refill_started"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return CodeRateLimited, nil
	}

	// Case D: error is a wrapped object. Upstream provider throttle —
	// transient, retry semantics same as case C.
	var asObject struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw.Error, &asObject); err == nil && asObject.Type != "" {
		return CodeRateLimited, nil
	}

	// Cases A/B/C: error is a JSON string.
	var errStr string
	if err := json.Unmarshal(raw.Error, &errStr); err != nil {
		return CodeRateLimited, nil
	}

	switch errStr {
	case "Token quota exceeded":
		d := &Detail{Window: raw.Window}
		if t, err := time.Parse(time.RFC3339, raw.ResetAt); err == nil {
			d.ResetAt = t.UTC()
		}
		return CodeQuotaExceeded, d
	case "credits_exhausted":
		return CodeCreditsExhausted, &Detail{AutoRefillStarted: raw.AutoRefillStarted}
	case "Rate limit exceeded":
		return CodeRateLimited, nil
	}
	return CodeRateLimited, nil
}
