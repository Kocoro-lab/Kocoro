package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/migrate/claudecode"
)

func TestClaudeMigratePreviewApply(t *testing.T) {
	sourceHome, sourcePath := writeClaudeMigrateFixture(t)
	target := t.TempDir()
	srv := NewServer(0, nil, &ServerDeps{ShannonDir: target}, "test")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	previewBody := fmt.Sprintf(`{"source_path":%q}`, sourcePath)
	resp, err := http.Post(ts.URL+"/migrate/claude-code/preview", "application/json", strings.NewReader(previewBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preview status=%d", resp.StatusCode)
	}
	raw := new(bytes.Buffer)
	if _, err := raw.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw.String(), sourceHome) {
		t.Fatalf("preview leaked absolute source path: %s", raw.String())
	}
	if strings.Contains(raw.String(), "sk-SECRET-SHOULD-NOT-LEAK") {
		t.Fatalf("preview leaked MCP secret: %s", raw.String())
	}
	var preview struct {
		PlanID  string `json:"plan_id"`
		Summary struct {
			Skills struct {
				ToImport int `json:"to_import"`
			} `json:"skills"`
			MCPServers struct {
				ToImport       int      `json:"to_import"`
				MissingEnvKeys []string `json:"missing_env_keys"`
			} `json:"mcp_servers"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(raw.Bytes(), &preview); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if preview.PlanID == "" {
		t.Fatal("preview missing plan_id")
	}
	if preview.Summary.Skills.ToImport != 1 || preview.Summary.MCPServers.ToImport != 1 {
		t.Fatalf("unexpected preview summary: %+v", preview.Summary)
	}
	if len(preview.Summary.MCPServers.MissingEnvKeys) != 1 || preview.Summary.MCPServers.MissingEnvKeys[0] != "TEST_API_KEY" {
		t.Fatalf("missing env key summary wrong: %+v", preview.Summary.MCPServers.MissingEnvKeys)
	}

	applyBody := fmt.Sprintf(`{"plan_id":%q}`, preview.PlanID)
	applyResp, err := http.Post(ts.URL+"/migrate/claude-code/apply", "application/json", strings.NewReader(applyBody))
	if err != nil {
		t.Fatal(err)
	}
	defer applyResp.Body.Close()
	if applyResp.StatusCode != http.StatusOK {
		t.Fatalf("apply status=%d", applyResp.StatusCode)
	}
	var apply struct {
		Result     string                    `json:"result"`
		Imported   map[string]map[string]any `json:"imported"`
		ManifestID string                    `json:"manifest_id"`
	}
	if err := json.NewDecoder(applyResp.Body).Decode(&apply); err != nil {
		t.Fatalf("decode apply: %v", err)
	}
	if apply.Result != "applied" || int(apply.Imported["skills"]["completed"].(float64)) != 1 || int(apply.Imported["mcp_servers"]["completed"].(float64)) != 1 {
		t.Fatalf("unexpected apply response: %+v", apply)
	}
	if apply.ManifestID == "" {
		t.Fatal("apply missing manifest_id")
	}
	if _, err := os.Stat(filepath.Join(target, "skills", "daily", "SKILL.md")); err != nil {
		t.Fatalf("skill not imported: %v", err)
	}
	cfg, err := os.ReadFile(filepath.Join(target, "config.yaml"))
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}
	if !strings.Contains(string(cfg), "TEST_API_KEY") || strings.Contains(string(cfg), "sk-SECRET-SHOULD-NOT-LEAK") {
		t.Fatalf("config secret handling wrong:\n%s", cfg)
	}
}

func TestClaudeMigratePreviewNotFound(t *testing.T) {
	home := t.TempDir()
	srv := NewServer(0, nil, &ServerDeps{ShannonDir: t.TempDir()}, "test")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := fmt.Sprintf(`{"source_path":%q}`, filepath.Join(home, ".claude"))
	resp, err := http.Post(ts.URL+"/migrate/claude-code/preview", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["error"] != "claude_not_found" {
		t.Fatalf("error=%v, want claude_not_found", got["error"])
	}
}

func TestClaudeMigrateApplyStalePlan(t *testing.T) {
	_, sourcePath := writeClaudeMigrateFixture(t)
	target := t.TempDir()
	srv := NewServer(0, nil, &ServerDeps{ShannonDir: target}, "test")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := fmt.Sprintf(`{"source_path":%q}`, sourcePath)
	resp, err := http.Post(ts.URL+"/migrate/claude-code/preview", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var preview struct {
		PlanID string `json:"plan_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&preview); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(sourcePath, "skills", "daily.md"), []byte("mutated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	applyResp, err := http.Post(ts.URL+"/migrate/claude-code/apply", "application/json", strings.NewReader(fmt.Sprintf(`{"plan_id":%q}`, preview.PlanID)))
	if err != nil {
		t.Fatal(err)
	}
	defer applyResp.Body.Close()
	if applyResp.StatusCode != http.StatusConflict {
		t.Fatalf("status=%d, want 409", applyResp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(applyResp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["error"] != "plan_stale" {
		t.Fatalf("error=%v, want plan_stale", got["error"])
	}
	if _, err := os.Stat(filepath.Join(target, "skills", "daily")); err == nil {
		t.Fatal("stale apply wrote target")
	}
}

func TestClaudeMigrateApplyExpiredPlan(t *testing.T) {
	target := t.TempDir()
	srv := NewServer(0, nil, &ServerDeps{ShannonDir: target}, "test")
	srv.migratePlans.Put(&claudecode.Plan{ID: "mig-expired", ExpiresAt: time.Now().Add(-time.Second)})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/migrate/claude-code/apply", "application/json", strings.NewReader(`{"plan_id":"mig-expired"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("status=%d, want 410", resp.StatusCode)
	}
}

func TestClaudeMigrateRecoverOrphansStartupHook(t *testing.T) {
	target := t.TempDir()
	staging := filepath.Join(target, "skills", "left.staging-test")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	p := &claudecode.Plan{ID: "mig-recover", CreatedAt: time.Now()}
	if err := claudecode.WriteIntentManifest(target, p, []string{staging}); err != nil {
		t.Fatal(err)
	}

	srv := NewServer(0, nil, &ServerDeps{ShannonDir: target}, "test")
	srv.recoverMigrationOrphans()
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("staging should be removed, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(target, ".migrate-manifests", "mig-recover.orphan.json")); err != nil {
		t.Fatalf("orphan manifest missing: %v", err)
	}
}

func writeClaudeMigrateFixture(t *testing.T) (home, sourcePath string) {
	t.Helper()
	home = t.TempDir()
	sourcePath = filepath.Join(home, ".claude")
	if err := os.MkdirAll(filepath.Join(sourcePath, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourcePath, "skills", "daily.md"), []byte("---\nname: daily\ndescription: Daily\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	claudeJSON := `{
  "mcpServers": {
    "test": {
      "command": "node",
      "args": ["server.js"],
      "env": {"TEST_API_KEY": "sk-SECRET-SHOULD-NOT-LEAK"}
    }
  }
}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(claudeJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	return home, sourcePath
}
