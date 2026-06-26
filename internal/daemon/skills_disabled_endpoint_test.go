package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
)

// TestSkillsDisabledEndpoint_AddRemoveRoute exercises POST/DELETE
// /skills/disabled through the full Handler() router. The DELETE assertion is
// load-bearing: DELETE /skills/{name} (handleDeleteGlobalSkill) also matches
// /skills/disabled, so this verifies the literal segment outranks the wildcard
// in Go 1.22 ServeMux and our handler — not skill deletion — runs.
func TestSkillsDisabledEndpoint_AddRemoveRoute(t *testing.T) {
	shannonDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := &ServerDeps{ShannonDir: shannonDir, AgentsDir: t.TempDir(), Config: &config.Config{}}
	srv := NewServer(0, nil, deps, "test")
	h := srv.Handler()

	send := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rec, req)
		return rec
	}

	// Add
	rec := send(http.MethodPost, "/skills/disabled", `{"skill":"pdf-reader"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /skills/disabled = %d, body=%s", rec.Code, rec.Body.String())
	}
	data, _ := os.ReadFile(filepath.Join(shannonDir, "config.yaml"))
	if !strings.Contains(string(data), "pdf-reader") {
		t.Fatalf("config.yaml missing pdf-reader:\n%s", data)
	}
	if !slices.Contains(deps.Config.Skills.Disabled, "pdf-reader") {
		t.Fatalf("in-memory Config.Skills.Disabled missing pdf-reader: %v", deps.Config.Skills.Disabled)
	}

	// Delete — must hit our handler, not handleDeleteGlobalSkill.
	rec = send(http.MethodDelete, "/skills/disabled", `{"skill":"pdf-reader"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /skills/disabled = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "removed") {
		t.Fatalf("DELETE /skills/disabled routed to wrong handler? body=%s", rec.Body.String())
	}
	if slices.Contains(deps.Config.Skills.Disabled, "pdf-reader") {
		t.Fatalf("in-memory list still has pdf-reader after delete: %v", deps.Config.Skills.Disabled)
	}

	// Empty skill → 400
	rec = send(http.MethodPost, "/skills/disabled", `{"skill":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty skill = %d, want 400", rec.Code)
	}
}

// TestHandleListSkills_DefaultAgentDisabledAnnotation asserts GET /skills marks
// each skill's default_agent_disabled flag from config.skills.disabled so the
// Desktop default-agent UI can seed its toggles.
func TestHandleListSkills_DefaultAgentDisabledAnnotation(t *testing.T) {
	shannonDir := t.TempDir()
	writeSkillFile(t, shannonDir, "pdf-reader", "---\nname: pdf-reader\ndescription: read pdfs\n---\n# Body")
	writeSkillFile(t, shannonDir, "summarize", "---\nname: summarize\ndescription: summarize text\n---\n# Body")

	deps := &ServerDeps{
		ShannonDir: shannonDir,
		AgentsDir:  t.TempDir(),
		Config:     &config.Config{Skills: config.SkillsConfig{Disabled: []string{"pdf-reader"}}},
	}
	srv := NewServer(0, nil, deps, "test")

	req := httptest.NewRequest("GET", "/skills", nil)
	rr := httptest.NewRecorder()
	srv.handleListSkills(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /skills = %d, body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Skills []skills.SkillMeta `json:"skills"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, m := range body.Skills {
		got[m.Slug] = m.DefaultAgentDisabled
	}
	if !got["pdf-reader"] {
		t.Errorf("pdf-reader should have default_agent_disabled=true, metas=%+v", body.Skills)
	}
	if got["summarize"] {
		t.Errorf("summarize should have default_agent_disabled=false")
	}
}

// TestSkillsDisabledEndpoint_PrefixBatch verifies the prefix form disables a
// whole skill family in ONE call (the longbridge batch-disable path that used
// to spin 126 times) and the batch skills[] form, without touching
// non-matching skills; DELETE prefix re-enables symmetrically.
func TestSkillsDisabledEndpoint_PrefixBatch(t *testing.T) {
	shannonDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeSkillFile(t, shannonDir, "longbridge-a", "---\nname: longbridge-a\ndescription: x\n---\n# B")
	writeSkillFile(t, shannonDir, "longbridge-b", "---\nname: longbridge-b\ndescription: x\n---\n# B")
	writeSkillFile(t, shannonDir, "other", "---\nname: other\ndescription: x\n---\n# B")

	deps := &ServerDeps{ShannonDir: shannonDir, AgentsDir: t.TempDir(), Config: &config.Config{}}
	srv := NewServer(0, nil, deps, "test")
	h := srv.Handler()
	send := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rec, req)
		return rec
	}

	// prefix disables both longbridge-*, not "other"
	rec := send(http.MethodPost, "/skills/disabled", `{"prefix":"longbridge-"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST prefix = %d, body=%s", rec.Code, rec.Body.String())
	}
	dis := deps.Config.Skills.Disabled
	if !slices.Contains(dis, "longbridge-a") || !slices.Contains(dis, "longbridge-b") {
		t.Errorf("prefix should disable both longbridge-*, got %v", dis)
	}
	if slices.Contains(dis, "other") {
		t.Errorf("prefix must not disable 'other', got %v", dis)
	}

	// DELETE prefix re-enables both
	rec = send(http.MethodDelete, "/skills/disabled", `{"prefix":"longbridge-"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE prefix = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(deps.Config.Skills.Disabled) != 0 {
		t.Errorf("prefix delete should re-enable all longbridge, got %v", deps.Config.Skills.Disabled)
	}

	// batch skills[] form
	rec = send(http.MethodPost, "/skills/disabled", `{"skills":["longbridge-a","other"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST skills[] = %d, body=%s", rec.Code, rec.Body.String())
	}
	dis = deps.Config.Skills.Disabled
	if !slices.Contains(dis, "longbridge-a") || !slices.Contains(dis, "other") {
		t.Errorf("skills[] should disable both listed, got %v", dis)
	}

	// neither skill/skills/prefix → 400
	rec = send(http.MethodPost, "/skills/disabled", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty request = %d, want 400", rec.Code)
	}
}
