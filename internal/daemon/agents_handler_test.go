package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHandleAgentsIncludesDescription(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "finance")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte("# finance\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "PROFILE.yaml"),
		[]byte("description:\n  en: Stock and market analysis\n  zh: 金融与市场分析\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Server{deps: &ServerDeps{AgentsDir: dir}}
	req := httptest.NewRequest(http.MethodGet, "/agents", nil)
	rec := httptest.NewRecorder()
	s.handleAgents(rec, req)

	var body struct {
		Agents []struct {
			Name        string            `json:"name"`
			Description map[string]string `json:"description"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	var found bool
	for _, a := range body.Agents {
		if a.Name == "finance" {
			found = true
			if a.Description["en"] != "Stock and market analysis" {
				t.Errorf("finance description[en] = %q, want \"Stock and market analysis\"", a.Description["en"])
			}
		}
	}
	if !found {
		t.Fatalf("agent \"finance\" not in response: %s", rec.Body.String())
	}
}
