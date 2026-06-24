package daemon

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

// newTestServerWithCloud builds a minimal *Server pointed at a fake cloud
// upstream. The fake's handler is the test's responsibility.
func newTestServerWithCloud(t *testing.T, cloudHandler http.HandlerFunc) (*Server, *httptest.Server) {
	t.Helper()
	cloud := httptest.NewServer(cloudHandler)
	t.Cleanup(cloud.Close)

	cfg := &config.Config{
		Endpoint: cloud.URL,
		APIKey:   "sk_test_key",
	}
	cfg.Cloud.Enabled = true

	s := &Server{
		deps: &ServerDeps{
			ShannonDir: t.TempDir(),
			Config:     cfg,
			GW:         client.NewGatewayClient(cloud.URL, "sk_test_key"),
		},
	}
	return s, cloud
}

// --- handleListUploads ---

func TestHandleListUploads_HappyPath(t *testing.T) {
	var gotQuery string
	var gotAPIKey string
	s, _ := newTestServerWithCloud(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/uploads" {
			gotQuery = r.URL.RawQuery
			gotAPIKey = r.Header.Get("X-API-Key")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{
				"uploads": [{"id":"abc","url":"https://x/y","filename":"f","content_type":"text/plain","size":1,"created_at":"2026-05-14T00:00:00Z"}],
				"total_count": 1
			}`))
			return
		}
		t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
	})

	req := httptest.NewRequest("GET", "/uploads?limit=50&offset=10", nil)
	rr := httptest.NewRecorder()
	s.handleListUploads(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if gotAPIKey != "sk_test_key" {
		t.Errorf("X-API-Key not forwarded; got %q", gotAPIKey)
	}
	if gotQuery != "limit=50&offset=10" {
		t.Errorf("query forwarded = %q, want limit=50&offset=10", gotQuery)
	}
	var body struct {
		Uploads    []map[string]any `json:"uploads"`
		TotalCount int              `json:"total_count"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.TotalCount != 1 {
		t.Errorf("total_count = %d, want 1", body.TotalCount)
	}
}

func TestHandleListUploads_ClampsLimit(t *testing.T) {
	var gotQuery string
	s, _ := newTestServerWithCloud(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"uploads":[],"total_count":0}`))
	})

	req := httptest.NewRequest("GET", "/uploads?limit=9999", nil)
	rr := httptest.NewRecorder()
	s.handleListUploads(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if !strings.Contains(gotQuery, "limit=100") {
		t.Errorf("limit not clamped to 100; query = %q", gotQuery)
	}
}

func TestHandleListUploads_CloudDisabledReturns503(t *testing.T) {
	s := &Server{
		deps: &ServerDeps{
			ShannonDir: t.TempDir(),
			Config:     &config.Config{APIKey: "sk_test"}, // Cloud.Enabled = false
			GW:         client.NewGatewayClient("http://nope", "sk_test"),
		},
	}
	req := httptest.NewRequest("GET", "/uploads", nil)
	rr := httptest.NewRecorder()
	s.handleListUploads(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

func TestHandleListUploads_NoAPIKeyReturns503(t *testing.T) {
	cfg := &config.Config{Endpoint: "http://x", APIKey: ""}
	cfg.Cloud.Enabled = true
	s := &Server{
		deps: &ServerDeps{
			ShannonDir: t.TempDir(),
			Config:     cfg,
			GW:         client.NewGatewayClient("http://x", ""),
		},
	}
	req := httptest.NewRequest("GET", "/uploads", nil)
	rr := httptest.NewRecorder()
	s.handleListUploads(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

func TestHandleListUploads_ForwardsKindToCloud(t *testing.T) {
	var gotQuery string
	s, _ := newTestServerWithCloud(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"uploads":[],"total_count":0}`))
	})

	req := httptest.NewRequest("GET", "/uploads?kind=session_share&limit=5", nil)
	rr := httptest.NewRecorder()
	s.handleListUploads(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(gotQuery, "kind=session_share") {
		t.Errorf("kind not forwarded; query = %q", gotQuery)
	}
}

func TestHandleListUploads_RejectsInvalidKindLocally(t *testing.T) {
	cloudHit := false
	s, _ := newTestServerWithCloud(t, func(w http.ResponseWriter, r *http.Request) {
		cloudHit = true
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"uploads":[],"total_count":0}`))
	})

	req := httptest.NewRequest("GET", "/uploads?kind=bogus", nil)
	rr := httptest.NewRecorder()
	s.handleListUploads(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
	if cloudHit {
		t.Error("cloud should NOT have been hit for invalid kind")
	}
}

func TestHandleListUploads_CloudUnauthorizedMapsTo401(t *testing.T) {
	s, _ := newTestServerWithCloud(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	})
	req := httptest.NewRequest("GET", "/uploads", nil)
	rr := httptest.NewRecorder()
	s.handleListUploads(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

// --- handleDeleteUpload ---

func TestHandleDeleteUpload_HappyPath(t *testing.T) {
	var gotMethod, gotPath string
	s, _ := newTestServerWithCloud(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"deleted":true,"id":"abc","cdn_eviction_seconds":300}`))
	})
	req := httptest.NewRequest("DELETE", "/uploads/abc", nil)
	req.SetPathValue("id", "abc")
	rr := httptest.NewRecorder()
	s.handleDeleteUpload(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if gotMethod != "DELETE" {
		t.Errorf("upstream method = %q", gotMethod)
	}
	if gotPath != "/api/v1/uploads/abc" {
		t.Errorf("upstream path = %q", gotPath)
	}
	var body struct {
		Deleted bool `json:"deleted"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !body.Deleted {
		t.Errorf("deleted = false in response")
	}
}

func TestHandleDeleteUpload_MissingIDReturns400(t *testing.T) {
	s, _ := newTestServerWithCloud(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream should not be hit when id is empty")
	})
	req := httptest.NewRequest("DELETE", "/uploads/", nil)
	// no SetPathValue → empty id
	rr := httptest.NewRecorder()
	s.handleDeleteUpload(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestHandleDeleteUpload_CloudNotFoundMapsTo404(t *testing.T) {
	s, _ := newTestServerWithCloud(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"not_found","message":"Upload not found"}`))
	})
	req := httptest.NewRequest("DELETE", "/uploads/nonexistent", nil)
	req.SetPathValue("id", "nonexistent")
	rr := httptest.NewRecorder()
	s.handleDeleteUpload(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestHandleDeleteUpload_BareCloud404StillMapsTo404(t *testing.T) {
	// Pins the design choice: in the delete path, classifyError treats ALL
	// 404s as ErrNotFound — including a bare "404 page not found" from a
	// proxy that has the route un-mounted. We chose not to disambiguate
	// because (a) cloud is the source of truth for "endpoint deployed" and
	// (b) the user-facing meaning ("the file you're trying to retract is no
	// longer accessible") is the same whether the row was deleted or the
	// endpoint isn't there.
	s, _ := newTestServerWithCloud(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	req := httptest.NewRequest("DELETE", "/uploads/x", nil)
	req.SetPathValue("id", "x")
	rr := httptest.NewRecorder()
	s.handleDeleteUpload(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestHandleDeleteUpload_CloudDisabledReturns503(t *testing.T) {
	s := &Server{
		deps: &ServerDeps{
			ShannonDir: t.TempDir(),
			Config:     &config.Config{APIKey: "sk_test"}, // Cloud.Enabled = false
			GW:         client.NewGatewayClient("http://nope", "sk_test"),
		},
	}
	req := httptest.NewRequest("DELETE", "/uploads/abc", nil)
	req.SetPathValue("id", "abc")
	rr := httptest.NewRecorder()
	s.handleDeleteUpload(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

// --- handleCreateUpload ---

// newImageUploadRequest builds a multipart POST /uploads carrying a "file" part
// (with the given content_type as the part header) and an optional content_type
// form field. Passing fieldType="" omits the form field, exercising the
// fallback to the part's own Content-Type.
func newImageUploadRequest(t *testing.T, partType, fieldType string, data []byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	hdr := make(map[string][]string)
	hdr["Content-Disposition"] = []string{`form-data; name="file"; filename="avatar.png"`}
	if partType != "" {
		hdr["Content-Type"] = []string{partType}
	}
	part, err := mw.CreatePart(hdr)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if fieldType != "" {
		if err := mw.WriteField("content_type", fieldType); err != nil {
			t.Fatalf("write field: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	req := httptest.NewRequest("POST", "/uploads", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func TestHandleCreateUpload_HappyPath(t *testing.T) {
	var gotMethod, gotPath, gotAPIKey, gotKind string
	s, _ := newTestServerWithCloud(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("X-API-Key")
		_ = r.ParseMultipartForm(1 << 20)
		gotKind = r.FormValue("kind")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"url":"https://static.kocoro.ai/public/h/avatar.png","key":"k","size":4,"content_type":"image/png"}`))
	})

	req := newImageUploadRequest(t, "image/png", "image/png", []byte("\x89PNG"))
	rr := httptest.NewRecorder()
	s.handleCreateUpload(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	// Avatars use the EPHEMERAL endpoint so they aren't recorded in the user's
	// upload library.
	if gotMethod != "POST" || gotPath != "/api/v1/uploads/ephemeral" {
		t.Errorf("upstream = %s %s, want POST /api/v1/uploads/ephemeral", gotMethod, gotPath)
	}
	if gotAPIKey != "sk_test_key" {
		t.Errorf("X-API-Key not forwarded; got %q", gotAPIKey)
	}
	// The ephemeral endpoint has no library row to classify — no kind sent.
	if gotKind != "" {
		t.Errorf("kind = %q, want empty (ephemeral upload sends no kind)", gotKind)
	}
	var body struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.HasPrefix(body.URL, "https://static.kocoro.ai/") {
		t.Errorf("url = %q, want a static.kocoro.ai URL", body.URL)
	}
}

func TestHandleCreateUpload_MissingFileReturns400(t *testing.T) {
	s, _ := newTestServerWithCloud(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream should not be hit when file is missing")
	})
	// A valid multipart form with no "file" part.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("content_type", "image/png")
	_ = mw.Close()
	req := httptest.NewRequest("POST", "/uploads", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	s.handleCreateUpload(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestHandleCreateUpload_BadTypeReturns400(t *testing.T) {
	s, _ := newTestServerWithCloud(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream should not be hit for a disallowed type")
	})
	req := newImageUploadRequest(t, "application/pdf", "application/pdf", []byte("%PDF"))
	rr := httptest.NewRecorder()
	s.handleCreateUpload(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestHandleCreateUpload_FileTooLargeMapsTo413(t *testing.T) {
	s, _ := newTestServerWithCloud(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"error":"file_too_large","message":"exceeds 50 MiB"}`))
	})
	req := newImageUploadRequest(t, "image/png", "image/png", []byte("\x89PNG"))
	rr := httptest.NewRecorder()
	s.handleCreateUpload(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413 (cloud 413 must round-trip)", rr.Code)
	}
}

func TestHandleCreateUpload_CloudDisabledReturns503(t *testing.T) {
	s := &Server{
		deps: &ServerDeps{
			ShannonDir: t.TempDir(),
			Config:     &config.Config{APIKey: "sk_test"}, // Cloud.Enabled = false
			GW:         client.NewGatewayClient("http://nope", "sk_test"),
		},
	}
	req := newImageUploadRequest(t, "image/png", "image/png", []byte("\x89PNG"))
	rr := httptest.NewRecorder()
	s.handleCreateUpload(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

// Over the daemon's own 10 MiB MaxBytesReader cap → 413 locally, before any
// cloud round trip (distinct from FileTooLargeMapsTo413, which tests the cloud's
// 413 propagating back).
func TestHandleCreateUpload_OverLocalCapReturns413(t *testing.T) {
	s, _ := newTestServerWithCloud(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream must not be hit when the body exceeds the local cap")
	})
	big := make([]byte, 11<<20) // 11 MiB > maxUploadSize (10 MiB)
	req := newImageUploadRequest(t, "image/png", "image/png", big)
	rr := httptest.NewRecorder()
	s.handleCreateUpload(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413 (local cap)", rr.Code)
	}
}

// content_type form field omitted → MIME resolves from the file part's own
// Content-Type header.
func TestHandleCreateUpload_ContentTypeFromPartHeader(t *testing.T) {
	s, _ := newTestServerWithCloud(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"url":"https://static.kocoro.ai/public/h/a.png"}`))
	})
	req := newImageUploadRequest(t, "image/png", "" /* no content_type field */, []byte("\x89PNG"))
	rr := httptest.NewRecorder()
	s.handleCreateUpload(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s (part-header MIME should be honored)", rr.Code, rr.Body.String())
	}
}

// A "; charset=…" suffix on the declared MIME is stripped before the whitelist
// check, so it still validates as image/png.
func TestHandleCreateUpload_StripsCharsetParam(t *testing.T) {
	s, _ := newTestServerWithCloud(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"url":"https://static.kocoro.ai/public/h/a.png"}`))
	})
	req := newImageUploadRequest(t, "image/png", "image/png; charset=utf-8", []byte("\x89PNG"))
	rr := httptest.NewRecorder()
	s.handleCreateUpload(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s (charset param should be stripped)", rr.Code, rr.Body.String())
	}
}
