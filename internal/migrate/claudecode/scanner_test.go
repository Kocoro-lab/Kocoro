package claudecode

import (
	"path/filepath"
	"testing"
)

func TestScan_BothSourcesPresent(t *testing.T) {
	src := SourcePaths{
		ClaudeHome:       filepath.Join("testdata", "claude_home_basic"),
		ClaudeUserConfig: filepath.Join("testdata", "claude_user_config_basic.json"),
	}
	got, err := Scan(src)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got.Skills) == 0 {
		t.Error("expected skills")
	}
	if len(got.Agents) == 0 {
		t.Error("expected agents")
	}
	if len(got.Commands) == 0 {
		t.Error("expected commands")
	}
	if got.GlobalRules == nil {
		t.Error("expected global rules")
	}
	if len(got.MCPServers) == 0 {
		t.Error("expected MCP servers")
	}
}

func TestScan_OnlyMCP_StillSucceeds(t *testing.T) {
	src := SourcePaths{
		ClaudeHome:       filepath.Join("testdata", "does-not-exist"),
		ClaudeUserConfig: filepath.Join("testdata", "claude_user_config_basic.json"),
	}
	got, err := Scan(src)
	if err != nil {
		t.Fatalf("Scan should not error when one source is missing: %v", err)
	}
	if len(got.MCPServers) == 0 {
		t.Fatal("expected MCP servers from claude_user_config")
	}
	if _, ok := got.SourceErrors["claude_home"]; !ok {
		t.Error("expected source error for claude_home")
	}
}

// TestScan_BothSourcesMissing_TotalImportableZero confirms that when neither
// source is reachable, TotalImportable() reports zero so the handler can
// translate the state into a 404 claude_not_found response per spec §12.1.
func TestScan_BothSourcesMissing_TotalImportableZero(t *testing.T) {
	src := SourcePaths{
		ClaudeHome:       filepath.Join("testdata", "does-not-exist"),
		ClaudeUserConfig: filepath.Join("testdata", "also-missing.json"),
	}
	got, err := Scan(src)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got.TotalImportable() != 0 {
		t.Errorf("TotalImportable = %d, want 0", got.TotalImportable())
	}
}

func TestScan_BothSourcesPresent_TotalImportableCovers(t *testing.T) {
	src := SourcePaths{
		ClaudeHome:       filepath.Join("testdata", "claude_home_basic"),
		ClaudeUserConfig: filepath.Join("testdata", "claude_user_config_basic.json"),
	}
	got, _ := Scan(src)
	want := len(got.Skills) + len(got.Agents) + len(got.Commands) + len(got.MCPServers)
	if got.GlobalRules != nil {
		want++
	}
	if got.TotalImportable() != want {
		t.Errorf("TotalImportable() = %d, want %d", got.TotalImportable(), want)
	}
}
