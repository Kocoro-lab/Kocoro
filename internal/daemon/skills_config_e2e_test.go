package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

func TestE2E_SkillAndConfigConsistencyHTTP(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/tools", "/api/v1/integrations/tools":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[]"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer gateway.Close()

	shannonDir := t.TempDir()
	agentsDir := filepath.Join(shannonDir, "agents")
	configPath := filepath.Join(shannonDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("agent:\n  temperature: 0.2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(agentsDir, "analyst"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "analyst", "AGENT.md"), []byte("analyst"), 0600); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(shannonDir, "skills", "docker")
	if err := os.MkdirAll(skillDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: Docker\ndescription: containers\n---\nbody\n"),
		0600,
	); err != nil {
		t.Fatal(err)
	}
	if err := agents.SetAttachedSkills(agentsDir, "analyst", []string{"Docker"}); err != nil {
		t.Fatal(err)
	}

	initialRevision, err := config.FileRevision(shannonDir)
	if err != nil {
		t.Fatal(err)
	}
	deps := &ServerDeps{
		Config:         &config.Config{Endpoint: gateway.URL, Agent: config.AgentConfig{Temperature: 0.2}},
		ConfigRevision: initialRevision,
		GW:             client.NewGatewayClient(gateway.URL, ""),
		ShannonDir:     shannonDir,
		AgentsDir:      agentsDir,
		SessionCache:   NewSessionCache(filepath.Join(shannonDir, "sessions")),
	}
	t.Cleanup(deps.SessionCache.CloseAll)
	srv := NewServer(0, nil, deps, "test")
	api := httptest.NewServer(srv.Handler())
	defer api.Close()

	deleteReq, err := http.NewRequest(http.MethodDelete, api.URL+"/agents/analyst/skills/docker", nil)
	if err != nil {
		t.Fatal(err)
	}
	deleteResp, err := http.DefaultClient.Do(deleteReq)
	if err != nil {
		t.Fatal(err)
	}
	var deleteBody map[string]any
	if err := json.NewDecoder(deleteResp.Body).Decode(&deleteBody); err != nil {
		deleteResp.Body.Close()
		t.Fatal(err)
	}
	deleteResp.Body.Close()
	if deleteResp.StatusCode != http.StatusOK || deleteBody["status"] != "deleted" {
		t.Fatalf("agent skill delete = %d %#v", deleteResp.StatusCode, deleteBody)
	}
	if _, err := os.Stat(filepath.Join(agentsDir, "analyst", "_attached.yaml")); !os.IsNotExist(err) {
		t.Fatalf("legacy attachment survived HTTP delete: %v", err)
	}

	brokenSkillDir := filepath.Join(shannonDir, "skills", "broken")
	if err := os.MkdirAll(brokenSkillDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brokenSkillDir, "SKILL.md"), []byte("no frontmatter"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := agents.SetAttachedSkills(agentsDir, "analyst", []string{"broken"}); err != nil {
		t.Fatal(err)
	}
	brokenDeleteReq, err := http.NewRequest(http.MethodDelete, api.URL+"/agents/analyst/skills/broken", nil)
	if err != nil {
		t.Fatal(err)
	}
	brokenDeleteResp, err := http.DefaultClient.Do(brokenDeleteReq)
	if err != nil {
		t.Fatal(err)
	}
	var brokenDeleteBody map[string]any
	if err := json.NewDecoder(brokenDeleteResp.Body).Decode(&brokenDeleteBody); err != nil {
		brokenDeleteResp.Body.Close()
		t.Fatal(err)
	}
	brokenDeleteResp.Body.Close()
	if brokenDeleteResp.StatusCode != http.StatusOK || brokenDeleteBody["status"] != "deleted" {
		t.Fatalf("malformed skill per-agent delete = %d %#v", brokenDeleteResp.StatusCode, brokenDeleteBody)
	}
	if _, err := os.Stat(filepath.Join(agentsDir, "analyst", "_attached.yaml")); !os.IsNotExist(err) {
		t.Fatalf("exact slug attachment survived malformed skill delete: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(agentsDir, "scout"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "scout", "AGENT.md"), []byte("scout"), 0600); err != nil {
		t.Fatal(err)
	}
	globalSkillDir := filepath.Join(shannonDir, "skills", "research-tools")
	if err := os.MkdirAll(globalSkillDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(globalSkillDir, "SKILL.md"),
		[]byte("---\nname: Research Tools\ndescription: research\n---\nbody\n"),
		0600,
	); err != nil {
		t.Fatal(err)
	}
	if err := agents.SetAttachedSkills(agentsDir, "analyst", []string{"research-tools"}); err != nil {
		t.Fatal(err)
	}
	if err := agents.SetAttachedSkills(agentsDir, "scout", []string{"Research Tools"}); err != nil {
		t.Fatal(err)
	}

	globalDeleteReq, err := http.NewRequest(http.MethodDelete, api.URL+"/skills/research-tools?confirm=true", nil)
	if err != nil {
		t.Fatal(err)
	}
	globalDeleteResp, err := http.DefaultClient.Do(globalDeleteReq)
	if err != nil {
		t.Fatal(err)
	}
	var globalDeleteBody map[string]any
	if err := json.NewDecoder(globalDeleteResp.Body).Decode(&globalDeleteBody); err != nil {
		globalDeleteResp.Body.Close()
		t.Fatal(err)
	}
	globalDeleteResp.Body.Close()
	if globalDeleteResp.StatusCode != http.StatusOK || globalDeleteBody["status"] != "deleted" {
		t.Fatalf("global skill delete = %d %#v", globalDeleteResp.StatusCode, globalDeleteBody)
	}
	if _, err := os.Stat(globalSkillDir); !os.IsNotExist(err) {
		t.Fatalf("global skill directory survived HTTP delete: %v", err)
	}
	for _, agentName := range []string{"analyst", "scout"} {
		if _, err := os.Stat(filepath.Join(agentsDir, agentName, "_attached.yaml")); !os.IsNotExist(err) {
			t.Fatalf("%s attachment survived global HTTP delete: %v", agentName, err)
		}
	}

	builtinReq, err := http.NewRequest(http.MethodDelete, api.URL+"/skills/kocoro?confirm=true", nil)
	if err != nil {
		t.Fatal(err)
	}
	builtinResp, err := http.DefaultClient.Do(builtinReq)
	if err != nil {
		t.Fatal(err)
	}
	var builtinBody map[string]any
	if err := json.NewDecoder(builtinResp.Body).Decode(&builtinBody); err != nil {
		builtinResp.Body.Close()
		t.Fatal(err)
	}
	builtinResp.Body.Close()
	if builtinResp.StatusCode != http.StatusForbidden || builtinBody["error"] != "skill_is_builtin" {
		t.Fatalf("builtin delete = %d %#v", builtinResp.StatusCode, builtinBody)
	}

	if err := os.WriteFile(configPath, []byte("agent:\n  temperature: 0.8\n"), 0600); err != nil {
		t.Fatal(err)
	}
	status := getConfigStatusE2E(t, api.URL)
	if status["reload_required"] != true {
		t.Fatalf("external edit status = %#v", status)
	}

	srv.loadConfigWithRevision = func() (*config.Config, string, error) {
		loadedRevision, err := config.FileRevision(shannonDir)
		if err != nil {
			return nil, "", err
		}
		if err := os.WriteFile(configPath, []byte("agent:\n  temperature: 0.9\n"), 0600); err != nil {
			return nil, "", err
		}
		return &config.Config{
			Endpoint: gateway.URL,
			Agent:    config.AgentConfig{Temperature: 0.8},
		}, loadedRevision, nil
	}
	reloadResp, err := http.Post(api.URL+"/config/reload", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	var reloadBody map[string]any
	if err := json.NewDecoder(reloadResp.Body).Decode(&reloadBody); err != nil {
		reloadResp.Body.Close()
		t.Fatal(err)
	}
	reloadResp.Body.Close()
	if reloadResp.StatusCode != http.StatusOK || reloadBody["status"] != "reloaded" {
		t.Fatalf("config reload = %d %#v", reloadResp.StatusCode, reloadBody)
	}
	if deps.Config.Agent.Temperature != 0.8 {
		t.Fatalf("live config temperature = %v, want loaded 0.8", deps.Config.Agent.Temperature)
	}
	status = getConfigStatusE2E(t, api.URL)
	if status["reload_required"] != true {
		t.Fatalf("edit during reload was incorrectly marked applied: %#v", status)
	}
}

func getConfigStatusE2E(t *testing.T, baseURL string) map[string]any {
	t.Helper()
	resp, err := http.Get(baseURL + "/config/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /config/status = %d %#v", resp.StatusCode, body)
	}
	return body
}
