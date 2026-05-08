package images

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient returns a Client wired to the given handler, with retries
// enabled but per-attempt backoff zeroed so tests don't sleep for seconds.
func newTestClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	c := NewClient(srv.URL, "sk_test_key", srv.Client())
	c.backoff = func(int) time.Duration { return 0 }
	return c, srv
}

func TestGenerateHappyPath(t *testing.T) {
	var got struct {
		hadAPIKey   bool
		contentType string
		body        []byte
	}
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.hadAPIKey = r.Header.Get("X-API-Key") == "sk_test_key"
		got.contentType = r.Header.Get("Content-Type")
		got.body, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{
            "images": [
                {"url":"https://cdn/x/a.png","key":"public/x/a.png","size_bytes":1234,"content_type":"image/png"}
            ],
            "model": "gpt-image-2",
            "size": "1024x1024"
        }`))
	}))
	defer srv.Close()

	res, err := c.Generate(context.Background(), GenerateRequest{
		Prompt:  "a serene cyberpunk cat",
		Size:    "1024x1024",
		Quality: "low",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(res.Images) != 1 || res.Images[0].URL != "https://cdn/x/a.png" {
		t.Errorf("URL = %v", res.Images)
	}
	if !got.hadAPIKey {
		t.Errorf("X-API-Key header missing")
	}
	if !strings.HasPrefix(got.contentType, "application/json") {
		t.Errorf("Content-Type = %q", got.contentType)
	}
}

// TestGenerateRequestOmitsModel guards the API spec rule that "model" must
// not be sent. If someone adds a Model field to GenerateRequest, this test
// flips red — the server pin would otherwise silently drop their addition.
func TestGenerateRequestOmitsModel(t *testing.T) {
	var rawBody []byte
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"images":[{"url":"https://cdn/x.png","content_type":"image/png"}],"model":"gpt-image-2","size":"1024x1024"}`))
	}))
	defer srv.Close()

	if _, err := c.Generate(context.Background(), GenerateRequest{Prompt: "anything"}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, has := parsed["model"]; has {
		t.Fatalf("request body must not contain 'model' (got body %s)", rawBody)
	}
}

func TestGenerateMultiImage(t *testing.T) {
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{
            "images": [
                {"url":"https://cdn/a.png","content_type":"image/png","size_bytes":1},
                {"url":"https://cdn/b.png","content_type":"image/png","size_bytes":2},
                {"url":"https://cdn/c.png","content_type":"image/png","size_bytes":3}
            ],
            "model": "gpt-image-2",
            "size": "1024x1024"
        }`))
	}))
	defer srv.Close()

	res, err := c.Generate(context.Background(), GenerateRequest{Prompt: "x", N: 3})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(res.Images) != 3 {
		t.Fatalf("expected 3 images, got %d", len(res.Images))
	}
}

func TestGenerateUnauthorizedNoRetry(t *testing.T) {
	var calls int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"unauthorized","message":"bad key"}`))
	}))
	defer srv.Close()

	_, err := c.Generate(context.Background(), GenerateRequest{Prompt: "x"})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected 1 attempt, got %d", got)
	}
}

func TestGenerateBadRequestNoRetry(t *testing.T) {
	var calls int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"prompt_too_long","message":"prompt exceeds 32000 chars"}`))
	}))
	defer srv.Close()

	_, err := c.Generate(context.Background(), GenerateRequest{Prompt: "x"})
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("expected ErrBadRequest, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected 1 attempt, got %d", got)
	}
}

func TestGenerateRequestTooLargeNoRetry(t *testing.T) {
	var calls int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(413)
		_, _ = w.Write([]byte(`{"error":"image_too_large","message":"output exceeds 20 MiB"}`))
	}))
	defer srv.Close()

	_, err := c.Generate(context.Background(), GenerateRequest{Prompt: "x"})
	if !errors.Is(err, ErrRequestTooLarge) {
		t.Fatalf("expected ErrRequestTooLarge, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected 1 attempt, got %d", got)
	}
}

func TestGenerateEndpointNotFoundNoRetry(t *testing.T) {
	var calls int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	_, err := c.Generate(context.Background(), GenerateRequest{Prompt: "x"})
	if !errors.Is(err, ErrEndpointNotFound) {
		t.Fatalf("expected ErrEndpointNotFound, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected 1 attempt, got %d", got)
	}
}

func TestGenerateUpstreamTimeoutNoRetry(t *testing.T) {
	var calls int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(504)
		_, _ = w.Write([]byte(`{"error":"upstream_timeout","message":"openai exceeded 500s"}`))
	}))
	defer srv.Close()

	_, err := c.Generate(context.Background(), GenerateRequest{Prompt: "x", Quality: "high"})
	if !errors.Is(err, ErrUpstreamTimeout) {
		t.Fatalf("expected ErrUpstreamTimeout, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected 1 attempt (no retry on 504 — re-running same args wastes the budget), got %d", got)
	}
}

func TestGenerateContentRejectedNoRetry(t *testing.T) {
	var calls int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(502)
		_, _ = w.Write([]byte(`{"error":"no_images_returned","message":"content moderation"}`))
	}))
	defer srv.Close()

	_, err := c.Generate(context.Background(), GenerateRequest{Prompt: "x"})
	if !errors.Is(err, ErrContentRejected) {
		t.Fatalf("expected ErrContentRejected, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected 1 attempt (retrying same prompt re-hits moderation), got %d", got)
	}
}

func TestGenerateUpstream502Retried(t *testing.T) {
	var calls int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(502)
			_, _ = w.Write([]byte(`{"error":"upstream_error","message":"openai 503"}`))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"images":[{"url":"https://cdn/ok.png","content_type":"image/png"}],"model":"gpt-image-2","size":"1024x1024"}`))
	}))
	defer srv.Close()

	res, err := c.Generate(context.Background(), GenerateRequest{Prompt: "x"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Images[0].URL != "https://cdn/ok.png" {
		t.Errorf("URL = %q", res.Images[0].URL)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("expected 2 server hits, got %d", got)
	}
}

func TestGenerateImageFailedRetried(t *testing.T) {
	// 500 image_failed = S3 write failure, retriable per API spec.
	var calls int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":"image_failed","message":"s3 timeout"}`))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"images":[{"url":"https://cdn/late.png","content_type":"image/png"}],"model":"gpt-image-2","size":"1024x1024"}`))
	}))
	defer srv.Close()

	res, err := c.Generate(context.Background(), GenerateRequest{Prompt: "x"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Images[0].URL != "https://cdn/late.png" {
		t.Errorf("URL = %q", res.Images[0].URL)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("expected 3 attempts, got %d", got)
	}
}

func TestGenerateServerMisconfiguredNoRetry(t *testing.T) {
	var calls int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"server_misconfigured","message":"missing storage bucket"}`))
	}))
	defer srv.Close()

	_, err := c.Generate(context.Background(), GenerateRequest{Prompt: "x"})
	if !errors.Is(err, ErrServerConfig) {
		t.Fatalf("expected ErrServerConfig, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected 1 attempt, got %d", got)
	}
}

func TestGenerateTransientExhausted(t *testing.T) {
	var calls int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(503)
	}))
	defer srv.Close()

	_, err := c.Generate(context.Background(), GenerateRequest{Prompt: "x"})
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("expected ErrTransient, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("expected 3 attempts, got %d", got)
	}
}

func TestGenerateNetworkErrorRetried(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	lis.Close()

	c := NewClient("http://"+addr, "sk_test", &http.Client{Timeout: 2 * time.Second})
	c.backoff = func(int) time.Duration { return 0 }

	_, err = c.Generate(context.Background(), GenerateRequest{Prompt: "x"})
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("expected ErrTransient, got %v", err)
	}
}

// TestGenerateContextCanceledDuringBackoff covers the path where ctx is
// canceled while doWithRetry is sleeping between attempts. The select inside
// doWithRetry must observe ctx.Done() and return ctx.Err() — without this
// branch a canceled ctx would silently survive the backoff and proceed to
// another attempt.
func TestGenerateContextCanceledDuringBackoff(t *testing.T) {
	var calls int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(503) // transient → schedules a retry with non-zero backoff
	}))
	defer srv.Close()

	// Override backoff so attempt 2 sleeps long enough to be interrupted.
	c.backoff = func(attempt int) time.Duration {
		if attempt == 1 {
			return 0
		}
		return 200 * time.Millisecond
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Wait for the first attempt to land at the server (advancing into
		// the backoff sleep), then cancel.
		for atomic.LoadInt32(&calls) < 1 {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond) // ensure we're inside the sleep
		cancel()
	}()

	_, err := c.Generate(ctx, GenerateRequest{Prompt: "x"})
	if err == nil {
		t.Fatal("expected ctx-canceled error from doWithRetry sleep")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected exactly 1 server hit (cancel during backoff prevents attempt 2), got %d", got)
	}
}

func TestGenerateContextCanceled(t *testing.T) {
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err := c.Generate(ctx, GenerateRequest{Prompt: "x"})
	if err == nil {
		t.Fatal("expected error from canceled ctx")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestGenerateResponseMissingImages(t *testing.T) {
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"images":[],"model":"gpt-image-2","size":"1024x1024"}`))
	}))
	defer srv.Close()

	_, err := c.Generate(context.Background(), GenerateRequest{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "missing images") {
		t.Fatalf("expected missing images error, got %v", err)
	}
}

func TestGenerateResponseMissingURL(t *testing.T) {
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"images":[{"key":"k","size_bytes":1}],"model":"gpt-image-2","size":"1024x1024"}`))
	}))
	defer srv.Close()

	_, err := c.Generate(context.Background(), GenerateRequest{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "missing url") {
		t.Fatalf("expected missing url error, got %v", err)
	}
}

// --- Edit endpoint tests ---------------------------------------------------

const cdnSrc = "https://static.kocoro.ai/public/abc/src.png"

func TestEditHappyPath(t *testing.T) {
	var got struct {
		path        string
		hadAPIKey   bool
		contentType string
		body        []byte
	}
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.hadAPIKey = r.Header.Get("X-API-Key") == "sk_test_key"
		got.contentType = r.Header.Get("Content-Type")
		got.body, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{
            "images": [
                {"url":"https://cdn/x/edited.png","key":"public/x/edited.png","size_bytes":2048,"content_type":"image/png"}
            ],
            "model": "gpt-image-2",
            "size": "1024x1024"
        }`))
	}))
	defer srv.Close()

	res, err := c.Edit(context.Background(), EditRequest{
		Prompt:    "add a hat",
		ImageURLs: []string{cdnSrc},
		Quality:   "low",
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if got.path != "/api/v1/images/edits" {
		t.Errorf("endpoint path = %q, want /api/v1/images/edits", got.path)
	}
	if len(res.Images) != 1 || res.Images[0].URL != "https://cdn/x/edited.png" {
		t.Errorf("URL = %v", res.Images)
	}
	if !got.hadAPIKey {
		t.Errorf("X-API-Key header missing")
	}
	if !strings.HasPrefix(got.contentType, "application/json") {
		t.Errorf("Content-Type = %q", got.contentType)
	}
	// Sanity-check that image_urls made it into the request body.
	if !strings.Contains(string(got.body), `"image_urls"`) {
		t.Errorf("request body missing image_urls field: %s", got.body)
	}
}

// TestEditRequestOmitsModel mirrors TestGenerateRequestOmitsModel — guards
// against a regression where someone adds a Model field to EditRequest. The
// server pin would silently drop it.
func TestEditRequestOmitsModel(t *testing.T) {
	var rawBody []byte
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"images":[{"url":"https://cdn/e.png","content_type":"image/png"}],"model":"gpt-image-2","size":"1024x1024"}`))
	}))
	defer srv.Close()

	if _, err := c.Edit(context.Background(), EditRequest{Prompt: "x", ImageURLs: []string{cdnSrc}}); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, has := parsed["model"]; has {
		t.Fatalf("request body must not contain 'model' (got body %s)", rawBody)
	}
}

func TestEditInvalidImageURLNoRetry(t *testing.T) {
	var calls int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"invalid_image_url","message":"https://example.com/x.png is not under static.kocoro.ai"}`))
	}))
	defer srv.Close()

	_, err := c.Edit(context.Background(), EditRequest{Prompt: "x", ImageURLs: []string{"https://example.com/x.png"}})
	if !errors.Is(err, ErrInvalidImageURL) {
		t.Fatalf("expected ErrInvalidImageURL, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected 1 attempt (re-running same args wastes a round-trip), got %d", got)
	}
}

func TestEditSourceTooLargeNoRetry(t *testing.T) {
	var calls int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(413)
		_, _ = w.Write([]byte(`{"error":"source_too_large","message":"source image exceeds 25 MiB"}`))
	}))
	defer srv.Close()

	_, err := c.Edit(context.Background(), EditRequest{Prompt: "x", ImageURLs: []string{cdnSrc}})
	if !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("expected ErrSourceTooLarge, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected 1 attempt, got %d", got)
	}
}

// TestEditSourceFetchFailedRetried verifies that 502 source_fetch_failed —
// which doesn't match any explicit sub-code branch — falls through to the
// 502 default and is retried as ErrTransient.
func TestEditSourceFetchFailedRetried(t *testing.T) {
	var calls int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(502)
			_, _ = w.Write([]byte(`{"error":"source_fetch_failed","message":"cdn 503"}`))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"images":[{"url":"https://cdn/ok.png","content_type":"image/png"}],"model":"gpt-image-2","size":"1024x1024"}`))
	}))
	defer srv.Close()

	res, err := c.Edit(context.Background(), EditRequest{Prompt: "x", ImageURLs: []string{cdnSrc}})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Images[0].URL != "https://cdn/ok.png" {
		t.Errorf("URL = %q", res.Images[0].URL)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("expected 2 server hits, got %d", got)
	}
}

// TestEditRequestTooLargeStillBadRequest ensures the 413 default branch
// (request_too_large / image_too_large) still maps to ErrRequestTooLarge —
// only source_too_large is rerouted to its own sentinel.
func TestEditRequestTooLargeStillRequestTooLarge(t *testing.T) {
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(413)
		_, _ = w.Write([]byte(`{"error":"request_too_large","message":"body exceeds 256 KiB"}`))
	}))
	defer srv.Close()

	_, err := c.Edit(context.Background(), EditRequest{Prompt: "x", ImageURLs: []string{cdnSrc}})
	if !errors.Is(err, ErrRequestTooLarge) {
		t.Fatalf("expected ErrRequestTooLarge, got %v", err)
	}
	if errors.Is(err, ErrSourceTooLarge) {
		t.Errorf("must NOT classify request_too_large as ErrSourceTooLarge")
	}
}

// TestEditBadRequestStillBadRequest mirrors TestEditRequestTooLargeStillRequestTooLarge
// for the 400 case: missing_image_urls / too_many_sources / etc still map to
// ErrBadRequest; only invalid_image_url is rerouted.
func TestEditBadRequestStillBadRequest(t *testing.T) {
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"too_many_sources","message":"image_urls must be 1..4"}`))
	}))
	defer srv.Close()

	_, err := c.Edit(context.Background(), EditRequest{Prompt: "x", ImageURLs: []string{cdnSrc}})
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("expected ErrBadRequest, got %v", err)
	}
	if errors.Is(err, ErrInvalidImageURL) {
		t.Errorf("must NOT classify too_many_sources as ErrInvalidImageURL")
	}
}

func TestDefaultBackoff(t *testing.T) {
	got := []time.Duration{
		defaultBackoff(1), defaultBackoff(2), defaultBackoff(3), defaultBackoff(4),
	}
	want := []time.Duration{0, time.Second, 2 * time.Second, 4 * time.Second}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("backoff(%d) = %v, want %v", i+1, got[i], want[i])
		}
	}
}
