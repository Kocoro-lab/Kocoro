package agents

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestEnsureBuiltins_CreatesOnFirstRun(t *testing.T) {
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "agents")
	os.MkdirAll(agentsDir, 0700)

	err := EnsureBuiltins(agentsDir, "0.0.99-test")
	if err != nil {
		t.Fatalf("EnsureBuiltins: %v", err)
	}

	// Verify explorer
	data, err := os.ReadFile(filepath.Join(agentsDir, "_builtin", "scout", "AGENT.md"))
	if err != nil {
		t.Fatalf("explorer AGENT.md missing: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("explorer AGENT.md is empty")
	}

	// Verify reviewer
	data, err = os.ReadFile(filepath.Join(agentsDir, "_builtin", "checker", "AGENT.md"))
	if err != nil {
		t.Fatalf("reviewer AGENT.md missing: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("reviewer AGENT.md is empty")
	}

	// Verify version file
	ver, err := os.ReadFile(filepath.Join(agentsDir, "_builtin", ".version"))
	if err != nil {
		t.Fatalf(".version missing: %v", err)
	}
	if string(ver) != "0.0.99-test" {
		t.Fatalf("expected version 0.0.99-test, got %s", string(ver))
	}
}

func TestEnsureBuiltins_SkipsWhenVersionMatches(t *testing.T) {
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "agents")
	os.MkdirAll(agentsDir, 0700)

	// First run
	EnsureBuiltins(agentsDir, "0.0.99")

	// Modify a file to detect if it gets overwritten
	marker := filepath.Join(agentsDir, "_builtin", "scout", "AGENT.md")
	os.WriteFile(marker, []byte("modified"), 0600)

	// Second run with same version
	EnsureBuiltins(agentsDir, "0.0.99")

	data, _ := os.ReadFile(marker)
	if string(data) != "modified" {
		t.Fatal("EnsureBuiltins overwrote files when version matched")
	}
}

func TestEnsureBuiltins_OverwritesOnVersionChange(t *testing.T) {
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "agents")
	os.MkdirAll(agentsDir, 0700)

	EnsureBuiltins(agentsDir, "0.0.98")

	marker := filepath.Join(agentsDir, "_builtin", "scout", "AGENT.md")
	os.WriteFile(marker, []byte("modified"), 0600)

	// Upgrade
	EnsureBuiltins(agentsDir, "0.0.99")

	data, _ := os.ReadFile(marker)
	if string(data) == "modified" {
		t.Fatal("EnsureBuiltins did not overwrite on version change")
	}
}

func TestMaterializeBuiltin(t *testing.T) {
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "agents")

	builtinDir := filepath.Join(agentsDir, "_builtin", "scout")
	os.MkdirAll(builtinDir, 0700)
	os.WriteFile(filepath.Join(builtinDir, "AGENT.md"), []byte("builtin prompt"), 0600)
	os.WriteFile(filepath.Join(builtinDir, "config.yaml"), []byte("tools:\n  allow: [bash]"), 0600)

	userDir := filepath.Join(agentsDir, "scout")
	if _, err := os.Stat(filepath.Join(userDir, "AGENT.md")); err == nil {
		t.Fatal("user dir should not exist before materialization")
	}

	err := MaterializeBuiltin(agentsDir, "scout")
	if err != nil {
		t.Fatalf("MaterializeBuiltin: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(userDir, "AGENT.md"))
	if err != nil || string(data) != "builtin prompt" {
		t.Fatalf("AGENT.md not materialized correctly")
	}
	data, err = os.ReadFile(filepath.Join(userDir, "config.yaml"))
	if err != nil || string(data) != "tools:\n  allow: [bash]" {
		t.Fatalf("config.yaml not materialized correctly")
	}
}

func TestBuiltinNames_MatchEmbeddedDirs(t *testing.T) {
	entries, err := fs.ReadDir(builtinFS, "builtin")
	if err != nil {
		t.Fatalf("reading embedded FS: %v", err)
	}
	var embedded []string
	for _, e := range entries {
		if e.IsDir() {
			embedded = append(embedded, e.Name())
		}
	}
	sort.Strings(embedded)

	names := make([]string, len(BuiltinNames))
	copy(names, BuiltinNames)
	sort.Strings(names)

	if len(embedded) != len(names) {
		t.Fatalf("BuiltinNames has %d entries but embedded FS has %d dirs: embedded=%v names=%v",
			len(names), len(embedded), embedded, names)
	}
	for i := range embedded {
		if embedded[i] != names[i] {
			t.Fatalf("mismatch at index %d: embedded=%q names=%q", i, embedded[i], names[i])
		}
	}
}

func TestMaterializeBuiltin_SkipsMemory(t *testing.T) {
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "agents")
	builtinDir := filepath.Join(agentsDir, "_builtin", "scout")
	os.MkdirAll(builtinDir, 0700)
	os.WriteFile(filepath.Join(builtinDir, "AGENT.md"), []byte("prompt"), 0600)
	os.WriteFile(filepath.Join(builtinDir, "MEMORY.md"), []byte("should not copy"), 0600)

	MaterializeBuiltin(agentsDir, "scout")

	userDir := filepath.Join(agentsDir, "scout")
	if _, err := os.Stat(filepath.Join(userDir, "MEMORY.md")); err == nil {
		t.Fatal("MEMORY.md should not be materialized")
	}
}
