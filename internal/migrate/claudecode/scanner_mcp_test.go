package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanMCP_ExtractsKeysNotValues(t *testing.T) {
	path := filepath.Join("testdata", "claude_user_config_basic.json")
	got, _, err := scanMCP(path)
	if err != nil {
		t.Fatalf("scanMCP: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 servers, got %d", len(got))
	}
	byName := map[string]ScannedMCPServer{}
	for _, s := range got {
		byName[s.Name] = s
	}

	// anthropic should expose key NAME but never value
	a := byName["anthropic"]
	if a.Transport != "stdio" {
		t.Errorf("anthropic.Transport = %q", a.Transport)
	}
	if len(a.EnvKeys) != 1 || a.EnvKeys[0] != "ANTHROPIC_API_KEY" {
		t.Errorf("anthropic.EnvKeys = %v", a.EnvKeys)
	}

	// internal-api: http with unsupported headers
	i := byName["internal-api"]
	if i.Transport != "http" {
		t.Errorf("internal-api.Transport = %q", i.Transport)
	}
	if len(i.UnsupportedFields) == 0 || i.UnsupportedFields[0] != "headers" {
		t.Errorf("internal-api.UnsupportedFields = %v", i.UnsupportedFields)
	}

	// command-only: no env, no warnings
	c := byName["command-only"]
	if len(c.EnvKeys) != 0 {
		t.Errorf("command-only.EnvKeys = %v", c.EnvKeys)
	}

	// hardest check: serialize the result and assert no leaked values appear
	blob, _ := json.Marshal(got)
	for _, leak := range []string{"sk-ant-DO-NOT-LEAK", "X-Auth", "DO-NOT-LEAK"} {
		// X-Auth header NAME may be acceptable to surface in a future "unsupported header names" UI,
		// but for v1 we redact the *value*. Check for the value specifically.
		if leak == "DO-NOT-LEAK" || strings.HasPrefix(leak, "sk-") {
			if strings.Contains(string(blob), leak) {
				t.Errorf("LEAK: serialized result contains %q", leak)
			}
		}
	}
}

func TestScanMCP_SymlinkConfigRejected(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "claude.json")
	if err := os.WriteFile(outside, []byte(`{"mcpServers":{"leak":{"command":"node"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "claude.json")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}

	got, warns, err := scanMCP(link)
	if err != nil {
		t.Fatalf("scanMCP: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("symlinked MCP config should be skipped, got %+v", got)
	}
	gotEscape := false
	for _, w := range warns {
		if w.Kind == "symlink_escape" && w.Path == "~/.claude.json" {
			gotEscape = true
		}
	}
	if !gotEscape {
		t.Errorf("expected symlink_escape warning, got %+v", warns)
	}
}
