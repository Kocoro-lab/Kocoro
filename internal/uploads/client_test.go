package uploads

import (
	"context"
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

// fileFactory returns an openBody factory over a fixed []byte payload, plus a
// counter so tests can verify the factory was called once per attempt.
func fileFactory(payload []byte) (func() (io.ReadCloser, error), *int32) {
	var calls int32
	return func() (io.ReadCloser, error) {
		atomic.AddInt32(&calls, 1)
		return io.NopCloser(strings.NewReader(string(payload))), nil
	}, &calls
}

func TestUploadHappyPath(t *testing.T) {
	var got struct {
		hadAPIKey bool
		filename  string
		body      []byte
	}
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.hadAPIKey = r.Header.Get("X-API-Key") == "sk_test_key"
		// Parse the multipart body to confirm filename and payload.
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			t.Fatalf("ParseMultipartForm: %v", err)
		}
		fhs := r.MultipartForm.File["file"]
		if len(fhs) != 1 {
			t.Fatalf("expected 1 file part, got %d", len(fhs))
		}
		got.filename = fhs[0].Filename
		f, _ := fhs[0].Open()
		defer f.Close()
		got.body, _ = io.ReadAll(f)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"url":"https://x/y/landing.html","key":"k","size":12,"content_type":"text/html"}`))
	}))
	defer srv.Close()

	open, _ := fileFactory([]byte("<h1>hi</h1>\n"))
	res, err := c.Upload(context.Background(), "landing.html", "text/html", open)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if res.URL != "https://x/y/landing.html" {
		t.Errorf("URL = %q", res.URL)
	}
	if !got.hadAPIKey {
		t.Errorf("X-API-Key header missing")
	}
	if got.filename != "landing.html" {
		t.Errorf("filename = %q", got.filename)
	}
	if string(got.body) != "<h1>hi</h1>\n" {
		t.Errorf("body = %q", got.body)
	}
}

func TestUploadUnauthorizedNoRetry(t *testing.T) {
	var calls int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"unauthorized","message":"bad key"}`))
	}))
	defer srv.Close()

	open, _ := fileFactory([]byte("hi"))
	_, err := c.Upload(context.Background(), "x.txt", "text/plain", open)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected 1 attempt, got %d", got)
	}
}

func TestUploadFileTooLargeNoRetry(t *testing.T) {
	var calls int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(413)
		_, _ = w.Write([]byte(`{"error":"file_too_large"}`))
	}))
	defer srv.Close()

	open, _ := fileFactory([]byte("hi"))
	_, err := c.Upload(context.Background(), "x.txt", "text/plain", open)
	if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("expected ErrFileTooLarge, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected 1 attempt, got %d", got)
	}
}

func TestUploadEndpointNotFoundNoRetry(t *testing.T) {
	var calls int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		// Mimic Go's default 404 handler — no JSON body, plain text.
		http.NotFound(w, r)
	}))
	defer srv.Close()

	open, _ := fileFactory([]byte("hi"))
	_, err := c.Upload(context.Background(), "x.txt", "text/plain", open)
	if !errors.Is(err, ErrEndpointNotFound) {
		t.Fatalf("expected ErrEndpointNotFound, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected 1 attempt (no retry on 404), got %d", got)
	}
}

func TestUploadServerConfigNoRetry(t *testing.T) {
	var calls int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"s3_unconfigured","message":"missing bucket"}`))
	}))
	defer srv.Close()

	open, _ := fileFactory([]byte("hi"))
	_, err := c.Upload(context.Background(), "x.txt", "text/plain", open)
	if !errors.Is(err, ErrServerConfig) {
		t.Fatalf("expected ErrServerConfig, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected 1 attempt, got %d", got)
	}
}

func TestUploadTransientThenSuccess(t *testing.T) {
	var calls int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":"upload_failed"}`))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"url":"https://x/y","key":"k","size":2,"content_type":"text/plain"}`))
	}))
	defer srv.Close()

	open, factCalls := fileFactory([]byte("hi"))
	res, err := c.Upload(context.Background(), "x.txt", "text/plain", open)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if res.URL != "https://x/y" {
		t.Errorf("URL = %q", res.URL)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("expected 2 server hits, got %d", got)
	}
	if got := atomic.LoadInt32(factCalls); got != 2 {
		t.Errorf("expected factory called 2x, got %d", got)
	}
}

func TestUploadTransientExhausted(t *testing.T) {
	var calls int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(503)
	}))
	defer srv.Close()

	open, factCalls := fileFactory([]byte("hi"))
	_, err := c.Upload(context.Background(), "x.txt", "text/plain", open)
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("expected ErrTransient, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("expected 3 server hits, got %d", got)
	}
	if got := atomic.LoadInt32(factCalls); got != 3 {
		t.Errorf("expected factory called 3x, got %d", got)
	}
}

func TestUploadNetworkErrorRetried(t *testing.T) {
	// Reserve a local port, then close the listener so dialing it fails fast.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	lis.Close() // immediate close → connect refuses

	c := NewClient("http://"+addr, "sk_test", &http.Client{Timeout: 2 * time.Second})
	c.backoff = func(int) time.Duration { return 0 }

	open, factCalls := fileFactory([]byte("hi"))
	_, err = c.Upload(context.Background(), "x.txt", "text/plain", open)
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("expected ErrTransient, got %v", err)
	}
	if got := atomic.LoadInt32(factCalls); got != 3 {
		t.Errorf("expected factory called 3x on network errors, got %d", got)
	}
}

func TestUploadFilenameDefault(t *testing.T) {
	var got string
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		fhs := r.MultipartForm.File["file"]
		if len(fhs) > 0 {
			got = fhs[0].Filename
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"url":"https://x/y","key":"k","size":2,"content_type":"text/plain"}`))
	}))
	defer srv.Close()

	open, _ := fileFactory([]byte("hi"))
	if _, err := c.Upload(context.Background(), "", "text/plain", open); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if got != "upload" {
		t.Errorf("default filename = %q, want %q", got, "upload")
	}
}

func TestUploadContextCanceled(t *testing.T) {
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hold the request long enough for the caller to cancel.
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

	open, _ := fileFactory([]byte("hi"))
	_, err := c.Upload(ctx, "x.txt", "text/plain", open)
	if err == nil {
		t.Fatal("expected error from canceled ctx")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestUploadResponseMissingURL(t *testing.T) {
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"key":"k","size":2}`))
	}))
	defer srv.Close()

	open, _ := fileFactory([]byte("hi"))
	_, err := c.Upload(context.Background(), "x.txt", "text/plain", open)
	if err == nil || !strings.Contains(err.Error(), "missing url") {
		t.Fatalf("expected missing url error, got %v", err)
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
