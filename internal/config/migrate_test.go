package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeYAML(t *testing.T, dir, contents string) string {
	t.Helper()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func findBackup(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "config.yaml.pre-migrate-") {
			return filepath.Join(dir, e.Name())
		}
	}
	return ""
}

func markerApplied(t *testing.T, dir, id string) bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, migrationsFileName))
	if err != nil {
		return false
	}
	var st migrationsState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("unmarshal marker: %v", err)
	}
	_, ok := st.Applied[id]
	return ok
}

func TestMigrate_HappyPath_128to200(t *testing.T) {
	dir := t.TempDir()
	const input = `agent:
    context_window: 128000
    max_iterations: 25
endpoint: https://api-dev.shannon.run
`
	configPath := writeYAML(t, dir, input)

	RunPendingMigrations(dir)

	got := readFile(t, configPath)
	const want = `agent:
    context_window: 200000
    max_iterations: 25
endpoint: https://api-dev.shannon.run
`
	if got != want {
		t.Fatalf("yaml mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}

	if findBackup(t, dir) == "" {
		t.Fatal("expected backup file matching config.yaml.pre-migrate-* in dir")
	}
	backupContents := readFile(t, findBackup(t, dir))
	if backupContents != input {
		t.Fatalf("backup contents drifted from original input\nwant:\n%s\ngot:\n%s", input, backupContents)
	}

	if !markerApplied(t, dir, migrationIDContextWindow128To200) {
		t.Fatal("expected migration marker to be recorded")
	}
}

func TestMigrate_ValueNotTarget_NoOpButMarked(t *testing.T) {
	dir := t.TempDir()
	const input = `agent:
    context_window: 64000
    max_iterations: 25
`
	configPath := writeYAML(t, dir, input)

	RunPendingMigrations(dir)

	if got := readFile(t, configPath); got != input {
		t.Fatalf("yaml should be byte-identical when value != 128000\nwant:\n%s\ngot:\n%s", input, got)
	}
	if findBackup(t, dir) != "" {
		t.Fatal("no backup should be created when no write happens")
	}
	if !markerApplied(t, dir, migrationIDContextWindow128To200) {
		t.Fatal("marker should still be recorded so subsequent launches skip the check")
	}
}

func TestMigrate_YAMLAbsent_NoOpButMarked(t *testing.T) {
	dir := t.TempDir() // no config.yaml here

	RunPendingMigrations(dir)

	if findBackup(t, dir) != "" {
		t.Fatal("no backup when yaml absent")
	}
	if !markerApplied(t, dir, migrationIDContextWindow128To200) {
		t.Fatal("marker should be recorded even when yaml absent")
	}
}

func TestMigrate_MalformedYAML_NoOpButMarked(t *testing.T) {
	dir := t.TempDir()
	const broken = `agent:
    context_window: 128000
  bogus_indent_unindented_back
`
	configPath := writeYAML(t, dir, broken)

	RunPendingMigrations(dir)

	if got := readFile(t, configPath); got != broken {
		t.Fatalf("malformed yaml must not be rewritten\nwant:\n%s\ngot:\n%s", broken, got)
	}
	if findBackup(t, dir) != "" {
		t.Fatal("no backup when migration aborts on malformed yaml")
	}
	if !markerApplied(t, dir, migrationIDContextWindow128To200) {
		t.Fatal("marker should be recorded so next launch doesn't re-touch broken yaml")
	}
}

func TestMigrate_AlreadyApplied_Skip(t *testing.T) {
	dir := t.TempDir()
	const input = `agent:
    context_window: 128000
`
	configPath := writeYAML(t, dir, input)

	// Pre-seed marker as if a previous launch already applied.
	preExisting := migrationsState{
		Applied: map[string]migrationRecord{
			migrationIDContextWindow128To200: {AppliedAt: "2026-01-01T00:00:00Z"},
		},
	}
	if err := saveMigrationsState(dir, preExisting); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	RunPendingMigrations(dir)

	if got := readFile(t, configPath); got != input {
		t.Fatalf("yaml must not be touched when marker already present\nwant:\n%s\ngot:\n%s", input, got)
	}
	if findBackup(t, dir) != "" {
		t.Fatal("no backup when migration is skipped via marker")
	}
}

func TestMigrate_AgentSectionAbsent_NoOp(t *testing.T) {
	dir := t.TempDir()
	const input = `endpoint: https://api-dev.shannon.run
api_key: ""
`
	configPath := writeYAML(t, dir, input)

	RunPendingMigrations(dir)

	if got := readFile(t, configPath); got != input {
		t.Fatalf("yaml should be byte-identical when agent section is absent\nwant:\n%s\ngot:\n%s", input, got)
	}
	if findBackup(t, dir) != "" {
		t.Fatal("no backup when there's nothing to migrate")
	}
}

// Verify that ONLY the targeted line changes — comments, key order,
// trailing whitespace, and unrelated values must remain byte-for-byte.
func TestMigrate_PreservesFormatting(t *testing.T) {
	dir := t.TempDir()
	const input = `# top-level comment
endpoint: https://api-dev.shannon.run

agent:
    # nested comment
    max_iterations: 25
    context_window: 128000  # old default
    temperature: 0.5
mcp_servers:
    github:
        command: gh
`
	configPath := writeYAML(t, dir, input)

	RunPendingMigrations(dir)

	got := readFile(t, configPath)
	const want = `# top-level comment
endpoint: https://api-dev.shannon.run

agent:
    # nested comment
    max_iterations: 25
    context_window: 200000  # old default
    temperature: 0.5
mcp_servers:
    github:
        command: gh
`
	if got != want {
		t.Fatalf("only the integer should change; everything else byte-identical\nwant:\n%s\ngot:\n%s", want, got)
	}
}

// Top-level `context_window:` (not under any indent) must NOT match the
// migration regex. This guards against accidentally rewriting an unrelated
// top-level key that happens to share the name.
func TestMigrate_TopLevelKeyNotMatched(t *testing.T) {
	dir := t.TempDir()
	const input = `context_window: 128000
agent:
    max_iterations: 25
`
	configPath := writeYAML(t, dir, input)

	RunPendingMigrations(dir)

	if got := readFile(t, configPath); got != input {
		t.Fatalf("top-level context_window must not be rewritten (no `agent:` parent)\nwant:\n%s\ngot:\n%s", input, got)
	}
}

func TestReplaceIndentedIntLine_MultipleIndentStyles(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "2-space indent",
			in:   "agent:\n  context_window: 128000\n",
			want: "agent:\n  context_window: 200000\n",
		},
		{
			name: "4-space indent",
			in:   "agent:\n    context_window: 128000\n",
			want: "agent:\n    context_window: 200000\n",
		},
		{
			name: "tab indent",
			in:   "agent:\n\tcontext_window: 128000\n",
			want: "agent:\n\tcontext_window: 200000\n",
		},
		{
			name: "trailing comment preserved",
			in:   "agent:\n  context_window: 128000  # default\n",
			want: "agent:\n  context_window: 200000  # default\n",
		},
		{
			name: "no trailing newline",
			in:   "agent:\n  context_window: 128000",
			want: "agent:\n  context_window: 200000",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, ok := replaceIndentedIntLine([]byte(tc.in), "context_window", 128000, 200000)
			if !ok {
				t.Fatalf("expected replacement to succeed for input:\n%s", tc.in)
			}
			if string(out) != tc.want {
				t.Fatalf("mismatch\nwant:\n%s\ngot:\n%s", tc.want, string(out))
			}
		})
	}
}

// Markers file written under a non-existent dir should not panic and
// should not crash RunPendingMigrations — the load is best-effort.
func TestRunPendingMigrations_EmptyDirNoOp(t *testing.T) {
	RunPendingMigrations("") // must not panic
}
