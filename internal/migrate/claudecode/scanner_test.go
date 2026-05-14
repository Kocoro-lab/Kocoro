package claudecode

import (
	"os"
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

// TestScan_ClaudeHomeIsSymlink_Rejected proves the privacy invariant: if the
// top-level ~/.claude root is itself a symlink (which could redirect to an
// attacker-controlled directory), Scan() refuses to traverse it and records
// a symlink_escape warning. Per-entry symlink rejection inside sub-scanners
// only triggers AFTER the root is opened — root protection has to live in
// Scan() itself.
func TestScan_ClaudeHomeIsSymlink_Rejected(t *testing.T) {
	realDir := t.TempDir()
	// Populate the real dir with a normal-looking Claude tree to prove that
	// even an "innocent" symlink target is refused.
	if err := os.MkdirAll(filepath.Join(realDir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "skills", "innocent.md"),
		[]byte("---\nname: innocent\ndescription: x\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	parent := t.TempDir()
	symlinkRoot := filepath.Join(parent, ".claude")
	if err := os.Symlink(realDir, symlinkRoot); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}

	src := SourcePaths{
		ClaudeHome:       symlinkRoot,
		ClaudeUserConfig: filepath.Join("testdata", "claude_user_config_basic.json"),
	}
	got, _ := Scan(src)
	if len(got.Skills) != 0 {
		t.Errorf("symlinked ClaudeHome must not yield skills, got %+v", got.Skills)
	}
	if _, ok := got.SourceErrors["claude_home"]; !ok {
		t.Error("expected SourceErrors entry for symlinked claude_home")
	}
	sawWarning := false
	for _, w := range got.Warnings {
		if w.Kind == "symlink_escape" && w.Path == "~/.claude" {
			sawWarning = true
		}
	}
	if !sawWarning {
		t.Errorf("expected symlink_escape warning at ~/.claude, got %+v", got.Warnings)
	}
	// Sanity: the OTHER source (MCP) should still scan fine.
	if len(got.MCPServers) == 0 {
		t.Error("symlinked claude_home should not affect MCP scan from claude_user_config")
	}
}
