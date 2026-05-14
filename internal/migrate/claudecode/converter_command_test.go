package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

func TestConvertCommand_SynthesizesFrontmatterFromH1(t *testing.T) {
	src := filepath.Join("testdata", "claude_home_basic", "commands", "review.md")
	staging := t.TempDir()
	c := ScannedCommand{Name: "review", SrcAbsPath: src}
	if err := ConvertCommand(c, staging, "2026-05-14T11:22:00Z"); err != nil {
		t.Fatalf("ConvertCommand: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(staging, "SKILL.md"))
	if err != nil {
		t.Fatalf("SKILL.md missing: %v", err)
	}
	s := string(data)
	if !strings.HasPrefix(s, "---\n") {
		t.Errorf("expected synthesized frontmatter, got: %s", s)
	}
	if !strings.Contains(s, "name: claude-command-review") {
		t.Errorf("expected commands-as-skills slug, got: %s", s)
	}
	if !strings.Contains(s, "description:") {
		t.Errorf("expected description field: %s", s)
	}
	if !strings.Contains(s, "imported from ~/.claude/commands/review.md") {
		t.Errorf("expected import banner: %s", s)
	}
}

func TestConvertCommand_RewritesExistingFrontmatterToSlug(t *testing.T) {
	src := filepath.Join("testdata", "claude_home_basic", "commands", "deploy.md")
	staging := t.TempDir()
	c := ScannedCommand{Name: "deploy", SrcAbsPath: src}
	if err := ConvertCommand(c, staging, "2026-05-14T11:22:00Z"); err != nil {
		t.Fatalf("ConvertCommand: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(staging, "SKILL.md"))
	s := string(data)
	if !strings.Contains(s, "name: claude-command-deploy") {
		t.Errorf("source 'name: deploy' should be rewritten to slug, got: %s", s)
	}
	// Body lines after the original frontmatter must survive.
	if !strings.Contains(s, "Body of deploy command") {
		t.Errorf("body lost: %s", s)
	}
}

func TestExtractDescription_TruncatesByRune(t *testing.T) {
	desc := extractDescription("# " + strings.Repeat("界", 201))
	if !utf8.ValidString(desc) {
		t.Fatalf("description is invalid UTF-8")
	}
	if got := utf8.RuneCountInString(desc); got != 200 {
		t.Fatalf("description runes = %d, want 200", got)
	}
}

func TestEscapeYAML_ParsesSpecialScalars(t *testing.T) {
	want := `# [deploy] & check: "prod" > now`
	var got map[string]string
	if err := yaml.Unmarshal([]byte("description: "+escapeYAML(want)+"\n"), &got); err != nil {
		t.Fatalf("description YAML did not parse: %v", err)
	}
	if got["description"] != want {
		t.Fatalf("description = %q, want %q", got["description"], want)
	}
	if !strings.HasPrefix(escapeYAML("plain"), `"`) {
		t.Fatalf("description scalar should be quoted")
	}
}
