package daemon

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/skills"
)

// clawhubSkillZip builds a minimal valid skill zip (one SKILL.md with
// frontmatter) for the fake ClawHub download endpoint.
func clawhubSkillZip(t *testing.T, slug string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create("SKILL.md")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	if _, err := f.Write([]byte("---\nname: " + slug + "\ndescription: installed from clawhub\n---\nbody")); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// newTestServerWithClawHub wires a Server whose ClawHub client points at a fake
// upstream that routes the /api/v1/* surface FetchClawHub* methods consume.
func newTestServerWithClawHub(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	mux := http.NewServeMux()
	// Browse: cursor-paginated list.
	mux.HandleFunc("/api/v1/skills", func(w http.ResponseWriter, r *http.Request) {
		next := "cursor-2"
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"items": []map[string]interface{}{
				{"slug": "alpha", "displayName": "Alpha", "summary": "first",
					"stats": map[string]int{"downloads": 10, "stars": 1}, "latestVersion": map[string]string{"version": "1.0.0"}},
				{"slug": "bravo", "displayName": "Bravo", "summary": "second",
					"stats": map[string]int{"downloads": 99, "stars": 5}, "latestVersion": map[string]string{"version": "2.1.0"}},
			},
			"nextCursor": next,
		})
	})
	// Detail.
	mux.HandleFunc("/api/v1/skills/alpha", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"skill": map[string]interface{}{
				"slug": "alpha", "displayName": "Alpha", "summary": "first",
				"description": "# Alpha\n\nbody", "stats": map[string]int{"downloads": 10, "stars": 1},
			},
			"owner":         map[string]string{"handle": "acme"},
			"latestVersion": map[string]string{"version": "1.0.0"},
		})
	})
	// Version manifest.
	mux.HandleFunc("/api/v1/skills/alpha/versions/1.0.0", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"version": map[string]interface{}{
				"files": []map[string]interface{}{
					{"path": "SKILL.md", "size": 12, "contentType": "text/markdown"},
					{"path": "scripts/run.py", "size": 34, "contentType": "text/x-python"},
				},
			},
		})
	})
	// File content.
	mux.HandleFunc("/api/v1/skills/alpha/file", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("path") != "SKILL.md" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("# Alpha\n\nbody"))
	})
	// Full-text search (distinct shape from the list endpoint; honors q).
	mux.HandleFunc("/api/v1/search", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"results": []map[string]interface{}{
				{"slug": "gamma", "displayName": "Gamma", "summary": "matched",
					"version": "3.0.0", "downloads": 5, "updatedAt": 100,
					"owner": map[string]string{"handle": "acme"}},
				{"slug": "delta", "displayName": "Delta", "summary": "matched too",
					"version": "1.2.0", "downloads": 50, "updatedAt": 200,
					"owner": map[string]string{"handle": "globex"}},
			},
		})
	})
	// Deterministic zip artifact download (install transport).
	mux.HandleFunc("/api/v1/download", func(w http.ResponseWriter, r *http.Request) {
		slug := r.URL.Query().Get("slug")
		if slug == "" {
			http.Error(w, "missing slug", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(clawhubSkillZip(t, slug))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	s := &Server{
		deps: &ServerDeps{
			ShannonDir: t.TempDir(),
			AgentsDir:  t.TempDir(),
		},
		clawhub:   skills.NewClawHubMarketplaceClient(srv.URL, 1*time.Hour),
		slugLocks: skills.NewSlugLocks(),
	}
	return s, srv
}

func TestHandleClawHubList(t *testing.T) {
	s, _ := newTestServerWithClawHub(t)

	req := httptest.NewRequest("GET", "/skills/clawhub?size=20&sort=downloads", nil)
	rr := httptest.NewRecorder()
	s.handleClawHubList(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Skills     []skills.MarketplaceEntry `json:"skills"`
		Size       int                       `json:"size"`
		NextCursor string                    `json:"next_cursor"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// ClawHub list shape: NO total/page, opaque next_cursor passed through.
	if body.NextCursor != "cursor-2" {
		t.Errorf("next_cursor = %q, want cursor-2", body.NextCursor)
	}
	if len(body.Skills) != 2 {
		t.Fatalf("skills len = %d, want 2", len(body.Skills))
	}
	if body.Size != 20 {
		t.Errorf("size = %d, want 20", body.Size)
	}
}

func TestHandleClawHubDetail(t *testing.T) {
	s, _ := newTestServerWithClawHub(t)

	req := httptest.NewRequest("GET", "/skills/clawhub/entry/alpha", nil)
	req.SetPathValue("slug", "alpha")
	rr := httptest.NewRecorder()
	s.handleClawHubDetail(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Slug      string `json:"slug"`
		Author    string `json:"author"`
		Homepage  string `json:"homepage"`
		Installed bool   `json:"installed"`
		Preview   string `json:"preview"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Slug != "alpha" || body.Author != "acme" {
		t.Errorf("unexpected body: %+v", body)
	}
	if body.Preview != "# Alpha\n\nbody" {
		t.Errorf("preview = %q, want SKILL.md body", body.Preview)
	}
	if body.Installed {
		t.Errorf("expected Installed=false for uninstalled skill")
	}
}

func TestHandleClawHubDetailNotFound(t *testing.T) {
	s, _ := newTestServerWithClawHub(t)

	req := httptest.NewRequest("GET", "/skills/clawhub/entry/missing", nil)
	req.SetPathValue("slug", "missing")
	rr := httptest.NewRecorder()
	s.handleClawHubDetail(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rr.Code, rr.Body.String())
	}
}

func TestHandleClawHubFiles(t *testing.T) {
	s, _ := newTestServerWithClawHub(t)

	req := httptest.NewRequest("GET", "/skills/clawhub/entry/alpha/files", nil)
	req.SetPathValue("slug", "alpha")
	rr := httptest.NewRecorder()
	s.handleClawHubFiles(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Version string               `json:"version"`
		Files   []skills.ClawHubFile `json:"files"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Version != "1.0.0" {
		t.Errorf("version = %q, want 1.0.0 (resolved from latest)", body.Version)
	}
	if len(body.Files) != 2 || body.Files[0].Path != "SKILL.md" {
		t.Errorf("unexpected files: %+v", body.Files)
	}
}

func TestHandleClawHubFile(t *testing.T) {
	s, _ := newTestServerWithClawHub(t)

	req := httptest.NewRequest("GET", "/skills/clawhub/entry/alpha/file?path=SKILL.md", nil)
	req.SetPathValue("slug", "alpha")
	rr := httptest.NewRecorder()
	s.handleClawHubFile(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Path != "SKILL.md" || body.Content != "# Alpha\n\nbody" {
		t.Errorf("unexpected body: %+v", body)
	}
}

func TestHandleClawHubFileMissingPath(t *testing.T) {
	s, _ := newTestServerWithClawHub(t)

	req := httptest.NewRequest("GET", "/skills/clawhub/entry/alpha/file", nil)
	req.SetPathValue("slug", "alpha")
	rr := httptest.NewRecorder()
	s.handleClawHubFile(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
}

// TestHandleClawHubListSearch covers the q != "" branch: ClawHub's full-text
// search endpoint is hit (distinct shape from browse), results are returned in
// one page, and next_cursor is empty (search is not cursor-paginated).
func TestHandleClawHubListSearch(t *testing.T) {
	s, _ := newTestServerWithClawHub(t)

	req := httptest.NewRequest("GET", "/skills/clawhub?q=match&sort=downloads", nil)
	rr := httptest.NewRecorder()
	s.handleClawHubList(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Skills     []skills.MarketplaceEntry `json:"skills"`
		NextCursor string                    `json:"next_cursor"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.NextCursor != "" {
		t.Errorf("next_cursor = %q, want empty for a search", body.NextCursor)
	}
	if len(body.Skills) != 2 {
		t.Fatalf("skills len = %d, want 2", len(body.Skills))
	}
	// sort=downloads re-orders the relevance-ranked pool: delta(50) before gamma(5).
	if body.Skills[0].Slug != "delta" {
		t.Errorf("re-sort by downloads wrong: got %q first, want delta", body.Skills[0].Slug)
	}
	// Search results carry the owner handle as author.
	if body.Skills[0].Author != "globex" {
		t.Errorf("author = %q, want globex", body.Skills[0].Author)
	}
}

// TestHandleClawHubInstall covers the install handler: it builds the entry from
// the deterministic download URL, installs the fetched zip, and returns 201 with
// the on-disk skill metadata.
func TestHandleClawHubInstall(t *testing.T) {
	s, _ := newTestServerWithClawHub(t)

	req := httptest.NewRequest("POST", "/skills/clawhub/install/alpha", nil)
	req.SetPathValue("slug", "alpha")
	rr := httptest.NewRecorder()
	s.handleClawHubInstall(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(filepath.Join(s.deps.ShannonDir, "skills", "alpha", "SKILL.md")); err != nil {
		t.Errorf("installed file missing: %v", err)
	}
	var meta skills.SkillMeta
	if err := json.Unmarshal(rr.Body.Bytes(), &meta); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if meta.Slug != "alpha" {
		t.Errorf("meta.Slug = %q, want alpha", meta.Slug)
	}
	if meta.InstallSource != skills.InstallSourceMarketplace {
		t.Errorf("meta.InstallSource = %q, want %q", meta.InstallSource, skills.InstallSourceMarketplace)
	}

	// Second install of the same slug must conflict (409).
	req2 := httptest.NewRequest("POST", "/skills/clawhub/install/alpha", nil)
	req2.SetPathValue("slug", "alpha")
	rr2 := httptest.NewRecorder()
	s.handleClawHubInstall(rr2, req2)
	if rr2.Code != http.StatusConflict {
		t.Errorf("second install status = %d, want 409; body = %s", rr2.Code, rr2.Body.String())
	}
}

// newTestServerWithAmbiguousClawHub wires a Server against a fake ClawHub where
// slug "ambi" is shared by two publishers: a bare (owner-less) request 409s
// (AMBIGUOUS_SKILL_SLUG), exactly as clawhub.ai does. Only owner=popular resolves
// to 200. The search endpoint (the only surface that carries owner handles)
// returns both publishers so resolveAmbiguousOwner can pick the most-downloaded.
func newTestServerWithAmbiguousClawHub(t *testing.T) *Server {
	t.Helper()
	mux := http.NewServeMux()
	// Detail: 409 without owner (ambiguous), 200 for the resolved owner.
	mux.HandleFunc("/api/v1/skills/ambi", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("owner") != "popular" {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"code": "AMBIGUOUS_SKILL_SLUG",
				"matches": []map[string]string{
					{"ownerHandle": "popular"}, {"ownerHandle": "tiny"},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"skill": map[string]interface{}{
				"slug": "ambi", "displayName": "Ambi", "summary": "shared slug",
				"description": "# Ambi\n\nbody",
			},
			"owner":         map[string]string{"handle": "popular"},
			"latestVersion": map[string]string{"version": "1.0.0"},
		})
	})
	// Search carries owner handles + downloads; popular(100) beats tiny(3).
	mux.HandleFunc("/api/v1/search", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"results": []map[string]interface{}{
				{"slug": "ambi", "downloads": 3, "owner": map[string]string{"handle": "tiny"}},
				{"slug": "ambi", "downloads": 100, "owner": map[string]string{"handle": "popular"}},
			},
		})
	})
	// Download 409s on a bare ambiguous slug; only a concrete owner serves the zip.
	mux.HandleFunc("/api/v1/download", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("owner") == "" && r.URL.Query().Get("ownerHandle") == "" {
			http.Error(w, "ambiguous", http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(clawhubSkillZip(t, "ambi"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Server{
		deps:      &ServerDeps{ShannonDir: t.TempDir(), AgentsDir: t.TempDir()},
		clawhub:   skills.NewClawHubMarketplaceClient(srv.URL, 1*time.Hour),
		slugLocks: skills.NewSlugLocks(),
	}
}

// TestHandleClawHubDetailAmbiguousSlug is the regression for the reported bug:
// a browse entry carries no owner, so an ambiguous slug 409'd upstream and the
// daemon surfaced a misleading 503 "clawhub unavailable". The daemon must now
// auto-resolve the most-popular publisher and return 200 with that author.
func TestHandleClawHubDetailAmbiguousSlug(t *testing.T) {
	s := newTestServerWithAmbiguousClawHub(t)

	req := httptest.NewRequest("GET", "/skills/clawhub/entry/ambi", nil)
	req.SetPathValue("slug", "ambi")
	rr := httptest.NewRecorder()
	s.handleClawHubDetail(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (auto-resolved); body = %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Slug    string `json:"slug"`
		Author  string `json:"author"`
		Preview string `json:"preview"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Slug != "ambi" || body.Author != "popular" {
		t.Errorf("expected resolved author=popular for ambi, got %+v", body)
	}
	if body.Preview != "# Ambi\n\nbody" {
		t.Errorf("preview = %q, want SKILL.md body", body.Preview)
	}
}

// TestHandleClawHubInstallAmbiguousSlug asserts install also auto-resolves an
// owner (browse-card installs pass no owner) so the download targets a concrete
// publisher instead of 409ing.
func TestHandleClawHubInstallAmbiguousSlug(t *testing.T) {
	s := newTestServerWithAmbiguousClawHub(t)

	req := httptest.NewRequest("POST", "/skills/clawhub/install/ambi", nil)
	req.SetPathValue("slug", "ambi")
	rr := httptest.NewRecorder()
	s.handleClawHubInstall(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(filepath.Join(s.deps.ShannonDir, "skills", "ambi", "SKILL.md")); err != nil {
		t.Errorf("installed file missing: %v", err)
	}
}

// newTestServerWithUnresolvableClawHub wires a fake ClawHub where a shared slug
// cannot be auto-resolved: search returns no owner-bearing match, and the
// download endpoint 409s on the resulting bare slug. This is the case the
// install handler must surface as an actionable 409, NOT a 502 upstream failure.
func newTestServerWithUnresolvableClawHub(t *testing.T) *Server {
	t.Helper()
	mux := http.NewServeMux()
	// Search carries no exact-slug owner match → resolveAmbiguousOwner yields "".
	mux.HandleFunc("/api/v1/search", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"results": []map[string]interface{}{}})
	})
	// Download 409s on the bare (owner-less) slug and never resolves.
	mux.HandleFunc("/api/v1/download", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "ambiguous", http.StatusConflict)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Server{
		deps:      &ServerDeps{ShannonDir: t.TempDir(), AgentsDir: t.TempDir()},
		clawhub:   skills.NewClawHubMarketplaceClient(srv.URL, 1*time.Hour),
		slugLocks: skills.NewSlugLocks(),
	}
}

// TestHandleClawHubInstallUnresolvableAmbiguousReturns409 is the regression for
// the doc-vs-behavior gap: when a shared slug can't be auto-resolved, install
// must return an actionable 409 ("retry with ?owner=") — the same shape as
// detail/files/file — rather than the misleading 502 the raw upstream-failure
// path would produce.
func TestHandleClawHubInstallUnresolvableAmbiguousReturns409(t *testing.T) {
	s := newTestServerWithUnresolvableClawHub(t)

	req := httptest.NewRequest("POST", "/skills/clawhub/install/ambi", nil)
	req.SetPathValue("slug", "ambi")
	rr := httptest.NewRecorder()
	s.handleClawHubInstall(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (actionable, not 502); body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "multiple owners") {
		t.Errorf("body = %s, want actionable 'multiple owners; retry with ?owner=' message", rr.Body.String())
	}
}
