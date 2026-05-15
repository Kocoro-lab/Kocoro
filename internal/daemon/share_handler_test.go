package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// newTestShareServer wires a *Server with an in-memory cloud upstream, a temp
// SessionCache, and a pre-saved session. The supplied handler must answer
// POST /api/v1/uploads, GET /api/v1/uploads, and (for happy-path tests) the
// gateway's completion endpoint. Unknown paths get 503 by default.
func newTestShareServer(t *testing.T, handler http.HandlerFunc) (*Server, string) {
	t.Helper()
	cloud := httptest.NewServer(handler)
	t.Cleanup(cloud.Close)

	shannonDir := t.TempDir()
	sc := NewSessionCache(shannonDir)
	t.Cleanup(func() { sc.CloseAll() })

	cfg := &config.Config{Endpoint: cloud.URL, APIKey: "sk_test_key"}
	cfg.Cloud.Enabled = true

	s := &Server{
		deps: &ServerDeps{
			ShannonDir:   shannonDir,
			Config:       cfg,
			GW:           client.NewGatewayClient(cloud.URL, "sk_test_key"),
			SessionCache: sc,
		},
	}

	// Drop a real session.json into the default sessions dir so mgr.Load
	// finds it. Use session.NewStore directly — the manager's NewSession
	// helper would also work but Store.Save lets us hand-craft Messages
	// without going through ensureRuntimeLocked.
	store := session.NewStore(sc.SessionsDir(""))
	sess := &session.Session{
		ID:    "sess_test123",
		Title: "Test session for share",
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("explain the loader")},
			{Role: "assistant", Content: client.NewTextContent("three stages, A→B→C")},
		},
	}
	if err := store.Save(sess); err != nil {
		t.Fatalf("save session: %v", err)
	}
	return s, sess.ID
}

func TestHandleSessionShare_CloudDisabledReturns503(t *testing.T) {
	cfg := &config.Config{Endpoint: "http://x", APIKey: ""}
	cfg.Cloud.Enabled = true
	s := &Server{
		deps: &ServerDeps{
			ShannonDir:   t.TempDir(),
			Config:       cfg,
			GW:           client.NewGatewayClient("http://x", ""),
			SessionCache: NewSessionCache(t.TempDir()),
		},
	}
	req := httptest.NewRequest("POST", "/sessions/abc/share", nil)
	req.SetPathValue("id", "abc")
	rr := httptest.NewRecorder()
	s.handleSessionShare(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandleSessionShare_EmptyIDReturns400(t *testing.T) {
	s, _ := newTestShareServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream should not be touched")
	})
	req := httptest.NewRequest("POST", "/sessions//share", nil)
	rr := httptest.NewRecorder()
	s.handleSessionShare(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestHandleSessionShare_PathTraversalReturns400(t *testing.T) {
	s, _ := newTestShareServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream should not be touched")
	})
	req := httptest.NewRequest("POST", "/sessions/x/share", nil)
	req.SetPathValue("id", "../etc/passwd")
	rr := httptest.NewRecorder()
	s.handleSessionShare(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestHandleSessionShare_SessionNotFoundReturns404(t *testing.T) {
	s, _ := newTestShareServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream should not be touched")
	})
	req := httptest.NewRequest("POST", "/sessions/nonexistent/share", nil)
	req.SetPathValue("id", "nonexistent")
	rr := httptest.NewRecorder()
	s.handleSessionShare(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandleSessionShare_HappyPath(t *testing.T) {
	// Cloud upstream that handles all three calls share makes:
	//   1. completions (gateway) — return a short Haiku-like summary
	//   2. POST /api/v1/uploads — accept the multipart, return url+key
	//   3. GET  /api/v1/uploads — return one row matching the upload's URL
	var gotUploadHTML string
	cloudHandler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/api/v1/uploads"):
			// Capture the uploaded file body for content assertions.
			if err := r.ParseMultipartForm(32 << 20); err != nil {
				t.Errorf("ParseMultipartForm: %v", err)
			}
			for _, fileHeaders := range r.MultipartForm.File {
				for _, fh := range fileHeaders {
					f, _ := fh.Open()
					buf := make([]byte, fh.Size)
					_, _ = f.Read(buf)
					gotUploadHTML = string(buf)
					_ = f.Close()
				}
			}
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{
				"url":"https://cdn.example/shared/abc.html",
				"key":"shared/abc.html",
				"size":4096,
				"content_type":"text/html"
			}`))
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v1/uploads"):
			// The post-upload list lookup: include our row with matching URL.
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{
				"uploads":[{
					"id":"upload-uuid-123",
					"url":"https://cdn.example/shared/abc.html",
					"filename":"session-foo.html",
					"content_type":"text/html",
					"size":4096,
					"created_at":"2026-05-15T10:00:00Z"
				}],
				"total_count":1
			}`))
		case strings.Contains(r.URL.Path, "messages") || strings.Contains(r.URL.Path, "completions"):
			// Gateway completion endpoint — return a synthesized response
			// matching client.CompletionResponse JSON. If this path doesn't
			// match exactly the share.Render call falls back to the title-
			// based summary, which is still a valid happy path for the test
			// (we don't assert summary content here, only that the share
			// pipeline doesn't choke on the LLM call).
			w.WriteHeader(503)
		default:
			http.Error(w, "unexpected: "+r.URL.Path, http.StatusInternalServerError)
		}
	}
	s, sessID := newTestShareServer(t, cloudHandler)

	req := httptest.NewRequest("POST", "/sessions/"+sessID+"/share", nil)
	req.SetPathValue("id", sessID)
	rr := httptest.NewRecorder()
	s.handleSessionShare(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	var body struct {
		URL             string `json:"url"`
		Key             string `json:"key"`
		Size            int64  `json:"size"`
		UploadID        string `json:"upload_id"`
		SummaryFallback bool   `json:"summary_fallback"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.URL != "https://cdn.example/shared/abc.html" {
		t.Errorf("url = %q", body.URL)
	}
	if body.UploadID != "upload-uuid-123" {
		t.Errorf("upload_id = %q, want the matched list row id", body.UploadID)
	}
	if !body.SummaryFallback {
		t.Errorf("expected summary fallback (gateway 503 in this test) but got Haiku path")
	}
	if !strings.Contains(gotUploadHTML, "<!DOCTYPE html>") || !strings.Contains(gotUploadHTML, "explain the loader") {
		t.Errorf("uploaded HTML missing expected content. First 400 bytes:\n%s", firstN(gotUploadHTML, 400))
	}
}

func TestBuildShareFilename(t *testing.T) {
	now := time.Date(2026, 5, 15, 8, 12, 14, 0, time.UTC)
	cases := []struct {
		name      string
		title     string
		sessionID string
		want      string
	}{
		{
			name:      "english title slugged",
			title:     "Refactor the loader",
			sessionID: "sess_abc",
			want:      "session-Refactor-the-loader-20260515-081214.html",
		},
		{
			name:      "chinese title preserved",
			title:     "现在支持哪些模型",
			sessionID: "sess_abc",
			want:      "session-现在支持哪些模型-20260515-081214.html",
		},
		{
			name:      "filesystem-unsafe chars stripped",
			title:     "report/2025: q3/q4 ★",
			sessionID: "sess_abc",
			want:      "session-report2025-q3q4-20260515-081214.html",
		},
		{
			name:      "empty title falls back to short session id",
			title:     "",
			sessionID: "2026-05-15-ec4484bab957",
			want:      "session-2026-05-15-ec4484bab957-20260515-081214.html",
		},
		{
			name:      "title that sanitizes to empty falls back",
			title:     "????///***",
			sessionID: "sess_xyz",
			want:      "session-sess_xyz-20260515-081214.html",
		},
		{
			name:      "title trimmed to 40 runes",
			title:     strings.Repeat("长", 60),
			sessionID: "sess_x",
			want:      "session-" + strings.Repeat("长", 40) + "-20260515-081214.html",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildShareFilename(tc.title, tc.sessionID, now)
			if got != tc.want {
				t.Errorf("got\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}

func TestHandleSessionShare_FilenameContainsTitle(t *testing.T) {
	// End-to-end check: the multipart filename presented to the cloud
	// upstream incorporates the session title so the "Published files"
	// listing is human-scannable.
	var gotFilename string
	s, sessID := newTestShareServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/api/v1/uploads") {
			_ = r.ParseMultipartForm(32 << 20)
			for _, fhs := range r.MultipartForm.File {
				for _, fh := range fhs {
					gotFilename = fh.Filename
				}
			}
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"url":"https://x/y","key":"k","size":100,"content_type":"text/html"}`))
		} else if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v1/uploads") {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"uploads":[],"total_count":0}`))
		} else {
			w.WriteHeader(503)
		}
	})

	req := httptest.NewRequest("POST", "/sessions/"+sessID+"/share", nil)
	req.SetPathValue("id", sessID)
	rr := httptest.NewRecorder()
	s.handleSessionShare(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	// newTestShareServer seeds Title = "Test session for share".
	if !strings.HasPrefix(gotFilename, "session-Test-session-for-share-") {
		t.Errorf("expected filename to include slugged title, got %q", gotFilename)
	}
	if !strings.HasSuffix(gotFilename, ".html") {
		t.Errorf("expected .html suffix, got %q", gotFilename)
	}
}

func TestHandleSessionShareRetract_HappyPath(t *testing.T) {
	var gotMethod, gotPath string
	s, sessID := newTestShareServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"deleted":true,"id":"upload-uuid-123","cdn_eviction_seconds":300}`))
	})
	req := httptest.NewRequest("DELETE", "/sessions/"+sessID+"/share?upload_id=upload-uuid-123", nil)
	req.SetPathValue("id", sessID)
	rr := httptest.NewRecorder()
	s.handleSessionShareRetract(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if gotMethod != "DELETE" || gotPath != "/api/v1/uploads/upload-uuid-123" {
		t.Errorf("upstream called with %s %s", gotMethod, gotPath)
	}
}

func TestHandleSessionShareRetract_MissingUploadID(t *testing.T) {
	s, sessID := newTestShareServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream should not be touched")
	})
	req := httptest.NewRequest("DELETE", "/sessions/"+sessID+"/share", nil)
	req.SetPathValue("id", sessID)
	rr := httptest.NewRecorder()
	s.handleSessionShareRetract(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestHandleSessionShareRetract_CloudDisabled(t *testing.T) {
	cfg := &config.Config{Endpoint: "http://x", APIKey: ""}
	cfg.Cloud.Enabled = true
	s := &Server{
		deps: &ServerDeps{
			ShannonDir:   t.TempDir(),
			Config:       cfg,
			GW:           client.NewGatewayClient("http://x", ""),
			SessionCache: NewSessionCache(t.TempDir()),
		},
	}
	req := httptest.NewRequest("DELETE", "/sessions/abc/share?upload_id=u1", nil)
	req.SetPathValue("id", "abc")
	rr := httptest.NewRecorder()
	s.handleSessionShareRetract(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

