package daemon

import (
	"encoding/json"
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
