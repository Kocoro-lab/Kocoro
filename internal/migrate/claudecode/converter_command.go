package claudecode

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	slug := "claude-command-" + c.Name
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
		if len(t) > 200 {
			t = t[:200]
		}
		return t
	}
	return "Imported Claude Code custom command"
}

// escapeYAML wraps a value in double quotes when it would otherwise break
// a flow-scalar mapping line. Keeps the synthesized frontmatter parseable.
func escapeYAML(s string) string {
	if strings.ContainsAny(s, `:"'`+"\n") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}
