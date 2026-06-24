// Package uploads is a thin HTTP client for Shannon Cloud's /api/v1/uploads
// endpoints — POST to publish a file, GET to list the current user's still-
// active uploads, DELETE to retract one. POST streams a multipart/form-data
// body via io.Pipe so 50 MiB files never sit in memory in full. All three
// methods classify HTTP responses into typed errors that callers can branch on
// and retry transient failures (5xx + network) with exponential backoff.
package uploads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Kind enumerates the business-purpose buckets Cloud accepts on POST /uploads.
// Cloud enforces these via a CHECK constraint (migration 130) — adding a new
// value here without a corresponding cloud migration causes 400 invalid_kind.
// Leaving Kind empty in UploadOptions lets Cloud default to "other" server-side.
const (
	KindSessionShare = "session_share"
	KindReport       = "report"
	KindLandingPage  = "landing_page"
	KindImage        = "image"
	KindOther        = "other"
)

// validUploadKinds is the client-side mirror of Cloud's CHECK constraint.
// Used both as upload-time pre-validation (skip a round trip on obvious typos)
// and as a list-filter whitelist. Kept in sync with the constants above.
var validUploadKinds = map[string]bool{
	KindSessionShare: true,
	KindReport:       true,
	KindLandingPage:  true,
	KindImage:        true,
	KindOther:        true,
}

// IsValidKind reports whether s is in the upload-kind whitelist. Empty string
// is NOT valid — callers that want "no preference" should pass "" to Upload
// and let Cloud default to "other", but list filters require an explicit value.
func IsValidKind(s string) bool { return validUploadKinds[s] }

// UploadOptions bundles the optional knobs for POST /api/v1/uploads. All
// fields are optional individually; the empty struct uploads as application/
// octet-stream with no business classification (Cloud defaults Kind to "other").
type UploadOptions struct {
	// Filename overrides the multipart Part filename. Empty falls back to
	// "upload" (server then sniffs by extension on the bytes).
	Filename string
	// ContentType overrides the stored MIME. Empty falls back to the server's
	// extension-based sniff, ending in application/octet-stream.
	ContentType string
	// Kind is the business-purpose bucket — see KindXxx constants. Empty is
	// allowed and means "let Cloud default to other". A non-empty value not in
	// validUploadKinds short-circuits with ErrBadRequest before any HTTP call.
	Kind string
	// Metadata is a pre-marshaled JSON object (≤ 8 KiB). Empty/nil = no
	// metadata field on the multipart envelope; Cloud stores {} in that case.
	// Must be a JSON object — arrays / scalars are rejected by Cloud with
	// invalid_metadata, but the client doesn't pre-validate shape (callers are
	// expected to marshal from a struct/map).
	Metadata json.RawMessage
}

// ListOptions bundles the query parameters for GET /api/v1/uploads. Zero values
// for Limit and Offset map to Cloud defaults (limit=20, offset=0); empty Kind
// disables the filter.
type ListOptions struct {
	Limit  int
	Offset int
	// Kind filters the response to a single business-purpose bucket. Empty
	// returns all kinds. A non-empty value not in validUploadKinds returns
	// ErrBadRequest before any HTTP call.
	Kind string
}

// UploadResponse mirrors the JSON returned by POST /api/v1/uploads on success.
// Use URL directly — its path segments are already percent-encoded server-side.
type UploadResponse struct {
	URL         string          `json:"url"`
	Key         string          `json:"key"`
	Size        int64           `json:"size"`
	ContentType string          `json:"content_type"`
	Kind        string          `json:"kind,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
}

// UploadEntry is a single record in GET /api/v1/uploads. Cloud omits
// s3_key / tenant_id / user_id by design — do not assume they exist.
type UploadEntry struct {
	ID          string          `json:"id"`
	URL         string          `json:"url"`
	Filename    string          `json:"filename"`
	ContentType string          `json:"content_type"`
	Size        int64           `json:"size"`
	Kind        string          `json:"kind,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	CreatedAt   string          `json:"created_at"` // RFC3339 UTC
}

// ListResponse mirrors the JSON returned by GET /api/v1/uploads on success.
// TotalCount is the user's active (non-deleted) file count under the current
// filters — it is not "everything they've ever published".
type ListResponse struct {
	Uploads    []UploadEntry `json:"uploads"`
	TotalCount int           `json:"total_count"`
}

// DeleteResponse mirrors the JSON returned by DELETE /api/v1/uploads/{id} on
// success. CDNEvictionSeconds is the worst-case window during which CloudFront
// edge nodes may still serve cached content — surface it to the user so
// they don't think the retract silently failed when the URL "still works"
// for a few minutes.
type DeleteResponse struct {
	Deleted            bool   `json:"deleted"`
	ID                 string `json:"id"`
	CDNEvictionSeconds int    `json:"cdn_eviction_seconds"`
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
	ErrUnauthorized = errors.New("upload: unauthorized")
	// ErrBadRequest is a 400 (missing_file, malformed_multipart). Permanent — client bug.
	ErrBadRequest = errors.New("upload: bad request")
	// ErrEndpointNotFound is a 404. Permanent — the gateway answered but does
	// not have /api/v1/uploads mounted. Usually means the deployment doesn't
	// include the uploads handler yet, or cloud.endpoint points at a wrong host.
	ErrEndpointNotFound = errors.New("upload: endpoint not deployed")
	// ErrInvalidKind is a 400 with code "invalid_kind" — the kind value is not
	// in Cloud's CHECK-constrained whitelist. Permanent client bug; same shape
	// as ErrBadRequest but split out so daemon handler and tool layer can map
	// it to a more actionable error message instead of generic "bad request".
	ErrInvalidKind = errors.New("upload: invalid kind")
	// ErrFileTooLarge is a 413. Permanent — file exceeds the 50 MiB server limit.
	ErrFileTooLarge = errors.New("upload: file too large")
	// ErrServerConfig is a 500 with code "s3_unconfigured". Permanent — server-side fix needed.
	ErrServerConfig = errors.New("upload: server misconfigured")
	// ErrNotFound is a 404 on the Delete path — the upload id does not exist,
	// has already been retracted, or belongs to another user. Cloud
	// deliberately conflates these three cases to avoid existence leaks, so
	// callers must surface a single "not found or already retracted" message
	// without trying to disambiguate.
	ErrNotFound = errors.New("upload: not found")
	// ErrTransient wraps 500 (other reasons) / 502 / 503 / 504 / network errors.
	// The client retries these internally before returning; once returned, retries
	// have already been exhausted.
	ErrTransient = errors.New("upload: transient")
)

// Client posts files to the Cloud uploads endpoint.
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
// the GatewayClient's existing *http.Client so timeouts and any future tracing
// transport are inherited rather than reinvented.
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

// Upload streams body to /api/v1/uploads as multipart/form-data and returns
// the parsed response. openBody is a factory: it MUST be cheap to call and
// MUST return a fresh, fully-rewound reader each time, because each retry
// reissues the request and consumes the body afresh.
//
// All UploadOptions fields are optional. Filename empty falls back to "upload";
// ContentType empty defers to the server's extension sniff; Kind empty lets
// Cloud default to "other"; Metadata nil/empty omits the field entirely. A
// non-empty Kind not in validUploadKinds short-circuits with ErrBadRequest
// before any HTTP call (defends against typos before they cost a round trip).
func (c *Client) Upload(
	ctx context.Context,
	openBody func() (io.ReadCloser, error),
	opts UploadOptions,
) (*UploadResponse, error) {
	if openBody == nil {
		return nil, fmt.Errorf("upload: openBody is required")
	}
	if opts.Kind != "" && !validUploadKinds[opts.Kind] {
		return nil, fmt.Errorf("%w: invalid kind %q (allowed: session_share, report, landing_page, image, other)", ErrBadRequest, opts.Kind)
	}
	return doWithRetry(ctx, c.maxAttempts, c.backoff, func(ctx context.Context) (*UploadResponse, error) {
		return c.uploadOnce(ctx, "/api/v1/uploads", openBody, opts)
	})
}

// UploadEphemeral streams body to /api/v1/uploads/ephemeral and returns the
// parsed response. Unlike Upload, the ephemeral endpoint does NOT record the
// file in the user's upload library (it never shows up in List / the Published
// Files UI and can't be retracted) — it just stores the bytes on the public CDN
// and returns an unguessable permanent URL. Use it for assets that are
// referenced by URL but aren't user-managed "published files", e.g. agent
// avatars. Only Filename and ContentType are honored; Kind/Metadata are ignored
// by this endpoint (it has no library row to classify).
func (c *Client) UploadEphemeral(
	ctx context.Context,
	openBody func() (io.ReadCloser, error),
	opts UploadOptions,
) (*UploadResponse, error) {
	if openBody == nil {
		return nil, fmt.Errorf("upload: openBody is required")
	}
	return doWithRetry(ctx, c.maxAttempts, c.backoff, func(ctx context.Context) (*UploadResponse, error) {
		return c.uploadOnce(ctx, "/api/v1/uploads/ephemeral", openBody, opts)
	})
}

// List calls GET /api/v1/uploads with the supplied options and returns the
// parsed response. Limit/Offset are passed through as-is; cloud clamps limit
// to [1, 100] internally (and 0 → default 20), but callers are encouraged to
// validate before calling so error messages stay close to the user. A
// non-empty Kind not in validUploadKinds short-circuits with ErrBadRequest.
func (c *Client) List(ctx context.Context, opts ListOptions) (*ListResponse, error) {
	if opts.Kind != "" && !validUploadKinds[opts.Kind] {
		return nil, fmt.Errorf("%w: invalid kind %q (allowed: session_share, report, landing_page, image, other)", ErrBadRequest, opts.Kind)
	}
	return doWithRetry(ctx, c.maxAttempts, c.backoff, func(ctx context.Context) (*ListResponse, error) {
		return c.listOnce(ctx, opts)
	})
}

// Delete calls DELETE /api/v1/uploads/{id} and returns the parsed response.
// id must be a UUID; the client does not pre-validate format (cloud answers
// 404 for malformed ids, same as for legitimately missing ones).
//
// Delete is idempotent on the server (a second call after a successful first
// returns 404 because deleted_at is non-NULL and the WHERE clause filters the
// row out), so 5xx retries are safe: the worst case is one extra 404 on a
// later retry, which the caller surfaces as "already retracted".
func (c *Client) Delete(ctx context.Context, id string) (*DeleteResponse, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("%w: id is required", ErrBadRequest)
	}
	return doWithRetry(ctx, c.maxAttempts, c.backoff, func(ctx context.Context) (*DeleteResponse, error) {
		return c.deleteOnce(ctx, id)
	})
}

// doWithRetry runs attempt up to maxAttempts times with backoff between
// retries. Only ErrTransient is retried; everything else short-circuits.
// Generic so each method's response type stays statically typed at the call
// site (no any-cast in callers).
func doWithRetry[T any](
	ctx context.Context,
	maxAttempts int,
	backoff func(int) time.Duration,
	attempt func(ctx context.Context) (*T, error),
) (*T, error) {
	var lastErr error
	for n := 1; n <= maxAttempts; n++ {
		if delay := backoff(n); delay > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		resp, err := attempt(ctx)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !isRetriable(err) {
			return nil, err
		}
		if n == maxAttempts {
			break
		}
	}
	return nil, lastErr
}

func isRetriable(err error) bool {
	return errors.Is(err, ErrTransient)
}

// uploadOnce performs a single multipart POST. Streaming is via io.Pipe + a
// goroutine that writes the multipart envelope; the HTTP body is the pipe
// reader so net/http drains it incrementally without buffering the file.
func (c *Client) uploadOnce(
	ctx context.Context,
	path string,
	openBody func() (io.ReadCloser, error),
	opts UploadOptions,
) (*UploadResponse, error) {
	body, err := openBody()
	if err != nil {
		return nil, fmt.Errorf("upload: open body: %w", err)
	}
	defer body.Close()

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	// Goroutine writes the multipart envelope into the pipe writer; the HTTP
	// client reads it from the pipe reader as request body.
	go func() {
		defer pw.Close()
		defer mw.Close()

		if opts.ContentType != "" {
			if werr := mw.WriteField("content_type", opts.ContentType); werr != nil {
				pw.CloseWithError(werr)
				return
			}
		}
		if opts.Kind != "" {
			if werr := mw.WriteField("kind", opts.Kind); werr != nil {
				pw.CloseWithError(werr)
				return
			}
		}
		if len(opts.Metadata) > 0 {
			if werr := mw.WriteField("metadata", string(opts.Metadata)); werr != nil {
				pw.CloseWithError(werr)
				return
			}
		}

		fname := opts.Filename
		if fname == "" {
			fname = "upload"
		}
		// Build the file part header manually so we can set Content-Type when
		// the caller specified one (CreateFormFile defaults to octet-stream).
		hdr := make(textproto.MIMEHeader)
		hdr.Set("Content-Disposition",
			fmt.Sprintf(`form-data; name="file"; filename=%q`, fname))
		if opts.ContentType != "" {
			hdr.Set("Content-Type", opts.ContentType)
		} else {
			hdr.Set("Content-Type", "application/octet-stream")
		}
		part, perr := mw.CreatePart(hdr)
		if perr != nil {
			pw.CloseWithError(perr)
			return
		}
		if _, cerr := io.Copy(part, body); cerr != nil {
			pw.CloseWithError(cerr)
			return
		}
	}()

	endpoint := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, pr)
	if err != nil {
		// Unblock the writer goroutine before returning, otherwise it sits
		// forever on the unbuffered pipe write inside io.Copy.
		_ = pr.CloseWithError(err)
		return nil, fmt.Errorf("upload: build request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Network / context-canceled errors. ctx errors propagate as-is so the
		// caller's select can distinguish cancellation from a transport hiccup.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: network: %v", ErrTransient, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var out UploadResponse
		if jerr := json.Unmarshal(respBody, &out); jerr != nil {
			return nil, fmt.Errorf("upload: parse response: %w", jerr)
		}
		if out.URL == "" {
			return nil, fmt.Errorf("upload: response missing url field")
		}
		return &out, nil
	}

	return nil, classifyError(resp.StatusCode, respBody, "upload")
}

// listOnce performs a single GET /api/v1/uploads with the given paging.
func (c *Client) listOnce(ctx context.Context, opts ListOptions) (*ListResponse, error) {
	endpoint, err := url.Parse(c.baseURL + "/api/v1/uploads")
	if err != nil {
		return nil, fmt.Errorf("upload: build request: %w", err)
	}
	q := endpoint.Query()
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Offset > 0 {
		q.Set("offset", strconv.Itoa(opts.Offset))
	}
	if opts.Kind != "" {
		q.Set("kind", opts.Kind)
	}
	endpoint.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("upload: build request: %w", err)
	}
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: network: %v", ErrTransient, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap on list payload

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var out ListResponse
		if jerr := json.Unmarshal(respBody, &out); jerr != nil {
			return nil, fmt.Errorf("upload: parse list response: %w", jerr)
		}
		if out.Uploads == nil {
			out.Uploads = []UploadEntry{}
		}
		return &out, nil
	}

	return nil, classifyError(resp.StatusCode, respBody, "list")
}

// deleteOnce performs a single DELETE /api/v1/uploads/{id}.
func (c *Client) deleteOnce(ctx context.Context, id string) (*DeleteResponse, error) {
	endpoint := c.baseURL + "/api/v1/uploads/" + url.PathEscape(id)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("upload: build request: %w", err)
	}
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: network: %v", ErrTransient, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var out DeleteResponse
		if jerr := json.Unmarshal(respBody, &out); jerr != nil {
			return nil, fmt.Errorf("upload: parse delete response: %w", jerr)
		}
		return &out, nil
	}

	return nil, classifyError(resp.StatusCode, respBody, "delete")
}

// classifyError maps non-2xx responses to the typed sentinel errors. op
// disambiguates the meaning of 404: "delete" treats 404 as "file not found /
// already retracted / cross-user" (ErrNotFound); all other ops treat 404 as
// "the endpoint isn't deployed at this gateway" (ErrEndpointNotFound). The
// response body is also consulted for the {"error": "..."} code — in
// particular, 500 + s3_unconfigured is a permanent server-config problem,
// while 500 + upload_failed (or no body) is treated as transient.
func classifyError(status int, body []byte, op string) error {
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
	case http.StatusBadRequest: // 400
		// invalid_kind is the only 400 sub-code daemon/tool layers need to
		// distinguish (so they can rewrite "kind=foo not in whitelist" into a
		// user-friendly message). invalid_metadata / metadata_too_large stay
		// under ErrBadRequest since they only fire on client bugs.
		if code == "invalid_kind" {
			return fmt.Errorf("%w (status %d, code %q)%s", ErrInvalidKind, status, code, suffix())
		}
		return fmt.Errorf("%w (status %d, code %q)%s", ErrBadRequest, status, code, suffix())
	case http.StatusNotFound: // 404
		if op == "delete" {
			// Cloud returns 404 for: file does not exist / already retracted /
			// belongs to another user / malformed UUID. Do not try to
			// disambiguate — the API deliberately conflates them.
			return fmt.Errorf("%w (status %d)%s", ErrNotFound, status, suffix())
		}
		return fmt.Errorf("%w (status %d)%s", ErrEndpointNotFound, status, suffix())
	case http.StatusRequestEntityTooLarge: // 413
		return fmt.Errorf("%w (status %d, code %q)%s", ErrFileTooLarge, status, code, suffix())
	case http.StatusInternalServerError: // 500
		if code == "s3_unconfigured" {
			return fmt.Errorf("%w (status %d, code %q)%s", ErrServerConfig, status, code, suffix())
		}
		return fmt.Errorf("%w (status %d, code %q)%s", ErrTransient, status, code, suffix())
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout: // 502/503/504
		return fmt.Errorf("%w (status %d, code %q)%s", ErrTransient, status, code, suffix())
	default:
		// Other 4xx → permanent (treat as bad request); other 5xx → transient.
		if status >= 500 {
			return fmt.Errorf("%w (status %d, code %q)%s", ErrTransient, status, code, suffix())
		}
		return fmt.Errorf("%w (status %d, code %q)%s", ErrBadRequest, status, code, suffix())
	}
}
