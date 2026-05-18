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
	res, err := c.Upload(context.Background(), open, UploadOptions{Filename: "landing.html", ContentType: "text/html"})
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
	_, err := c.Upload(context.Background(), open, UploadOptions{Filename: "x.txt", ContentType: "text/plain"})
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
	_, err := c.Upload(context.Background(), open, UploadOptions{Filename: "x.txt", ContentType: "text/plain"})
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
	_, err := c.Upload(context.Background(), open, UploadOptions{Filename: "x.txt", ContentType: "text/plain"})
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
	_, err := c.Upload(context.Background(), open, UploadOptions{Filename: "x.txt", ContentType: "text/plain"})
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
	res, err := c.Upload(context.Background(), open, UploadOptions{Filename: "x.txt", ContentType: "text/plain"})
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
	_, err := c.Upload(context.Background(), open, UploadOptions{Filename: "x.txt", ContentType: "text/plain"})
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
	_, err = c.Upload(context.Background(), open, UploadOptions{Filename: "x.txt", ContentType: "text/plain"})
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
	if _, err := c.Upload(context.Background(), open, UploadOptions{ContentType: "text/plain"}); err != nil {
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
	_, err := c.Upload(ctx, open, UploadOptions{Filename: "x.txt", ContentType: "text/plain"})
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
	_, err := c.Upload(context.Background(), open, UploadOptions{Filename: "x.txt", ContentType: "text/plain"})
	if err == nil || !strings.Contains(err.Error(), "missing url") {
		t.Fatalf("expected missing url error, got %v", err)
	}
}

// --- List ---

func TestListHappyPath(t *testing.T) {
	var got struct {
		method    string
		path      string
		query     string
		hadAPIKey bool
		accept    string
	}
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		got.query = r.URL.RawQuery
		got.hadAPIKey = r.Header.Get("X-API-Key") == "sk_test_key"
		got.accept = r.Header.Get("Accept")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{
			"uploads": [
				{"id":"6b28b36c-d218-448f-adb5-e256218fb025","url":"https://x/y/a.html","filename":"a.html","content_type":"text/html","size":2688,"created_at":"2026-05-14T07:19:09.781827Z"},
				{"id":"abcd-1234","url":"https://x/y/b.png","filename":"b.png","content_type":"image/png","size":1024,"created_at":"2026-05-13T11:00:00Z"}
			],
			"total_count": 2
		}`))
	}))
	defer srv.Close()

	res, err := c.List(context.Background(), ListOptions{Limit: 50, Offset: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got.method != "GET" {
		t.Errorf("method = %q, want GET", got.method)
	}
	if got.path != "/api/v1/uploads" {
		t.Errorf("path = %q", got.path)
	}
	if got.query != "limit=50&offset=10" {
		t.Errorf("query = %q", got.query)
	}
	if !got.hadAPIKey {
		t.Errorf("X-API-Key header missing")
	}
	if got.accept != "application/json" {
		t.Errorf("Accept header = %q, want application/json", got.accept)
	}
	if res.TotalCount != 2 {
		t.Errorf("total_count = %d, want 2", res.TotalCount)
	}
	if len(res.Uploads) != 2 {
		t.Fatalf("got %d entries, want 2", len(res.Uploads))
	}
	if res.Uploads[0].ID != "6b28b36c-d218-448f-adb5-e256218fb025" {
		t.Errorf("Uploads[0].ID = %q", res.Uploads[0].ID)
	}
	if res.Uploads[0].Size != 2688 {
		t.Errorf("Uploads[0].Size = %d", res.Uploads[0].Size)
	}
}

func TestListOmitsZeroPagingParams(t *testing.T) {
	var gotQuery string
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"uploads":[],"total_count":0}`))
	}))
	defer srv.Close()

	if _, err := c.List(context.Background(), ListOptions{}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if gotQuery != "" {
		t.Errorf("expected empty query when limit/offset == 0, got %q", gotQuery)
	}
}

func TestListEmptyUploadsArrayNotNil(t *testing.T) {
	// Cloud may serialize empty list as `"uploads": null`; clients should
	// always receive an empty slice so range-loops are safe.
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"uploads":null,"total_count":0}`))
	}))
	defer srv.Close()

	res, err := c.List(context.Background(), ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if res.Uploads == nil {
		t.Error("Uploads is nil; expected non-nil empty slice")
	}
}

func TestListUnauthorizedNoRetry(t *testing.T) {
	var calls int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"Authentication required"}`))
	}))
	defer srv.Close()

	_, err := c.List(context.Background(), ListOptions{Limit: 20})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected 1 attempt, got %d", got)
	}
}

func TestListTransientExhausted(t *testing.T) {
	var calls int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(503)
	}))
	defer srv.Close()

	_, err := c.List(context.Background(), ListOptions{Limit: 20})
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("expected ErrTransient, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("expected 3 attempts, got %d", got)
	}
}

// --- Delete ---

func TestDeleteHappyPath(t *testing.T) {
	var got struct {
		method    string
		path      string
		hadAPIKey bool
		hasBody   bool
	}
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		got.hadAPIKey = r.Header.Get("X-API-Key") == "sk_test_key"
		body, _ := io.ReadAll(r.Body)
		got.hasBody = len(body) > 0

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{
			"deleted": true,
			"id": "6b28b36c-d218-448f-adb5-e256218fb025",
			"cdn_eviction_seconds": 300
		}`))
	}))
	defer srv.Close()

	res, err := c.Delete(context.Background(), "6b28b36c-d218-448f-adb5-e256218fb025")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got.method != "DELETE" {
		t.Errorf("method = %q, want DELETE", got.method)
	}
	if got.path != "/api/v1/uploads/6b28b36c-d218-448f-adb5-e256218fb025" {
		t.Errorf("path = %q", got.path)
	}
	if !got.hadAPIKey {
		t.Errorf("X-API-Key header missing")
	}
	if got.hasBody {
		t.Errorf("DELETE should not send a request body")
	}
	if !res.Deleted {
		t.Errorf("Deleted = false, want true")
	}
	if res.ID != "6b28b36c-d218-448f-adb5-e256218fb025" {
		t.Errorf("ID = %q", res.ID)
	}
	if res.CDNEvictionSeconds != 300 {
		t.Errorf("CDNEvictionSeconds = %d, want 300", res.CDNEvictionSeconds)
	}
}

func TestDeleteEmptyIDClientSide(t *testing.T) {
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be hit when id is empty")
	}))
	defer srv.Close()

	_, err := c.Delete(context.Background(), "")
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("expected ErrBadRequest for empty id, got %v", err)
	}
	_, err = c.Delete(context.Background(), "   ")
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("expected ErrBadRequest for whitespace id, got %v", err)
	}
}

func TestDeleteNotFoundNoRetry(t *testing.T) {
	var calls int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"not_found","message":"Upload not found"}`))
	}))
	defer srv.Close()

	_, err := c.Delete(context.Background(), "nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if errors.Is(err, ErrEndpointNotFound) {
		t.Errorf("delete 404 must NOT classify as ErrEndpointNotFound")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected 1 attempt (no retry on 404), got %d", got)
	}
}

func TestDeleteUnauthorizedNoRetry(t *testing.T) {
	var calls int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"unauthorized","message":"Authentication required"}`))
	}))
	defer srv.Close()

	_, err := c.Delete(context.Background(), "abc")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected 1 attempt, got %d", got)
	}
}

func TestDeleteTransientThenSuccess(t *testing.T) {
	var calls int32
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"deleted":true,"id":"abc","cdn_eviction_seconds":300}`))
	}))
	defer srv.Close()

	res, err := c.Delete(context.Background(), "abc")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !res.Deleted {
		t.Errorf("Deleted = false after retry; want true")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("expected 2 server hits, got %d", got)
	}
}

func TestDeleteIDPathEscaped(t *testing.T) {
	// Defense in depth — caller is supposed to pass a UUID, but if a malformed
	// id slips through it must not break the URL parser. RequestURI keeps the
	// original (escaped) form; r.URL.Path is already %2F → '/' decoded by
	// net/http, so it can't be used to detect escaping.
	var gotURI string
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.RequestURI
		w.WriteHeader(404)
	}))
	defer srv.Close()

	_, _ = c.Delete(context.Background(), "weird id/with spaces")
	if !strings.HasPrefix(gotURI, "/api/v1/uploads/") {
		t.Errorf("URI = %q, expected /api/v1/uploads/ prefix", gotURI)
	}
	if !strings.Contains(gotURI, "%2F") {
		t.Errorf("URI = %q must contain %%2F (PathEscape of '/') so the id stays in one segment", gotURI)
	}
	if !strings.Contains(gotURI, "%20") {
		t.Errorf("URI = %q must contain %%20 (PathEscape of space)", gotURI)
	}
}

// --- classifyError op disambiguation ---

func TestClassifyError404UploadOpReturnsEndpointNotFound(t *testing.T) {
	err := classifyError(404, []byte(""), "upload")
	if !errors.Is(err, ErrEndpointNotFound) {
		t.Errorf("upload op 404 → %v, want ErrEndpointNotFound", err)
	}
}

func TestClassifyError404DeleteOpReturnsNotFound(t *testing.T) {
	err := classifyError(404, []byte(`{"error":"not_found"}`), "delete")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("delete op 404 → %v, want ErrNotFound", err)
	}
	if errors.Is(err, ErrEndpointNotFound) {
		t.Errorf("delete op 404 must NOT carry ErrEndpointNotFound")
	}
}

func TestClassifyError404ListOpReturnsEndpointNotFound(t *testing.T) {
	// List 404 means the endpoint isn't deployed (cloud has no other reason
	// to return 404 for a GET on the collection root).
	err := classifyError(404, []byte(""), "list")
	if !errors.Is(err, ErrEndpointNotFound) {
		t.Errorf("list op 404 → %v, want ErrEndpointNotFound", err)
	}
}

// --- kind / metadata wiring ---

func TestUploadKindAndMetadataInForm(t *testing.T) {
	var got struct {
		kind     string
		metadata string
	}
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("ParseMultipartForm: %v", err)
		}
		got.kind = r.FormValue("kind")
		got.metadata = r.FormValue("metadata")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"url":"https://x/y","key":"k","size":2,"content_type":"text/html","kind":"session_share","metadata":{"session_id":"abc"}}`))
	}))
	defer srv.Close()

	open, _ := fileFactory([]byte("hi"))
	res, err := c.Upload(context.Background(), open, UploadOptions{
		Filename:    "share.html",
		ContentType: "text/html",
		Kind:        KindSessionShare,
		Metadata:    []byte(`{"session_id":"abc"}`),
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if got.kind != "session_share" {
		t.Errorf("kind field = %q, want session_share", got.kind)
	}
	if got.metadata != `{"session_id":"abc"}` {
		t.Errorf("metadata field = %q", got.metadata)
	}
	if res.Kind != "session_share" {
		t.Errorf("response Kind = %q", res.Kind)
	}
	if string(res.Metadata) != `{"session_id":"abc"}` {
		t.Errorf("response Metadata = %q", string(res.Metadata))
	}
}

func TestUploadOmitsKindAndMetadataWhenZero(t *testing.T) {
	var got struct {
		hasKind     bool
		hasMetadata bool
	}
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("ParseMultipartForm: %v", err)
		}
		_, got.hasKind = r.MultipartForm.Value["kind"]
		_, got.hasMetadata = r.MultipartForm.Value["metadata"]
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"url":"https://x/y","key":"k","size":2,"content_type":"text/plain"}`))
	}))
	defer srv.Close()

	open, _ := fileFactory([]byte("hi"))
	if _, err := c.Upload(context.Background(), open, UploadOptions{ContentType: "text/plain"}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if got.hasKind {
		t.Error("kind field should NOT be present when Kind == \"\"")
	}
	if got.hasMetadata {
		t.Error("metadata field should NOT be present when Metadata == nil")
	}
}

func TestUploadInvalidKindRejectedLocally(t *testing.T) {
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be hit when kind is invalid")
	}))
	defer srv.Close()

	open, _ := fileFactory([]byte("hi"))
	_, err := c.Upload(context.Background(), open, UploadOptions{Kind: "bogus"})
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("expected ErrBadRequest for invalid kind, got %v", err)
	}
}

func TestUploadInvalidKindFromServer(t *testing.T) {
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"invalid_kind","message":"kind 'foo' not allowed"}`))
	}))
	defer srv.Close()

	open, _ := fileFactory([]byte("hi"))
	// Send a valid-looking kind so the client doesn't short-circuit; rely on
	// the server response to drive the classification path.
	_, err := c.Upload(context.Background(), open, UploadOptions{Kind: KindOther})
	if !errors.Is(err, ErrInvalidKind) {
		t.Fatalf("expected ErrInvalidKind from server 400, got %v", err)
	}
}

func TestListWithKindFilter(t *testing.T) {
	var gotQuery string
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"uploads":[],"total_count":0}`))
	}))
	defer srv.Close()

	if _, err := c.List(context.Background(), ListOptions{Limit: 20, Kind: KindSessionShare}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if !strings.Contains(gotQuery, "kind=session_share") {
		t.Errorf("query = %q, expected kind=session_share", gotQuery)
	}
}

func TestListInvalidKindRejectedLocally(t *testing.T) {
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be hit when list kind is invalid")
	}))
	defer srv.Close()

	_, err := c.List(context.Background(), ListOptions{Kind: "bogus"})
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("expected ErrBadRequest, got %v", err)
	}
}

func TestListResponseCarriesKindAndMetadata(t *testing.T) {
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{
			"uploads": [
				{"id":"a","url":"https://x/y/a.html","filename":"a.html","content_type":"text/html","size":1,"kind":"session_share","metadata":{"session_id":"s1"},"created_at":"2026-05-18T00:00:00Z"}
			],
			"total_count": 1
		}`))
	}))
	defer srv.Close()

	res, err := c.List(context.Background(), ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if res.Uploads[0].Kind != "session_share" {
		t.Errorf("Kind = %q", res.Uploads[0].Kind)
	}
	if string(res.Uploads[0].Metadata) != `{"session_id":"s1"}` {
		t.Errorf("Metadata = %q", string(res.Uploads[0].Metadata))
	}
}

func TestIsValidKind(t *testing.T) {
	for _, k := range []string{KindSessionShare, KindReport, KindLandingPage, KindImage, KindOther} {
		if !IsValidKind(k) {
			t.Errorf("IsValidKind(%q) = false, want true", k)
		}
	}
	for _, k := range []string{"", "bogus", "Session_Share", "SESSION_SHARE"} {
		if IsValidKind(k) {
			t.Errorf("IsValidKind(%q) = true, want false", k)
		}
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
