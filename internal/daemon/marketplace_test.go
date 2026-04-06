package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
)

// daemonTestRunGit runs git inside dir for marketplace handler tests.
// Mirrors the unexported runGit helper in internal/skills/api.go.
func daemonTestRunGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd.Run()
}

// daemonTestWriteFile writes a file, creating parent dirs. Mirrors
// mustWrite from the skills package but without needing t *testing.T
// everywhere.
func daemonTestWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// newTestServerWithMarketplace constructs a minimal *Server suitable for
// exercising the marketplace handlers. It sets only the fields those
// handlers read (marketplace client, deps.ShannonDir, deps.AgentsDir).
func newTestServerWithMarketplace(t *testing.T, registryJSON string) (*Server, *httptest.Server) {
	t.Helper()
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(registryJSON))
	}))
	t.Cleanup(registry.Close)

	shannonDir := t.TempDir()
	agentsDir := t.TempDir()

	s := &Server{
		deps: &ServerDeps{
			ShannonDir: shannonDir,
			AgentsDir:  agentsDir,
		},
		marketplace: skills.NewMarketplaceClient(registry.URL, 1*time.Hour),
		slugLocks:   skills.NewSlugLocks(),
	}
	return s, registry
}

func TestHandleMarketplaceList(t *testing.T) {
	registryJSON := `{
		"version": 1,
		"skills": [
			{"slug":"alpha","name":"alpha","description":"first","author":"a","repo":"r","downloads":10},
			{"slug":"bravo","name":"bravo","description":"second","author":"b","repo":"r","downloads":100}
		]
	}`
	s, _ := newTestServerWithMarketplace(t, registryJSON)

	req := httptest.NewRequest("GET", "/skills/marketplace?page=1&size=20&sort=downloads", nil)
	rr := httptest.NewRecorder()
	s.handleMarketplaceList(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Total  int                       `json:"total"`
		Page   int                       `json:"page"`
		Size   int                       `json:"size"`
		Skills []skills.MarketplaceEntry `json:"skills"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Total != 2 {
		t.Errorf("Total = %d, want 2", body.Total)
	}
	if len(body.Skills) != 2 || body.Skills[0].Slug != "bravo" {
		t.Errorf("sort order wrong: %+v", body.Skills)
	}
}

func TestHandleMarketplaceDetail(t *testing.T) {
	registryJSON := `{
		"version":1,
		"skills":[{"slug":"demo","name":"demo","description":"d","author":"a","repo":"r","homepage":"https://example.com"}]
	}`
	s, _ := newTestServerWithMarketplace(t, registryJSON)

	req := httptest.NewRequest("GET", "/skills/marketplace/entry/demo", nil)
	req.SetPathValue("slug", "demo")
	rr := httptest.NewRecorder()
	s.handleMarketplaceDetail(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Slug      string `json:"slug"`
		Homepage  string `json:"homepage"`
		Installed bool   `json:"installed"`
		Preview   string `json:"preview"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Slug != "demo" || body.Homepage != "https://example.com" {
		t.Errorf("unexpected body: %+v", body)
	}
	if body.Installed {
		t.Errorf("expected Installed=false for uninstalled skill, got true")
	}
	if body.Preview != "" {
		t.Errorf("expected empty preview for uninstalled skill, got %q", body.Preview)
	}
}

func TestHandleMarketplaceDetailInstalledPreview(t *testing.T) {
	registryJSON := `{
		"version":1,
		"skills":[{"slug":"demo","name":"demo","description":"d","author":"a","repo":"r"}]
	}`
	s, _ := newTestServerWithMarketplace(t, registryJSON)

	// Simulate the skill already present on disk.
	skillDir := filepath.Join(s.deps.ShannonDir, "skills", "demo")
	if err := os.MkdirAll(skillDir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const skillBody = "---\nname: demo\ndescription: d\n---\nOn-disk body text."
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillBody), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	req := httptest.NewRequest("GET", "/skills/marketplace/entry/demo", nil)
	req.SetPathValue("slug", "demo")
	rr := httptest.NewRecorder()
	s.handleMarketplaceDetail(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Installed bool   `json:"installed"`
		Preview   string `json:"preview"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !body.Installed {
		t.Errorf("expected Installed=true, got false")
	}
	if body.Preview != skillBody {
		t.Errorf("preview mismatch:\n got %q\nwant %q", body.Preview, skillBody)
	}
}

func TestHandleMarketplaceDetailBlocksMalicious(t *testing.T) {
	registryJSON := `{
		"version":1,
		"skills":[{"slug":"evil","name":"evil","description":"d","author":"a","repo":"r",
			"security":{"virustotal":"malicious"}}]
	}`
	s, _ := newTestServerWithMarketplace(t, registryJSON)

	req := httptest.NewRequest("GET", "/skills/marketplace/entry/evil", nil)
	req.SetPathValue("slug", "evil")
	rr := httptest.NewRecorder()
	s.handleMarketplaceDetail(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rr.Code)
	}
}

func TestHandleMarketplaceDetailNotFound(t *testing.T) {
	s, _ := newTestServerWithMarketplace(t, `{"version":1,"skills":[]}`)

	req := httptest.NewRequest("GET", "/skills/marketplace/entry/nope", nil)
	req.SetPathValue("slug", "nope")
	rr := httptest.NewRecorder()
	s.handleMarketplaceDetail(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestHandleMarketplaceInstallSuccess(t *testing.T) {
	// Fixture git repo for file:// clone. Deliberately use a description
	// in the frontmatter that differs from the registry entry so we can
	// prove the response reflects on-disk truth, not synthesized registry
	// metadata.
	repo := t.TempDir()
	if err := daemonTestRunGit(repo, "init", "-q", "-b", "main"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	const onDiskDesc = "On-disk authoritative description"
	daemonTestWriteFile(t, filepath.Join(repo, "SKILL.md"),
		"---\nname: demo\ndescription: "+onDiskDesc+"\n---\nbody")
	daemonTestRunGit(repo, "config", "user.email", "t@e.com")
	daemonTestRunGit(repo, "config", "user.name", "t")
	daemonTestRunGit(repo, "config", "commit.gpgsign", "false")
	daemonTestRunGit(repo, "add", ".")
	daemonTestRunGit(repo, "commit", "-q", "-m", "init")

	// Registry entry intentionally carries a different description so we
	// can detect drift between response body and on-disk SKILL.md.
	registryJSON := fmt.Sprintf(`{
		"version":1,
		"skills":[{"slug":"demo","name":"demo","description":"Registry description (should NOT appear in response)","author":"a","repo":"file://%s","ref":"main"}]
	}`, repo)
	s, _ := newTestServerWithMarketplace(t, registryJSON)

	req := httptest.NewRequest("POST", "/skills/marketplace/install/demo", nil)
	req.SetPathValue("slug", "demo")
	rr := httptest.NewRecorder()
	s.handleMarketplaceInstall(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(filepath.Join(s.deps.ShannonDir, "skills", "demo", "SKILL.md")); err != nil {
		t.Errorf("installed file missing: %v", err)
	}

	// Response body must reflect on-disk truth, not the registry entry.
	var meta skills.SkillMeta
	if err := json.Unmarshal(rr.Body.Bytes(), &meta); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if meta.Name != "demo" {
		t.Errorf("meta.Name = %q, want demo", meta.Name)
	}
	if meta.Description != onDiskDesc {
		t.Errorf("meta.Description = %q, want %q (registry description leaked into response)",
			meta.Description, onDiskDesc)
	}
	if meta.InstallSource != skills.InstallSourceMarketplace {
		t.Errorf("meta.InstallSource = %q, want %q", meta.InstallSource, skills.InstallSourceMarketplace)
	}
	if meta.MarketplaceSlug != "demo" {
		t.Errorf("meta.MarketplaceSlug = %q, want demo", meta.MarketplaceSlug)
	}
}

func TestHandleMarketplaceInstallMaliciousBlocked(t *testing.T) {
	registryJSON := `{
		"version":1,
		"skills":[{"slug":"evil","name":"evil","description":"d","author":"a","repo":"file:///x","security":{"virustotal":"malicious"}}]
	}`
	s, _ := newTestServerWithMarketplace(t, registryJSON)

	req := httptest.NewRequest("POST", "/skills/marketplace/install/evil", nil)
	req.SetPathValue("slug", "evil")
	rr := httptest.NewRecorder()
	s.handleMarketplaceInstall(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rr.Code)
	}
}

// TestHandleMarketplaceInstallUpstreamFailureMaps502 verifies that a
// failed git clone surfaces as 502 Bad Gateway (upstream problem), not
// 500 Internal Server Error (local problem). Uses a registry entry with
// a bogus file:// path that definitely does not exist.
func TestHandleMarketplaceInstallUpstreamFailureMaps502(t *testing.T) {
	registryJSON := `{
		"version":1,
		"skills":[{"slug":"demo","name":"demo","description":"d","author":"a","repo":"file:///nonexistent/path/to/repo","ref":"main"}]
	}`
	s, _ := newTestServerWithMarketplace(t, registryJSON)

	req := httptest.NewRequest("POST", "/skills/marketplace/install/demo", nil)
	req.SetPathValue("slug", "demo")
	rr := httptest.NewRecorder()
	s.handleMarketplaceInstall(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (upstream clone failure)", rr.Code)
	}
}

func TestHandleMarketplaceInstallNotFound(t *testing.T) {
	s, _ := newTestServerWithMarketplace(t, `{"version":1,"skills":[]}`)

	req := httptest.NewRequest("POST", "/skills/marketplace/install/nope", nil)
	req.SetPathValue("slug", "nope")
	rr := httptest.NewRecorder()
	s.handleMarketplaceInstall(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestHandleSkillUsage(t *testing.T) {
	s, _ := newTestServerWithMarketplace(t, `{"version":1,"skills":[]}`)

	// Wire up two agents attaching the skill.
	if err := agents.SetAttachedSkills(s.deps.AgentsDir, "coder", []string{"demo"}); err != nil {
		t.Fatalf("SetAttachedSkills coder: %v", err)
	}
	if err := agents.SetAttachedSkills(s.deps.AgentsDir, "researcher", []string{"demo", "other"}); err != nil {
		t.Fatalf("SetAttachedSkills researcher: %v", err)
	}

	req := httptest.NewRequest("GET", "/skills/demo/usage", nil)
	req.SetPathValue("name", "demo")
	rr := httptest.NewRecorder()
	s.handleSkillUsage(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Skill  string   `json:"skill"`
		Agents []string `json:"agents"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Skill != "demo" {
		t.Errorf("Skill = %q", body.Skill)
	}
	want := []string{"coder", "researcher"}
	if len(body.Agents) != 2 || body.Agents[0] != want[0] || body.Agents[1] != want[1] {
		t.Errorf("Agents = %v, want %v", body.Agents, want)
	}
}

func TestHandleListSkillsIncludesMarketplaceProvenance(t *testing.T) {
	repo := t.TempDir()
	if err := daemonTestRunGit(repo, "init", "-q", "-b", "main"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	daemonTestWriteFile(t, filepath.Join(repo, "SKILL.md"), "---\nname: demo\ndescription: Demo\n---\nbody")
	daemonTestRunGit(repo, "config", "user.email", "t@e.com")
	daemonTestRunGit(repo, "config", "user.name", "t")
	daemonTestRunGit(repo, "config", "commit.gpgsign", "false")
	daemonTestRunGit(repo, "add", ".")
	daemonTestRunGit(repo, "commit", "-q", "-m", "init")

	registryJSON := fmt.Sprintf(`{
		"version":1,
		"skills":[{"slug":"demo","name":"demo","description":"Demo","author":"a","repo":"file://%s","ref":"main"}]
	}`, repo)
	s, _ := newTestServerWithMarketplace(t, registryJSON)

	req := httptest.NewRequest("POST", "/skills/marketplace/install/demo", nil)
	req.SetPathValue("slug", "demo")
	rr := httptest.NewRecorder()
	s.handleMarketplaceInstall(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("install status = %d, body = %s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest("GET", "/skills", nil)
	rr = httptest.NewRecorder()
	s.handleListSkills(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var body struct {
		Skills []skills.SkillMeta `json:"skills"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(body.Skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(body.Skills))
	}
	if body.Skills[0].InstallSource != skills.InstallSourceMarketplace {
		t.Errorf("install_source = %q, want %q", body.Skills[0].InstallSource, skills.InstallSourceMarketplace)
	}
	if body.Skills[0].MarketplaceSlug != "demo" {
		t.Errorf("marketplace_slug = %q, want demo", body.Skills[0].MarketplaceSlug)
	}
}

func TestHandleLocalSkillCollisionStaysLocal(t *testing.T) {
	repo := t.TempDir()
	if err := daemonTestRunGit(repo, "init", "-q", "-b", "main"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	daemonTestWriteFile(t, filepath.Join(repo, "SKILL.md"), "---\nname: ontology\ndescription: Marketplace ontology\n---\nbody")
	daemonTestRunGit(repo, "config", "user.email", "t@e.com")
	daemonTestRunGit(repo, "config", "user.name", "t")
	daemonTestRunGit(repo, "config", "commit.gpgsign", "false")
	daemonTestRunGit(repo, "add", ".")
	daemonTestRunGit(repo, "commit", "-q", "-m", "init")

	registryJSON := fmt.Sprintf(`{
		"version":1,
		"skills":[{"slug":"ontology","name":"ontology","description":"Registry ontology","author":"a","repo":"file://%s","ref":"main"}]
	}`, repo)
	s, _ := newTestServerWithMarketplace(t, registryJSON)

	req := httptest.NewRequest("POST", "/skills/marketplace/install/ontology", nil)
	req.SetPathValue("slug", "ontology")
	rr := httptest.NewRecorder()
	s.handleMarketplaceInstall(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("install status = %d, body = %s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest("DELETE", "/skills/ontology", nil)
	req.SetPathValue("name", "ontology")
	rr = httptest.NewRecorder()
	s.handleDeleteGlobalSkill(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body = %s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest("PUT", "/skills/ontology", strings.NewReader(`{"description":"Local fake","prompt":"---\nname: ontology\ndescription: Local fake\n---\n# body"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "ontology")
	rr = httptest.NewRecorder()
	s.handlePutGlobalSkill(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("put status = %d, body = %s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest("GET", "/skills", nil)
	rr = httptest.NewRecorder()
	s.handleListSkills(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var body struct {
		Skills []skills.SkillMeta `json:"skills"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(body.Skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(body.Skills))
	}
	if body.Skills[0].InstallSource != skills.InstallSourceLocal {
		t.Errorf("install_source = %q, want %q", body.Skills[0].InstallSource, skills.InstallSourceLocal)
	}
	if body.Skills[0].MarketplaceSlug != "" {
		t.Errorf("marketplace_slug = %q, want empty", body.Skills[0].MarketplaceSlug)
	}
}

// TestE2E_RealClawHubOntology installs the real ontology skill from
// ClawHub's Convex-hosted zip, exercising the full marketplace handler
// stack end-to-end with a real HTTP upstream.
//
// Gated by SHANNON_E2E_ONTOLOGY=1 so normal test runs don't hit the
// network. Run with:
//
//	SHANNON_E2E_ONTOLOGY=1 go test ./internal/daemon/ -run TestE2E_RealClawHubOntology -v
//
// Assertions:
//   - Install returns 201
//   - Response body reflects on-disk SKILL.md (frontmatter-derived name)
//   - SKILL.md, scripts/ontology.py, references/queries.md, references/schema.md all land
//   - No .git/.github metadata leaks in
//   - Detail endpoint returns installed=true + preview containing the frontmatter
//   - Re-install returns 409 with ErrSkillAlreadyInstalled
//   - Usage endpoint returns empty agents array
func TestE2E_RealClawHubOntology(t *testing.T) {
	if os.Getenv("SHANNON_E2E_ONTOLOGY") != "1" {
		t.Skip("set SHANNON_E2E_ONTOLOGY=1 to run the live ClawHub ontology E2E test")
	}

	// Build a local registry that points at the real ClawHub download URL.
	const ontologyZipURL = "https://wry-manatee-359.convex.site/api/v1/download?slug=ontology"
	registryJSON := fmt.Sprintf(`{
		"version": 1,
		"skills": [
			{
				"slug": "ontology",
				"name": "ontology",
				"description": "Typed knowledge graph for structured agent memory",
				"author": "oswalpalash",
				"license": "MIT-0",
				"download_url": "%s",
				"homepage": "https://clawhub.ai/oswalpalash/ontology",
				"downloads": 152759,
				"stars": 484,
				"version": "1.0.4"
			}
		]
	}`, ontologyZipURL)

	s, _ := newTestServerWithMarketplace(t, registryJSON)

	// 1. Install
	req := httptest.NewRequest("POST", "/skills/marketplace/install/ontology", nil)
	req.SetPathValue("slug", "ontology")
	rr := httptest.NewRecorder()
	s.handleMarketplaceInstall(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("install: status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var meta skills.SkillMeta
	if err := json.Unmarshal(rr.Body.Bytes(), &meta); err != nil {
		t.Fatalf("unmarshal install response: %v", err)
	}
	if meta.Name != "ontology" {
		t.Errorf("install response Name = %q, want ontology", meta.Name)
	}
	if meta.Description == "" {
		t.Error("install response Description is empty — expected on-disk SKILL.md description")
	}
	t.Logf("install response: name=%q description=%q source=%q",
		meta.Name, meta.Description, meta.Source)

	// 2. Verify installed tree
	installedRoot := filepath.Join(s.deps.ShannonDir, "skills", "ontology")
	expectedFiles := []string{
		"SKILL.md",
		"scripts/ontology.py",
		"references/queries.md",
		"references/schema.md",
	}
	for _, rel := range expectedFiles {
		p := filepath.Join(installedRoot, rel)
		info, err := os.Stat(p)
		if err != nil {
			t.Errorf("expected file missing: %s — %v", rel, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("expected file empty: %s", rel)
		}
		t.Logf("  ✓ %s (%d bytes)", rel, info.Size())
	}

	// No git metadata leaked in.
	for _, forbidden := range []string{".git", ".github", ".gitignore", ".gitattributes"} {
		if _, err := os.Stat(filepath.Join(installedRoot, forbidden)); !os.IsNotExist(err) {
			t.Errorf("git metadata leaked in: %s", forbidden)
		}
	}

	// 3. Detail endpoint now shows installed=true and a preview.
	req = httptest.NewRequest("GET", "/skills/marketplace/entry/ontology", nil)
	req.SetPathValue("slug", "ontology")
	rr = httptest.NewRecorder()
	s.handleMarketplaceDetail(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("detail: status = %d", rr.Code)
	}
	var detail struct {
		Slug      string `json:"slug"`
		Installed bool   `json:"installed"`
		Preview   string `json:"preview"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &detail); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}
	if !detail.Installed {
		t.Error("detail: Installed should be true after successful install")
	}
	if !strings.Contains(detail.Preview, "name: ontology") {
		t.Errorf("detail Preview should contain frontmatter 'name: ontology', got first 200 chars: %q",
			firstN(detail.Preview, 200))
	}

	// 4. Re-install → 409
	req = httptest.NewRequest("POST", "/skills/marketplace/install/ontology", nil)
	req.SetPathValue("slug", "ontology")
	rr = httptest.NewRecorder()
	s.handleMarketplaceInstall(rr, req)
	if rr.Code != http.StatusConflict {
		t.Errorf("re-install: status = %d, want 409", rr.Code)
	}

	// 5. Usage endpoint → empty agents (nothing attached)
	req = httptest.NewRequest("GET", "/skills/ontology/usage", nil)
	req.SetPathValue("name", "ontology")
	rr = httptest.NewRecorder()
	s.handleSkillUsage(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("usage: status = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"agents":[]`) {
		t.Errorf("usage body should have empty agents array, got: %s", rr.Body.String())
	}
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// TestHandleMarketplaceNilDepsReturns500 verifies the nil-deps guard
// in every marketplace handler. Matches the nil-safety contract of
// resolveRegistryURL in the constructor: tests that build Server with
// nil deps must not panic on handler invocation.
func TestHandleMarketplaceNilDepsReturns500(t *testing.T) {
	s := &Server{
		deps:        nil,
		marketplace: skills.NewMarketplaceClient("http://127.0.0.1:1/unused", 1*time.Hour),
		slugLocks:   skills.NewSlugLocks(),
	}

	cases := []struct {
		name string
		fn   func(http.ResponseWriter, *http.Request)
		path string
		slug string // "" if no path var
	}{
		{"list", s.handleMarketplaceList, "/skills/marketplace", ""},
		{"detail", s.handleMarketplaceDetail, "/skills/marketplace/entry/demo", "demo"},
		{"install", s.handleMarketplaceInstall, "/skills/marketplace/install/demo", "demo"},
		{"usage", s.handleSkillUsage, "/skills/demo/usage", ""}, // usage uses name, not slug
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tc.path, nil)
			if tc.slug != "" {
				req.SetPathValue("slug", tc.slug)
			}
			if tc.name == "usage" {
				req.SetPathValue("name", "demo")
			}
			rr := httptest.NewRecorder()
			// Must not panic. Must return 500.
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("%s handler panicked with nil deps: %v", tc.name, p)
				}
			}()
			tc.fn(rr, req)
			if rr.Code != http.StatusInternalServerError {
				t.Errorf("%s: status = %d, want 500", tc.name, rr.Code)
			}
		})
	}
}

// TestHandleMarketplaceDetailPreviewFieldAlwaysPresent verifies the
// preview field is present in the JSON schema regardless of install
// state. Without this, uninstalled skills would have no preview key,
// breaking Desktop clients that depend on the field's existence.
func TestHandleMarketplaceDetailPreviewFieldAlwaysPresent(t *testing.T) {
	s, _ := newTestServerWithMarketplace(t, `{"version":1,"skills":[{"slug":"demo","name":"demo","description":"d","author":"a","repo":"r"}]}`)

	req := httptest.NewRequest("GET", "/skills/marketplace/entry/demo", nil)
	req.SetPathValue("slug", "demo")
	rr := httptest.NewRecorder()
	s.handleMarketplaceDetail(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}

	// Unmarshal into a map so we can distinguish "missing key" from
	// "empty string". A struct would silently zero-value either case.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := raw["preview"]; !ok {
		t.Errorf("preview field should be present in JSON schema for uninstalled skills, got keys: %v", mapKeys(raw))
	}
	if _, ok := raw["installed"]; !ok {
		t.Errorf("installed field should be present in JSON schema, got keys: %v", mapKeys(raw))
	}
}

func mapKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestHandleMarketplaceInstallGitSymlinkMaps422 verifies that a git
// payload containing a symlink surfaces as 422 (invalid payload), not
// 500. Pre-fix, stageCleanPayload returned a bare error that fell
// through to the default handler branch and got classified as 500.
// The fix wraps the error with ErrInvalidSkillPayload in installFromGit.
func TestHandleMarketplaceInstallGitSymlinkMaps422(t *testing.T) {
	// Fixture git repo with a symlink.
	repo := t.TempDir()
	if err := daemonTestRunGit(repo, "init", "-q", "-b", "main"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	daemonTestWriteFile(t, filepath.Join(repo, "SKILL.md"), "---\nname: demo\ndescription: d\n---\n")
	// Create a symlink alongside SKILL.md.
	if err := os.Symlink("/etc/passwd", filepath.Join(repo, "evil")); err != nil {
		t.Skipf("symlink unsupported on this filesystem: %v", err)
	}
	daemonTestRunGit(repo, "config", "user.email", "t@e.com")
	daemonTestRunGit(repo, "config", "user.name", "t")
	daemonTestRunGit(repo, "config", "commit.gpgsign", "false")
	daemonTestRunGit(repo, "add", ".")
	daemonTestRunGit(repo, "commit", "-q", "-m", "init")

	registryJSON := fmt.Sprintf(`{
		"version":1,
		"skills":[{"slug":"demo","name":"demo","description":"d","author":"a","repo":"file://%s","ref":"main"}]
	}`, repo)
	s, _ := newTestServerWithMarketplace(t, registryJSON)

	req := httptest.NewRequest("POST", "/skills/marketplace/install/demo", nil)
	req.SetPathValue("slug", "demo")
	rr := httptest.NewRecorder()
	s.handleMarketplaceInstall(rr, req)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 (symlink in git payload → invalid payload), body = %s", rr.Code, rr.Body.String())
	}
}

func TestHandleSkillUsageEmpty(t *testing.T) {
	s, _ := newTestServerWithMarketplace(t, `{"version":1,"skills":[]}`)

	req := httptest.NewRequest("GET", "/skills/unused/usage", nil)
	req.SetPathValue("name", "unused")
	rr := httptest.NewRecorder()
	s.handleSkillUsage(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"agents":[]`) {
		t.Errorf("expected empty agents array in body, got: %s", rr.Body.String())
	}
}

func TestHandleMarketplaceListFiltersMalicious(t *testing.T) {
	registryJSON := `{
		"version":1,
		"skills":[
			{"slug":"good","name":"good","description":"d","author":"a","repo":"r"},
			{"slug":"evil","name":"evil","description":"d","author":"a","repo":"r",
			 "security":{"virustotal":"malicious"}}
		]
	}`
	s, _ := newTestServerWithMarketplace(t, registryJSON)

	req := httptest.NewRequest("GET", "/skills/marketplace", nil)
	rr := httptest.NewRecorder()
	s.handleMarketplaceList(rr, req)

	if !strings.Contains(rr.Body.String(), `"good"`) || strings.Contains(rr.Body.String(), `"evil"`) {
		t.Errorf("malicious entry should be excluded: %s", rr.Body.String())
	}
}
