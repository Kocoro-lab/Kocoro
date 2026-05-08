// Package images is a thin HTTP client for Shannon Cloud's image endpoints
// (POST /api/v1/images/generations and POST /api/v1/images/edits). It posts
// a small JSON request, classifies HTTP responses into typed sentinel errors,
// and retries transient failures (502 upstream_error / 502 decode_failed /
// 500 image_failed / 503 / network) with exponential backoff. Permanent
// failures and "fix-the-prompt" failures (504 upstream_timeout, 502
// no_images_returned) short-circuit so callers don't burn paid generations
// on hopeless retries.
package images

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// GenerateRequest mirrors the JSON body accepted by /api/v1/images/generations.
//
// Field semantics:
//   - Prompt:     1..32000 chars, required.
//   - Size:       "1024x1024" / "1024x1536" / "1536x1024" / "auto"; empty = server default.
//   - Quality:    "auto" / "low" / "medium" / "high"; empty = server default.
//   - N:          1..10; 0 = use server default (omitted from request via `omitempty`).
//   - Background: "transparent" / "opaque" / "auto"; empty = unset.
//
// IMPORTANT: do NOT add a Model field. The server pins gpt-image-2 and
// silently drops any "model" key in the request body. Adding it here would
// mislead future readers into thinking it does something.
type GenerateRequest struct {
	Prompt     string `json:"prompt"`
	Size       string `json:"size,omitempty"`
	Quality    string `json:"quality,omitempty"`
	N          int    `json:"n,omitempty"`
	Background string `json:"background,omitempty"`
}

// EditRequest mirrors the JSON body accepted by /api/v1/images/edits.
//
// Field semantics:
//   - Prompt:     1..32000 chars, required. Describes the modification in
//     natural language; gpt-image-2 locates the region itself (no mask field).
//   - ImageURLs:  1..4 source URLs, required. Each entry must start with
//     https://static.kocoro.ai/. The HTTP client does NOT enforce the prefix —
//     callers should validate first to avoid wasted round-trips. The server
//     returns 400 invalid_image_url for non-CDN URLs.
//   - Size:       "1024x1024" / "1024x1536" / "1536x1024" / "auto"; empty = server default.
//   - Quality:    "auto" / "low" / "medium" / "high"; empty = server default.
//   - N:          1..10; 0 = use server default (omitted from request via `omitempty`).
//   - Background: "transparent" / "opaque" / "auto"; empty = unset.
//
// IMPORTANT: do NOT add a Model field — the server pins gpt-image-2 and
// silently drops any "model" key (same as /generations).
type EditRequest struct {
	Prompt     string   `json:"prompt"`
	ImageURLs  []string `json:"image_urls"`
	Size       string   `json:"size,omitempty"`
	Quality    string   `json:"quality,omitempty"`
	N          int      `json:"n,omitempty"`
	Background string   `json:"background,omitempty"`
}

// Image is a single generated image entry. URL is a permanent public CDN URL —
// no auth, no expiry — safe to embed in markdown / Slack / Feishu replies.
type Image struct {
	URL         string `json:"url"`
	Key         string `json:"key"`
	SizeBytes   int64  `json:"size_bytes"`
	ContentType string `json:"content_type"`
}

// GenerateResponse mirrors the JSON returned on success. Usage is left as raw
// JSON because callers only forward it (e.g. for audit / billing) and the
// shape may extend over time without the client needing to recompile.
type GenerateResponse struct {
	Images []Image          `json:"images"`
	Model  string           `json:"model"`
	Size   string           `json:"size"`
	Usage  *json.RawMessage `json:"usage,omitempty"`
}

// errorBody is the on-the-wire shape of {"error": "...", "message": "..."}.
type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// Sentinel errors. Callers wrap with errors.Is to decide retry policy and how
// to surface the failure to the user.
var (
	// ErrUnauthorized is a 401. Permanent — fix the API key.
	ErrUnauthorized = errors.New("image: unauthorized")
	// ErrBadRequest is a 400 (invalid_json / missing_prompt / prompt_too_long /
	// n_too_large). Permanent — client bug or out-of-range arg.
	ErrBadRequest = errors.New("image: bad request")
	// ErrRequestTooLarge is a 413 (request_too_large or image_too_large).
	// Permanent — server-side hard cap.
	ErrRequestTooLarge = errors.New("image: request too large")
	// ErrEndpointNotFound is a 404. Permanent — the gateway answered but does
	// not have the requested images endpoint (/api/v1/images/generations or
	// /api/v1/images/edits) mounted. Means cloud.endpoint points at a
	// deployment that doesn't include the images handler.
	ErrEndpointNotFound = errors.New("image: endpoint not deployed")
	// ErrUpstreamTimeout is a 504 upstream_timeout. Permanent for the same
	// args — retrying with the same prompt+quality will time out again.
	// Caller should advise the model to drop quality or n.
	ErrUpstreamTimeout = errors.New("image: upstream timeout")
	// ErrContentRejected is a 502 no_images_returned (content moderation hit
	// or upstream empty response). Permanent for this prompt — retrying with
	// the same text will hit the same moderation outcome.
	ErrContentRejected = errors.New("image: no images returned")
	// ErrServerConfig is a 500 with a server-misconfigured signal. Permanent —
	// requires server-side fix (e.g. missing storage bucket).
	ErrServerConfig = errors.New("image: server misconfigured")
	// ErrTransient wraps the retriable failures: 502 upstream_error,
	// 502 decode_failed, 502 source_fetch_failed (edits only), 500 image_failed,
	// 503, and network errors. The client retries these internally before
	// returning; once returned, retries have already been exhausted.
	ErrTransient = errors.New("image: transient")
	// ErrInvalidImageURL is a 400 invalid_image_url (edits only). Permanent —
	// at least one entry in image_urls is not under https://static.kocoro.ai/.
	// Callers should obtain a CDN URL via generate_image or publish_to_web
	// before retrying; same args will hit the same outcome.
	ErrInvalidImageURL = errors.New("image: invalid source URL")
	// ErrSourceTooLarge is a 413 source_too_large (edits only). Permanent —
	// one of the source images exceeds OpenAI's 25 MiB hard cap. Re-publish
	// a smaller / compressed version before retrying.
	ErrSourceTooLarge = errors.New("image: source too large")
)

// Client posts image-generation requests to the Cloud endpoint.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	// retry / backoff knobs (overridable in tests)
	maxAttempts int
	backoff     func(attempt int) time.Duration
}

// NewClient builds a Client. baseURL should be the gateway base (no trailing
// slash, e.g. "https://api-dev.shannon.run"). httpClient is required — pass
// the GatewayClient's existing *http.Client so the 600s timeout (which meets
// the API spec's >=600s requirement) and any future tracing transport are
// inherited rather than reinvented.
func NewClient(baseURL, apiKey string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 600 * time.Second}
	}
	return &Client{
		baseURL:     strings.TrimRight(baseURL, "/"),
		apiKey:      apiKey,
		httpClient:  httpClient,
		maxAttempts: 3,
		backoff:     defaultBackoff,
	}
}

// defaultBackoff: 1s, 2s, 4s before attempts 2, 3, 4. Attempt 1 has no delay.
func defaultBackoff(attempt int) time.Duration {
	if attempt <= 1 {
		return 0
	}
	d := time.Second
	for i := 1; i < attempt-1; i++ {
		d *= 2
	}
	return d
}

// Generate posts a single request to /api/v1/images/generations, retrying on
// transient failures. Permanent / business errors (validation, content-rejection,
// upstream-timeout) short-circuit on the first attempt to avoid wasting paid
// generations.
func (c *Client) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	return c.doWithRetry(ctx, func() (*GenerateResponse, error) {
		return c.attempt(ctx, "/api/v1/images/generations", req)
	})
}

// Edit posts a single request to /api/v1/images/edits with the same retry /
// classification semantics as Generate. The success-response schema is
// identical to /generations, so we reuse GenerateResponse.
//
// IMPORTANT: callers should pre-validate that every ImageURLs entry starts
// with https://static.kocoro.ai/. The HTTP client passes them through; the
// server will reject non-CDN URLs with 400 invalid_image_url
// (ErrInvalidImageURL), which is permanent — retrying wastes a round trip.
func (c *Client) Edit(ctx context.Context, req EditRequest) (*GenerateResponse, error) {
	return c.doWithRetry(ctx, func() (*GenerateResponse, error) {
		return c.attempt(ctx, "/api/v1/images/edits", req)
	})
}

// doWithRetry is shared by Generate / Edit. The closure returns nil error on
// success, a typed sentinel on permanent failure (no retry), or anything
// wrapping ErrTransient on retriable failure. Backoff is consulted before
// every attempt so attempt 1 has zero delay (defaultBackoff returns 0).
func (c *Client) doWithRetry(ctx context.Context, do func() (*GenerateResponse, error)) (*GenerateResponse, error) {
	var lastErr error
	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		if delay := c.backoff(attempt); delay > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		resp, err := do()
		if err == nil {
			return resp, nil
		}

		lastErr = err
		if !isRetriable(err) {
			return nil, err
		}
		if attempt == c.maxAttempts {
			break
		}
	}
	return nil, lastErr
}

func isRetriable(err error) bool {
	return errors.Is(err, ErrTransient)
}

// attempt marshals req, posts to baseURL+endpoint, and parses or classifies
// the response. It re-marshals on every retry rather than caching the body
// because retries are rare and the marshal is cheap relative to the round
// trip.
func (c *Client) attempt(ctx context.Context, endpoint string, req any) (*GenerateResponse, error) {
	bodyBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("image: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("image: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("X-API-Key", c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: network: %v", ErrTransient, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var out GenerateResponse
		if jerr := json.Unmarshal(respBody, &out); jerr != nil {
			return nil, fmt.Errorf("image: parse response: %w", jerr)
		}
		if len(out.Images) == 0 {
			return nil, fmt.Errorf("image: response missing images field")
		}
		for i := range out.Images {
			if out.Images[i].URL == "" {
				return nil, fmt.Errorf("image: response image[%d] missing url", i)
			}
		}
		return &out, nil
	}

	return nil, classifyError(resp.StatusCode, respBody)
}

// classifyError maps non-2xx responses to the typed sentinel errors. The
// response body's "error" code is consulted to disambiguate same-status
// outcomes:
//   - 400: invalid_image_url (edits) → ErrInvalidImageURL; everything else → ErrBadRequest
//   - 413: source_too_large (edits) → ErrSourceTooLarge; everything else → ErrRequestTooLarge
//   - 502: no_images_returned → ErrContentRejected; upstream_error / decode_failed /
//     source_fetch_failed (edits) / unknown → ErrTransient (retriable)
//   - 500: image_failed → ErrTransient; server_misconfigured / s3_unconfigured → ErrServerConfig
func classifyError(status int, body []byte) error {
	var parsed errorBody
	_ = json.Unmarshal(body, &parsed)
	code := parsed.Error

	suffix := func() string {
		if parsed.Message != "" {
			return ": " + parsed.Message
		}
		if len(body) > 0 && code == "" {
			s := strings.TrimSpace(string(body))
			if s != "" {
				return ": " + s
			}
		}
		return ""
	}

	switch status {
	case http.StatusUnauthorized: // 401
		return fmt.Errorf("%w (status %d, code %q)%s", ErrUnauthorized, status, code, suffix())
	case http.StatusBadRequest: // 400 — disambiguate edits-only sub-codes
		switch code {
		case "invalid_image_url":
			// edits only — image_urls entry not under static.kocoro.ai
			return fmt.Errorf("%w (status %d, code %q)%s", ErrInvalidImageURL, status, code, suffix())
		default:
			// invalid_json / missing_prompt / prompt_too_long / n_too_large /
			// missing_image_urls / too_many_sources → ErrBadRequest
			return fmt.Errorf("%w (status %d, code %q)%s", ErrBadRequest, status, code, suffix())
		}
	case http.StatusNotFound: // 404 — endpoint not deployed at this gateway
		return fmt.Errorf("%w (status %d)%s", ErrEndpointNotFound, status, suffix())
	case http.StatusRequestEntityTooLarge: // 413 — disambiguate edits-only sub-code
		switch code {
		case "source_too_large":
			// edits only — single source image > 25 MiB OpenAI cap
			return fmt.Errorf("%w (status %d, code %q)%s", ErrSourceTooLarge, status, code, suffix())
		default:
			// request_too_large (body > 256 KiB) / image_too_large (output > 20 MiB)
			return fmt.Errorf("%w (status %d, code %q)%s", ErrRequestTooLarge, status, code, suffix())
		}
	case http.StatusGatewayTimeout: // 504 upstream_timeout — fix-the-args, do not retry
		return fmt.Errorf("%w (status %d, code %q)%s", ErrUpstreamTimeout, status, code, suffix())
	case http.StatusBadGateway: // 502 — disambiguate by sub-code
		switch code {
		case "no_images_returned":
			return fmt.Errorf("%w (status %d, code %q)%s", ErrContentRejected, status, code, suffix())
		default:
			// upstream_error (transient OpenAI hiccup), decode_failed (base64
			// corruption), or unknown 502 → all retriable.
			return fmt.Errorf("%w (status %d, code %q)%s", ErrTransient, status, code, suffix())
		}
	case http.StatusServiceUnavailable: // 503 — transient
		return fmt.Errorf("%w (status %d, code %q)%s", ErrTransient, status, code, suffix())
	case http.StatusInternalServerError: // 500 — disambiguate by sub-code
		switch code {
		case "image_failed":
			// S3 write failure per API spec — retriable.
			return fmt.Errorf("%w (status %d, code %q)%s", ErrTransient, status, code, suffix())
		case "server_misconfigured", "s3_unconfigured":
			return fmt.Errorf("%w (status %d, code %q)%s", ErrServerConfig, status, code, suffix())
		default:
			// Unknown 500 → treat as transient (consistent with uploads client).
			return fmt.Errorf("%w (status %d, code %q)%s", ErrTransient, status, code, suffix())
		}
	default:
		// Other 4xx → permanent (treat as bad request); other 5xx → transient.
		if status >= 500 {
			return fmt.Errorf("%w (status %d, code %q)%s", ErrTransient, status, code, suffix())
		}
		return fmt.Errorf("%w (status %d, code %q)%s", ErrBadRequest, status, code, suffix())
	}
}
