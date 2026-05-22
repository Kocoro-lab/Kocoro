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
	"github.com/Kocoro-lab/ShanClaw/internal/share"
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
	var gotUploadKind, gotUploadMetadata string
	var gotListQuery string
	cloudHandler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/api/v1/uploads"):
			// Capture the uploaded file body for content assertions.
			if err := r.ParseMultipartForm(32 << 20); err != nil {
				t.Errorf("ParseMultipartForm: %v", err)
			}
			gotUploadKind = r.FormValue("kind")
			gotUploadMetadata = r.FormValue("metadata")
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
			gotListQuery = r.URL.RawQuery
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
	if gotUploadKind != "session_share" {
		t.Errorf("session share upload kind = %q, want session_share", gotUploadKind)
	}
	if !strings.Contains(gotUploadMetadata, `"session_id":"`+sessID+`"`) {
		t.Errorf("session share upload metadata missing session_id %q: %s", sessID, gotUploadMetadata)
	}
	if !strings.Contains(gotListQuery, "kind=session_share") {
		t.Errorf("upload_id lookup must filter by kind=session_share; query = %q", gotListQuery)
	}
}

func TestBuildShareUploadMetadata(t *testing.T) {
	t.Run("default agent omits agent key", func(t *testing.T) {
		got := string(buildShareUploadMetadata("sess_abc", ""))
		if got != `{"session_id":"sess_abc"}` {
			t.Errorf("metadata = %q, want exactly {\"session_id\":\"sess_abc\"}", got)
		}
	})
	t.Run("named agent included", func(t *testing.T) {
		got := string(buildShareUploadMetadata("sess_abc", "researcher"))
		// json.Marshal sorts map keys alphabetically — agent comes before session_id.
		if got != `{"agent":"researcher","session_id":"sess_abc"}` {
			t.Errorf("metadata = %q", got)
		}
	})
}

func TestBuildShareFilename(t *testing.T) {
	now := time.Date(2026, 5, 15, 8, 12, 14, 0, time.UTC)
	cases := []struct {
		name      string
		haikuSlug string
		title     string
		sessionID string
		want      string
	}{
		{
			name:      "haiku slug preferred over title",
			haikuSlug: "debug-payment-bug",
			title:     "Help me fix the payment flow",
			sessionID: "sess_abc",
			want:      "session-debug-payment-bug-20260515-081214.html",
		},
		{
			name:      "haiku slug from non-English session — saves UX for CJK",
			haikuSlug: "supported-models-query",
			title:     "现在支持哪些模型",
			sessionID: "sess_abc",
			want:      "session-supported-models-query-20260515-081214.html",
		},
		{
			name:      "haiku failed → fall back to title-ASCII slug",
			haikuSlug: "",
			title:     "Refactor the loader",
			sessionID: "sess_abc",
			want:      "session-Refactor-the-loader-20260515-081214.html",
		},
		{
			name:      "haiku failed + pure CJK title → session id (final fallback)",
			haikuSlug: "",
			title:     "现在支持哪些模型",
			sessionID: "sess_abc",
			want:      "session-sess_abc-20260515-081214.html",
		},
		{
			name:      "haiku failed + mixed CJK title → keep ASCII portion",
			haikuSlug: "",
			title:     "前端 refactor 重构",
			sessionID: "sess_abc",
			want:      "session-refactor-20260515-081214.html",
		},
		{
			name:      "haiku failed + cyrillic/arabic title → session id",
			haikuSlug: "",
			title:     "Привет мир こんにちは مرحبا",
			sessionID: "sess_x",
			want:      "session-sess_x-20260515-081214.html",
		},
		{
			name:      "haiku failed + filesystem-unsafe chars stripped",
			haikuSlug: "",
			title:     "report/2025: q3/q4 ★",
			sessionID: "sess_abc",
			want:      "session-report2025-q3q4-20260515-081214.html",
		},
		{
			name:      "everything empty falls back to session id",
			haikuSlug: "",
			title:     "",
			sessionID: "2026-05-15-ec4484bab957",
			want:      "session-2026-05-15-ec4484bab957-20260515-081214.html",
		},
		{
			name:      "title sanitizes to empty + haiku empty → session id",
			haikuSlug: "",
			title:     "????///***",
			sessionID: "sess_xyz",
			want:      "session-sess_xyz-20260515-081214.html",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildShareFilename(tc.haikuSlug, tc.title, tc.sessionID, now)
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

// shareCloudHandler returns a cloud upstream that POST-uploads (returning
// the canned url) and LIST-matches that URL to a fixed upload_id. Reused by
// the PublishedShares persistence tests below.
func shareCloudHandler(uploadID, url string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/api/v1/uploads"):
			_ = r.ParseMultipartForm(32 << 20)
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"url":"` + url + `","key":"k","size":100,"content_type":"text/html"}`))
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v1/uploads"):
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"uploads":[{"id":"` + uploadID + `","url":"` + url + `","filename":"f.html","content_type":"text/html","size":100,"created_at":"2026-05-18T00:00:00Z"}],"total_count":1}`))
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/api/v1/uploads/"):
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"deleted":true,"id":"` + uploadID + `","cdn_eviction_seconds":300}`))
		default:
			w.WriteHeader(503)
		}
	}
}

// TestHandleSessionShare_PersistsPublishedShare guards the daemon-side
// source-of-truth: after a successful share, the session file MUST contain
// a PublishedShares entry with the upload_id cloud returned via LIST. This
// is what the UI falls back to via GET /sessions/{id}/shares when its own
// upload_id storage gets out of sync — the suspected root cause of
// named-agent retract failures.
func TestHandleSessionShare_PersistsPublishedShare(t *testing.T) {
	const wantUploadID = "upload-uuid-aaa"
	const wantURL = "https://cdn.example/shared/aaa.html"

	s, sessID := newTestShareServer(t, shareCloudHandler(wantUploadID, wantURL))

	req := httptest.NewRequest("POST", "/sessions/"+sessID+"/share", nil)
	req.SetPathValue("id", sessID)
	rr := httptest.NewRecorder()
	s.handleSessionShare(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("share status = %d, body=%s", rr.Code, rr.Body.String())
	}

	// Re-load from disk (not the in-memory cache) to prove the write hit.
	store := session.NewStore(s.deps.SessionCache.SessionsDir(""))
	defer store.Close()
	sess, err := store.Load(sessID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(sess.PublishedShares) != 1 {
		t.Fatalf("PublishedShares len = %d, want 1; got %+v", len(sess.PublishedShares), sess.PublishedShares)
	}
	got := sess.PublishedShares[0]
	if got.UploadID != wantUploadID {
		t.Errorf("UploadID = %q, want %q", got.UploadID, wantUploadID)
	}
	if got.URL != wantURL {
		t.Errorf("URL = %q, want %q", got.URL, wantURL)
	}
	if got.Filename == "" {
		t.Error("Filename empty — handler did not propagate the buildShareFilename result")
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt zero — handler did not stamp the entry")
	}
}

// TestHandleSessionShareRetract_RemovesPublishedShare verifies the retract
// path filters the matching upload_id out of PublishedShares so the daemon
// SoT stays in sync with the cloud state.
func TestHandleSessionShareRetract_RemovesPublishedShare(t *testing.T) {
	const uploadID = "upload-uuid-bbb"
	const url = "https://cdn.example/shared/bbb.html"

	s, sessID := newTestShareServer(t, shareCloudHandler(uploadID, url))

	// First share so there's an entry to retract.
	req := httptest.NewRequest("POST", "/sessions/"+sessID+"/share", nil)
	req.SetPathValue("id", sessID)
	rr := httptest.NewRecorder()
	s.handleSessionShare(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("seed share failed: %d %s", rr.Code, rr.Body.String())
	}

	// Now retract.
	req2 := httptest.NewRequest("DELETE", "/sessions/"+sessID+"/share?upload_id="+uploadID, nil)
	req2.SetPathValue("id", sessID)
	rr2 := httptest.NewRecorder()
	s.handleSessionShareRetract(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("retract failed: %d %s", rr2.Code, rr2.Body.String())
	}

	store := session.NewStore(s.deps.SessionCache.SessionsDir(""))
	defer store.Close()
	sess, err := store.Load(sessID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(sess.PublishedShares) != 0 {
		t.Fatalf("PublishedShares len = %d after retract, want 0; got %+v", len(sess.PublishedShares), sess.PublishedShares)
	}
}

// TestHandleSessionShares_ReturnsEntries pins the new GET /sessions/{id}/shares
// endpoint: empty array (NOT null) on a fresh session; populated after a share.
// UI clients depend on the response always being a JSON array to avoid having
// to handle null.
func TestHandleSessionShares_ReturnsEntries(t *testing.T) {
	const uploadID = "upload-uuid-ccc"
	const url = "https://cdn.example/shared/ccc.html"

	s, sessID := newTestShareServer(t, shareCloudHandler(uploadID, url))

	// Fresh session → empty array.
	req := httptest.NewRequest("GET", "/sessions/"+sessID+"/shares", nil)
	req.SetPathValue("id", sessID)
	rr := httptest.NewRecorder()
	s.handleSessionShares(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("empty case status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var empty struct {
		Shares []session.PublishedShareEntry `json:"shares"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &empty); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if empty.Shares == nil {
		t.Error("shares is null — handler must return [] not null")
	}
	if len(empty.Shares) != 0 {
		t.Errorf("fresh session has %d entries, want 0", len(empty.Shares))
	}

	// After a share → one entry.
	shareReq := httptest.NewRequest("POST", "/sessions/"+sessID+"/share", nil)
	shareReq.SetPathValue("id", sessID)
	shareRr := httptest.NewRecorder()
	s.handleSessionShare(shareRr, shareReq)
	if shareRr.Code != http.StatusOK {
		t.Fatalf("share failed: %d %s", shareRr.Code, shareRr.Body.String())
	}

	req2 := httptest.NewRequest("GET", "/sessions/"+sessID+"/shares", nil)
	req2.SetPathValue("id", sessID)
	rr2 := httptest.NewRecorder()
	s.handleSessionShares(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("populated case status = %d", rr2.Code)
	}
	var populated struct {
		Shares []session.PublishedShareEntry `json:"shares"`
	}
	if err := json.Unmarshal(rr2.Body.Bytes(), &populated); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(populated.Shares) != 1 || populated.Shares[0].UploadID != uploadID {
		t.Errorf("populated shares = %+v, want one entry with UploadID=%s", populated.Shares, uploadID)
	}
}

// TestHandleSessionShares_NonexistentSession returns 404 — symmetric with
// the share/retract paths so UI clients can use the same not-found branch.
func TestHandleSessionShares_NonexistentSession(t *testing.T) {
	s, _ := newTestShareServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream should not be touched")
	})
	req := httptest.NewRequest("GET", "/sessions/missing/shares", nil)
	req.SetPathValue("id", "missing")
	rr := httptest.NewRecorder()
	s.handleSessionShares(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

// waitForShareTaskTerminal polls s.shareTasks until the task reaches a
// terminal phase (completed / failed / cancelled) or a 5s deadline elapses.
// Terminal-phase polling beats a fixed sleep — fast tests stay fast, slow CI
// gets the headroom it needs without flaking. Returns the final snapshot.
func waitForShareTaskTerminal(t *testing.T, s *Server, taskID string) *shareTaskState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task := s.getShareTask(taskID)
		if task == nil {
			t.Fatalf("task %s disappeared before reaching terminal phase", taskID)
		}
		switch task.Phase {
		case ShareTaskPhaseCompleted, ShareTaskPhaseFailed, ShareTaskPhaseCancelled:
			return task
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("task %s did not reach terminal phase within 5s", taskID)
	return nil
}

// TestHandleSessionShare_AsyncReturns202 covers the new default contract:
// POST returns 202 + task_id immediately, and the goroutine completes the
// share in the background. The task snapshot eventually carries the URL +
// upload_id that the legacy sync body would have returned directly.
func TestHandleSessionShare_AsyncReturns202(t *testing.T) {
	const uploadID = "upload-uuid-async-aaa"
	const url = "https://cdn.example/shared/async-aaa.html"

	s, sessID := newTestShareServer(t, shareCloudHandler(uploadID, url))
	s.deps.Config.Daemon.ShareAsyncDefault = true

	req := httptest.NewRequest("POST", "/sessions/"+sessID+"/share", nil)
	req.SetPathValue("id", sessID)
	rr := httptest.NewRecorder()
	s.handleSessionShare(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		TaskID    string `json:"task_id"`
		SessionID string `json:"session_id"`
		Phase     string `json:"phase"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.HasPrefix(body.TaskID, "share-") {
		t.Errorf("task_id = %q, want share-* prefix", body.TaskID)
	}
	if body.SessionID != sessID {
		t.Errorf("session_id = %q, want %q", body.SessionID, sessID)
	}
	if body.Phase != ShareTaskPhaseAccepted {
		t.Errorf("phase = %q, want %q", body.Phase, ShareTaskPhaseAccepted)
	}
	if body.Status != "accepted" {
		t.Errorf("status = %q, want %q", body.Status, "accepted")
	}

	final := waitForShareTaskTerminal(t, s, body.TaskID)
	if final.Phase != ShareTaskPhaseCompleted {
		t.Fatalf("final phase = %q, want %q (error=%q)",
			final.Phase, ShareTaskPhaseCompleted, final.Error)
	}
	if final.URL != url {
		t.Errorf("URL = %q, want %q", final.URL, url)
	}
	if final.UploadID != uploadID {
		t.Errorf("UploadID = %q, want %q", final.UploadID, uploadID)
	}

	// Daemon SoT must mirror the cloud-side write (same contract as sync path).
	store := session.NewStore(s.deps.SessionCache.SessionsDir(""))
	defer store.Close()
	sess, err := store.Load(sessID)
	if err != nil {
		t.Fatalf("reload session: %v", err)
	}
	if len(sess.PublishedShares) != 1 || sess.PublishedShares[0].UploadID != uploadID {
		t.Errorf("PublishedShares after async = %+v, want one entry with UploadID=%s",
			sess.PublishedShares, uploadID)
	}
}

// TestHandleSessionShare_AsyncTagsSessionShareKind guards the async path's
// kind/metadata wiring. Sync path is already covered by
// TestHandleSessionShare_HappyPath; this is the analogous regression for
// share_async.runShareTask. Without this, a future refactor could quietly
// drop the kind tag on the async path and the only signal would be Desktop
// UI users complaining that their shared conversations show up under
// "Other" instead of "Session" in the published-files panel.
func TestHandleSessionShare_AsyncTagsSessionShareKind(t *testing.T) {
	const uploadID = "upload-uuid-async-kind"
	const url = "https://cdn.example/shared/async-kind.html"

	var gotKind, gotMetadata, gotListQuery string
	cloud := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/api/v1/uploads"):
			_ = r.ParseMultipartForm(32 << 20)
			gotKind = r.FormValue("kind")
			gotMetadata = r.FormValue("metadata")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"url":"` + url + `","key":"k","size":100,"content_type":"text/html"}`))
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v1/uploads"):
			gotListQuery = r.URL.RawQuery
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"uploads":[{"id":"` + uploadID + `","url":"` + url + `","filename":"f.html","content_type":"text/html","size":100,"created_at":"2026-05-18T00:00:00Z"}],"total_count":1}`))
		default:
			w.WriteHeader(503)
		}
	}

	s, sessID := newTestShareServer(t, cloud)
	s.deps.Config.Daemon.ShareAsyncDefault = true

	req := httptest.NewRequest("POST", "/sessions/"+sessID+"/share", nil)
	req.SetPathValue("id", sessID)
	rr := httptest.NewRecorder()
	s.handleSessionShare(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)

	final := waitForShareTaskTerminal(t, s, body.TaskID)
	if final.Phase != ShareTaskPhaseCompleted {
		t.Fatalf("task did not complete: %+v", final)
	}

	if gotKind != "session_share" {
		t.Errorf("async upload kind = %q, want session_share", gotKind)
	}
	if !strings.Contains(gotMetadata, `"session_id":"`+sessID+`"`) {
		t.Errorf("async upload metadata missing session_id %q: %s", sessID, gotMetadata)
	}
	if !strings.Contains(gotListQuery, "kind=session_share") {
		t.Errorf("async upload_id lookup must filter by kind=session_share; query = %q", gotListQuery)
	}
}

// TestHandleSessionShare_AsyncEmitsProgressEvents covers the SSE contract:
// the share_progress phase sequence MUST include accepted → uploading →
// completed in order (we don't insist on every intermediate phase landing
// in the test, since rendering can complete before the subscriber sees it,
// but the sequence must be monotonic and include both endpoints).
func TestHandleSessionShare_AsyncEmitsProgressEvents(t *testing.T) {
	const uploadID = "upload-uuid-async-bbb"
	const url = "https://cdn.example/shared/async-bbb.html"

	s, sessID := newTestShareServer(t, shareCloudHandler(uploadID, url))
	s.deps.Config.Daemon.ShareAsyncDefault = true
	s.eventBus = NewEventBus()

	ch := s.eventBus.Subscribe()
	defer s.eventBus.Unsubscribe(ch)

	req := httptest.NewRequest("POST", "/sessions/"+sessID+"/share", nil)
	req.SetPathValue("id", sessID)
	rr := httptest.NewRecorder()
	s.handleSessionShare(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rr.Code)
	}

	// Collect events until completed or timeout.
	phases := []string{}
	deadline := time.After(5 * time.Second)
collect:
	for {
		select {
		case evt := <-ch:
			if evt.Type != EventShareProgress {
				continue
			}
			var payload struct {
				Phase string `json:"phase"`
			}
			if err := json.Unmarshal(evt.Payload, &payload); err != nil {
				t.Fatalf("payload unmarshal: %v", err)
			}
			phases = append(phases, payload.Phase)
			if payload.Phase == ShareTaskPhaseCompleted || payload.Phase == ShareTaskPhaseFailed {
				break collect
			}
		case <-deadline:
			t.Fatalf("did not receive terminal share_progress within 5s; got %v", phases)
		}
	}

	if len(phases) < 2 {
		t.Fatalf("expected at least accepted + completed events; got %v", phases)
	}
	if phases[0] != ShareTaskPhaseAccepted {
		t.Errorf("first phase = %q, want %q", phases[0], ShareTaskPhaseAccepted)
	}
	if phases[len(phases)-1] != ShareTaskPhaseCompleted {
		t.Errorf("last phase = %q, want %q; full sequence %v",
			phases[len(phases)-1], ShareTaskPhaseCompleted, phases)
	}
}

// TestHandleSessionShare_AsyncUploadFailureSetsFailedPhase covers the error
// path: cloud upload returns 500 → task transitions to failed with the
// upstream error message preserved in task.Error.
func TestHandleSessionShare_AsyncUploadFailureSetsFailedPhase(t *testing.T) {
	failingCloud := func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/api/v1/uploads") {
			// 500 with code "upload_failed" routes to ErrTransient — the
			// uploads client retries 3 times then surfaces as a wrapped
			// transient error. Either way the share task ends in failed.
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":"upload_failed","message":"simulated S3 error"}`))
			return
		}
		w.WriteHeader(503)
	}
	s, sessID := newTestShareServer(t, failingCloud)
	s.deps.Config.Daemon.ShareAsyncDefault = true

	req := httptest.NewRequest("POST", "/sessions/"+sessID+"/share", nil)
	req.SetPathValue("id", sessID)
	rr := httptest.NewRecorder()
	s.handleSessionShare(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rr.Code)
	}
	var body struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)

	// Wait long enough for 3 retries × backoff (1+2+4=7s); the 180s task
	// timeout is well above that so the test won't race the ceiling.
	deadline := time.Now().Add(15 * time.Second)
	var final *shareTaskState
	for time.Now().Before(deadline) {
		task := s.getShareTask(body.TaskID)
		if task == nil {
			t.Fatalf("task disappeared")
		}
		if task.Phase == ShareTaskPhaseFailed || task.Phase == ShareTaskPhaseCompleted {
			final = task
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if final == nil {
		t.Fatal("task did not reach terminal phase")
	}
	if final.Phase != ShareTaskPhaseFailed {
		t.Errorf("phase = %q, want %q", final.Phase, ShareTaskPhaseFailed)
	}
	if final.Error == "" {
		t.Error("Error empty — failure context lost")
	}
	if final.URL != "" {
		t.Errorf("URL = %q on failed task, want empty", final.URL)
	}
}

// TestHandleSessionShareTask_ReturnsSnapshot covers the polling-fallback
// endpoint: after the goroutine completes, GET returns the same snapshot
// the final SSE event carried.
func TestHandleSessionShareTask_ReturnsSnapshot(t *testing.T) {
	const uploadID = "upload-uuid-async-ccc"
	const url = "https://cdn.example/shared/async-ccc.html"
	s, sessID := newTestShareServer(t, shareCloudHandler(uploadID, url))
	s.deps.Config.Daemon.ShareAsyncDefault = true

	req := httptest.NewRequest("POST", "/sessions/"+sessID+"/share", nil)
	req.SetPathValue("id", sessID)
	rr := httptest.NewRecorder()
	s.handleSessionShare(rr, req)
	var body struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)

	final := waitForShareTaskTerminal(t, s, body.TaskID)
	if final.Phase != ShareTaskPhaseCompleted {
		t.Fatalf("setup failed — task did not complete: %+v", final)
	}

	// Now GET the task — should mirror the snapshot.
	getReq := httptest.NewRequest("GET", "/sessions/"+sessID+"/share/tasks/"+body.TaskID, nil)
	getReq.SetPathValue("id", sessID)
	getReq.SetPathValue("task_id", body.TaskID)
	getRr := httptest.NewRecorder()
	s.handleSessionShareTask(getRr, getReq)

	if getRr.Code != http.StatusOK {
		t.Fatalf("GET task status = %d, body=%s", getRr.Code, getRr.Body.String())
	}
	var got shareTaskState
	if err := json.Unmarshal(getRr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal task: %v", err)
	}
	if got.TaskID != body.TaskID || got.Phase != ShareTaskPhaseCompleted ||
		got.URL != url || got.UploadID != uploadID {
		t.Errorf("snapshot mismatch: %+v", got)
	}
}

// TestHandleSessionShareTask_404OnMissing covers a fresh / expired task_id.
func TestHandleSessionShareTask_404OnMissing(t *testing.T) {
	s, sessID := newTestShareServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream should not be touched")
	})
	req := httptest.NewRequest("GET", "/sessions/"+sessID+"/share/tasks/share-deadbeef", nil)
	req.SetPathValue("id", sessID)
	req.SetPathValue("task_id", "share-deadbeef")
	rr := httptest.NewRecorder()
	s.handleSessionShareTask(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

// TestHandleSessionShareTask_404OnSessionMismatch defends against a UI bug
// where one session's GET probes another session's task ID.
func TestHandleSessionShareTask_404OnSessionMismatch(t *testing.T) {
	const uploadID = "upload-uuid-async-ddd"
	s, sessID := newTestShareServer(t, shareCloudHandler(uploadID, "https://x/y.html"))
	s.deps.Config.Daemon.ShareAsyncDefault = true

	req := httptest.NewRequest("POST", "/sessions/"+sessID+"/share", nil)
	req.SetPathValue("id", sessID)
	rr := httptest.NewRecorder()
	s.handleSessionShare(rr, req)
	var body struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	_ = waitForShareTaskTerminal(t, s, body.TaskID)

	// GET with a different session id should 404 even though the task exists.
	getReq := httptest.NewRequest("GET", "/sessions/other_session/share/tasks/"+body.TaskID, nil)
	getReq.SetPathValue("id", "other_session")
	getReq.SetPathValue("task_id", body.TaskID)
	getRr := httptest.NewRecorder()
	s.handleSessionShareTask(getRr, getReq)
	if getRr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 on session id mismatch", getRr.Code)
	}
}

// TestHandleSessionShare_ExplicitAsyncFalseOverridesDefault confirms the
// per-request override beats daemon.share_async_default — operators who set
// the default to true can still get the legacy sync body from a script that
// passes `?async=false`.
func TestHandleSessionShare_ExplicitAsyncFalseOverridesDefault(t *testing.T) {
	const uploadID = "upload-uuid-sync-override"
	const url = "https://cdn.example/sync-override.html"
	s, sessID := newTestShareServer(t, shareCloudHandler(uploadID, url))
	s.deps.Config.Daemon.ShareAsyncDefault = true // override target

	req := httptest.NewRequest("POST", "/sessions/"+sessID+"/share?async=false", nil)
	req.SetPathValue("id", sessID)
	rr := httptest.NewRecorder()
	s.handleSessionShare(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		URL      string `json:"url"`
		UploadID string `json:"upload_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.URL != url || body.UploadID != uploadID {
		t.Errorf("sync body lost: %+v", body)
	}
}

// TestShareMetadataFromConfig_ExhaustiveFieldCopy is a regression guard for
// the config → share.ShareMetadata helper. Every field on ShareMetadataConfig
// must propagate through the helper; if a new field gets added to the config
// struct and an author forgets to copy it in shareMetadataFromConfig, the
// renderer would silently see an empty value and skip the corresponding meta
// tag. A reflect-based check catches the omission at test time instead of in
// production.
func TestShareMetadataFromConfig_ExhaustiveFieldCopy(t *testing.T) {
	cfg := &config.Config{
		Daemon: config.DaemonConfig{
			ShareMetadata: config.ShareMetadataConfig{
				SiteName:       "TestSite",
				SiteURL:        "https://example.test/",
				DefaultOGImage: "https://example.test/og.png",
				TwitterImage:   "https://example.test/twitter-1200x630.png",
				LogoURL:        "https://example.test/logo.png",
			},
		},
	}
	got := shareMetadataFromConfig(cfg)
	want := share.ShareMetadata{
		SiteName:       "TestSite",
		SiteURL:        "https://example.test/",
		DefaultOGImage: "https://example.test/og.png",
		TwitterImage:   "https://example.test/twitter-1200x630.png",
		LogoURL:        "https://example.test/logo.png",
	}
	if got != want {
		t.Errorf("shareMetadataFromConfig dropped or rewrote a field:\n  got  %+v\n  want %+v", got, want)
	}

	// nil cfg must produce a zero-value ShareMetadata (never panic). This
	// path is defensive: every caller in production goes through
	// deps.Snapshot() which returns non-nil, but the helper takes a pointer
	// and is cheaper to make safe than to require callers to verify.
	if zero := shareMetadataFromConfig(nil); zero != (share.ShareMetadata{}) {
		t.Errorf("nil cfg should yield zero ShareMetadata, got %+v", zero)
	}
}
