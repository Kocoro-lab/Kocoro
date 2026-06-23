package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/skills"
)

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
