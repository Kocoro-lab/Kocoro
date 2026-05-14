package claudecode

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ConvertCommand wraps a Claude custom command into a Kocoro skill at
// stagingDir. Output slug is "claude-command-<name>" — the result page must
// say "imported as Kocoro skills" so the user understands the routing.
// Source frontmatter is intentionally discarded: we always synthesize fresh
// minimal frontmatter with the enforced slug and a description derived from
// the body. The body content (everything after the source's '---' fences,
// or the entire file if there were none) is preserved verbatim.
func ConvertCommand(c ScannedCommand, stagingDir, importedAt string) error {
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return err
	}
	data, err := os.ReadFile(c.SrcAbsPath)
	if err != nil {
		return err
	}
	_, body, ok := splitFrontmatter(string(data))
	if !ok {
		body = string(data)
	}
	slug := commandSkillSlug(c.Name)
	desc := extractDescription(body)
	header := fmt.Sprintf("---\nname: %s\ndescription: %s\nlicense: imported\n---\n", slug, escapeYAML(desc))
	banner := fmt.Sprintf("<!-- imported from ~/.claude/commands/%s.md on %s -->\n", c.Name, importedAt)
	out := header + banner + strings.TrimLeft(body, "\n")
	return os.WriteFile(filepath.Join(stagingDir, "SKILL.md"), []byte(out), 0o644)
}

// extractDescription picks the first H1 (stripping '# ') or the first
// non-empty line if no H1, truncated to 200 chars. Falls back to a generic
// description if the body has nothing usable.
func extractDescription(body string) string {
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		t = strings.TrimPrefix(t, "# ")
		return truncateRunes(t, 200)
	}
	return "Imported Claude Code custom command"
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	for i := range s {
		if max == 0 {
			return s[:i]
		}
		max--
	}
	return s
}

// escapeYAML emits a double-quoted scalar so synthesized frontmatter remains
// parseable for descriptions that start with YAML indicators or contain
// special flow characters.
func escapeYAML(s string) string {
	node := yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   "!!str",
		Value: s,
		Style: yaml.DoubleQuotedStyle,
	}
	out, err := yaml.Marshal(&node)
	if err != nil {
		return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
	}
	return strings.TrimSuffix(string(out), "\n")
}
