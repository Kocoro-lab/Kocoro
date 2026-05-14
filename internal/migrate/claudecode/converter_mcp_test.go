package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeMCP_DisablesMissingEnvOrUnsupported(t *testing.T) {
	target := t.TempDir()
	existing := `endpoint: https://api-dev.shannon.run
mcp_servers:
  preexisting:
    command: foo
permissions:
  always_allow_tools:
    - bash
`
	if err := os.WriteFile(filepath.Join(target, "config.yaml"), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	servers := []ScannedMCPServer{
		{Name: "preexisting", Transport: "stdio", Command: "SHOULD-NOT-WRITE", Status: "ok"},
		{Name: "anthropic", Transport: "stdio", Command: "node", Args: []string{"server.js"}, EnvKeys: []string{"ANTHROPIC_API_KEY"}, Status: "ok"},
		{Name: "internal-api", Transport: "http", URL: "https://x", UnsupportedFields: []string{"headers"}, Status: "ok"},
		{Name: "command-only", Transport: "stdio", Command: "/usr/local/bin/mcp", Status: "ok"},
	}
	disabled := map[string]bool{"anthropic": true, "internal-api": true}

	if err := MergeMCPIntoConfig(target, servers, disabled, "2026-05-14T11:22:00Z"); err != nil {
		t.Fatalf("MergeMCPIntoConfig: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(target, "config.yaml"))
	s := string(data)

	// Pre-existing unrelated keys preserved.
	for _, want := range []string{"endpoint:", "preexisting:", "always_allow_tools:"} {
		if !strings.Contains(s, want) {
			t.Errorf("merged config dropped pre-existing key %q: %s", want, s)
		}
	}
	// All imported servers present.
	for _, want := range []string{"anthropic:", "internal-api:", "command-only:"} {
		if !strings.Contains(s, want) {
			t.Errorf("merged config missing imported server %q: %s", want, s)
		}
	}
	// Env key name present, value empty.
	if !strings.Contains(s, "ANTHROPIC_API_KEY:") {
		t.Errorf("ANTHROPIC_API_KEY name missing: %s", s)
	}
	// Value must be empty — never a real secret. Allow either '""' or empty after the colon.
	for _, leak := range []string{"sk-ant", "DO-NOT-LEAK"} {
		if strings.Contains(s, leak) {
			t.Errorf("LEAK: config.yaml contains %q", leak)
		}
	}
	// Disabled servers have `disabled: true` somewhere in their entry.
	if !strings.Contains(s, "disabled: true") {
		t.Errorf("expected at least one disabled:true entry: %s", s)
	}
	if strings.Contains(s, "SHOULD-NOT-WRITE") {
		t.Errorf("existing MCP server was overwritten: %s", s)
	}
	if _, err := os.Stat(filepath.Join(target, "config.yaml.pre-migrate-bak")); !os.IsNotExist(err) {
		t.Errorf("config backup should not be written; stat err=%v", err)
	}
}

func TestMergeMCP_PreservesUnknownConfigKeys(t *testing.T) {
	target := t.TempDir()
	existing := `endpoint: https://api-dev.shannon.run
some_future_key:
  nested:
    setting: 42
`
	if err := os.WriteFile(filepath.Join(target, "config.yaml"), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	servers := []ScannedMCPServer{
		{Name: "x", Transport: "stdio", Command: "node", Status: "ok"},
	}
	if err := MergeMCPIntoConfig(target, servers, nil, "2026-05-14T11:22:00Z"); err != nil {
		t.Fatalf("MergeMCPIntoConfig: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(target, "config.yaml"))
	s := string(data)
	if !strings.Contains(s, "some_future_key") || !strings.Contains(s, "setting: 42") {
		t.Errorf("merge must preserve unknown config keys: %s", s)
	}
}

func TestMergeMCP_NewConfigFile(t *testing.T) {
	target := t.TempDir()
	servers := []ScannedMCPServer{
		{Name: "fresh", Transport: "stdio", Command: "node", Status: "ok"},
	}
	if err := MergeMCPIntoConfig(target, servers, nil, "2026-05-14T11:22:00Z"); err != nil {
		t.Fatalf("MergeMCPIntoConfig: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(target, "config.yaml"))
	if err != nil {
		t.Fatalf("config.yaml not created: %v", err)
	}
	if !strings.Contains(string(data), "fresh") {
		t.Errorf("server missing: %s", data)
	}
}

func TestMergeMCP_SkipsErrorStatusServers(t *testing.T) {
	target := t.TempDir()
	servers := []ScannedMCPServer{
		{Name: "ok-one", Transport: "stdio", Command: "node", Status: "ok"},
		{Name: "bad-transport", Transport: "websocket", Status: "error", ErrorReason: "unsupported_transport"},
	}
	if err := MergeMCPIntoConfig(target, servers, nil, "2026-05-14T11:22:00Z"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(target, "config.yaml"))
	s := string(data)
	if strings.Contains(s, "bad-transport") {
		t.Errorf("error-status server must not be merged: %s", s)
	}
	if !strings.Contains(s, "ok-one") {
		t.Errorf("ok server must be merged: %s", s)
	}
}
