// Package uploads is a thin HTTP client for Shannon Cloud's POST /api/v1/uploads
// endpoint. It streams a multipart/form-data body via io.Pipe so 50 MiB files
// never sit in memory in full, classifies HTTP responses into typed errors that
// callers can branch on, and retries transient failures (5xx + network) with
// exponential backoff.
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
	"strings"
	"time"
)

// UploadResponse mirrors the JSON returned by /api/v1/uploads on success.
// Use URL directly — its path segments are already percent-encoded server-side.
type UploadResponse struct {
	URL         string `json:"url"`
	Key         string `json:"key"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
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
	// ErrFileTooLarge is a 413. Permanent — file exceeds the 50 MiB server limit.
	ErrFileTooLarge = errors.New("upload: file too large")
	// ErrServerConfig is a 500 with code "s3_unconfigured". Permanent — server-side fix needed.
	ErrServerConfig = errors.New("upload: server misconfigured")
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
// filename and contentType may be empty. When filename is empty it falls back
// to "upload" (server further falls back to extension sniff on the bytes).
// When contentType is empty the server sniffs by extension or returns
// application/octet-stream.
func (c *Client) Upload(
	ctx context.Context,
	filename, contentType string,
	openBody func() (io.ReadCloser, error),
) (*UploadResponse, error) {
	if openBody == nil {
		return nil, fmt.Errorf("upload: openBody is required")
	}

	var lastErr error
	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		if delay := c.backoff(attempt); delay > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		resp, err := c.attempt(ctx, filename, contentType, openBody)
		if err == nil {
			return resp, nil
		}

		lastErr = err
		// Permanent errors short-circuit immediately.
		if !isRetriable(err) {
			return nil, err
		}
		// Don't sleep again past the last attempt.
		if attempt == c.maxAttempts {
			break
		}
	}
	return nil, lastErr
}

func isRetriable(err error) bool {
	return errors.Is(err, ErrTransient)
}

// attempt performs a single multipart POST. Streaming is via io.Pipe + a
// goroutine that writes the multipart envelope; the HTTP body is the pipe
// reader so net/http drains it incrementally without buffering the file.
func (c *Client) attempt(
	ctx context.Context,
	filename, contentType string,
	openBody func() (io.ReadCloser, error),
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

		if contentType != "" {
			if werr := mw.WriteField("content_type", contentType); werr != nil {
				pw.CloseWithError(werr)
				return
			}
		}

		fname := filename
		if fname == "" {
			fname = "upload"
		}
		// Build the file part header manually so we can set Content-Type when
		// the caller specified one (CreateFormFile defaults to octet-stream).
		hdr := make(textproto.MIMEHeader)
		hdr.Set("Content-Disposition",
			fmt.Sprintf(`form-data; name="file"; filename=%q`, fname))
		if contentType != "" {
			hdr.Set("Content-Type", contentType)
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

	endpoint := c.baseURL + "/api/v1/uploads"
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

	return nil, classifyError(resp.StatusCode, respBody)
}

// classifyError maps non-2xx responses to the typed sentinel errors. The
// response body is consulted for the {"error": "..."} code — in particular,
// 500 + s3_unconfigured is a permanent server-config problem, while 500 +
// upload_failed (or no body) is treated as transient.
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
	case http.StatusBadRequest: // 400
		return fmt.Errorf("%w (status %d, code %q)%s", ErrBadRequest, status, code, suffix())
	case http.StatusNotFound: // 404 — endpoint not deployed at this gateway
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
