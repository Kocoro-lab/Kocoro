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
	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
)

// TestMCPDefaultDisabledEndpoint_AddRemove exercises POST/DELETE
// /mcp/default-disabled through the full router, asserting config + in-memory
// mirror update and the empty-body guard.
func TestMCPDefaultDisabledEndpoint_AddRemove(t *testing.T) {
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
	rec := send(http.MethodPost, "/mcp/default-disabled", `{"server":"longbridge"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /mcp/default-disabled = %d, body=%s", rec.Code, rec.Body.String())
	}
	data, _ := os.ReadFile(filepath.Join(shannonDir, "config.yaml"))
	if !strings.Contains(string(data), "longbridge") {
		t.Fatalf("config.yaml missing longbridge:\n%s", data)
	}
	if !slices.Contains(deps.Config.MCP.DefaultAgentDisabled, "longbridge") {
		t.Fatalf("in-memory MCP.DefaultAgentDisabled missing longbridge: %v", deps.Config.MCP.DefaultAgentDisabled)
	}

	// Remove
	rec = send(http.MethodDelete, "/mcp/default-disabled", `{"server":"longbridge"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /mcp/default-disabled = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "removed") {
		t.Fatalf("DELETE body unexpected: %s", rec.Body.String())
	}
	if slices.Contains(deps.Config.MCP.DefaultAgentDisabled, "longbridge") {
		t.Fatalf("in-memory list still has longbridge after delete: %v", deps.Config.MCP.DefaultAgentDisabled)
	}

	// Empty server → 400
	rec = send(http.MethodPost, "/mcp/default-disabled", `{"server":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty server = %d, want 400", rec.Code)
	}
}

// TestConfigStatus_MCPDefaultAgentDisabled asserts GET /config/status echoes the
// default-agent MCP denylist so Desktop can seed the default agent's toggles.
func TestConfigStatus_MCPDefaultAgentDisabled(t *testing.T) {
	deps := &ServerDeps{
		ShannonDir: t.TempDir(),
		AgentsDir:  t.TempDir(),
		Config: &config.Config{
			MCPServers: map[string]mcp.MCPServerConfig{
				"serverA": {},
				"serverB": {},
			},
			MCP: config.MCPConfig{DefaultAgentDisabled: []string{"serverB"}},
			Koe: config.KoeConfig{AudioProcessing: "clean_device"},
		},
	}
	srv := NewServer(0, nil, deps, "test")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/config/status", nil)
	srv.handleConfigStatus(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /config/status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	raw, ok := resp["mcp_default_agent_disabled"]
	if !ok {
		t.Fatalf("response missing mcp_default_agent_disabled: %v", resp)
	}
	list, _ := raw.([]interface{})
	found := false
	for _, v := range list {
		if v == "serverB" {
			found = true
		}
	}
	if !found {
		t.Errorf("mcp_default_agent_disabled should contain serverB, got %v", raw)
	}
	koeBlock, ok := resp["koe"].(map[string]interface{})
	if !ok {
		t.Fatalf("response missing koe block: %v", resp)
	}
	if got := koeBlock["audio_processing"]; got != "clean_device" {
		t.Fatalf("koe.audio_processing = %v, want clean_device", got)
	}
}
