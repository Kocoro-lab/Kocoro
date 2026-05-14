package claudecode

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// agentFrontmatter mirrors the Claude Code agent frontmatter shape. Tools
// may be a comma-separated string OR a YAML list; both are normalized.
type agentFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Tools       any    `yaml:"tools"`
	Model       string `yaml:"model"`
}

var knownAgentFrontmatterKeys = map[string]bool{
	"name": true, "description": true, "tools": true, "model": true,
}

// ConvertAgent reads Claude's single-file agent markdown, splits frontmatter
// into config.yaml, and writes the body to AGENT.md with an import banner.
// Unknown frontmatter keys are surfaced as an unsupported_fields warning
// (not silently dropped) so the user knows what didn't translate.
func ConvertAgent(a ScannedAgent, stagingDir, importedAt string) ([]Warning, error) {
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(a.SrcAbsPath)
	if err != nil {
		return nil, err
	}
	fmText, body, _ := splitFrontmatter(string(data))

	var meta agentFrontmatter
	var raw map[string]any
	var warns []Warning
	if fmText != "" {
		if err := yaml.Unmarshal([]byte(fmText), &meta); err != nil {
			return nil, fmt.Errorf("agent %q frontmatter parse: %w", a.Name, err)
		}
		// Decode again into a generic map to detect unknown keys.
		_ = yaml.Unmarshal([]byte(fmText), &raw)
		if unknown := unknownFrontmatterKeys(raw); len(unknown) > 0 {
			warns = append(warns, Warning{
				Kind:   "unsupported_fields",
				Server: a.Name,
				Fields: unknown,
			})
		}
	}

	// AGENT.md: banner + body (body retains trailing whitespace from source).
	banner := fmt.Sprintf("<!-- imported from ~/.claude/agents/%s.md on %s -->\n", a.Name, importedAt)
	if meta.Description != "" {
		banner += fmt.Sprintf("<!-- description: %s -->\n", meta.Description)
	}
	banner += "\n"
	agentPath := filepath.Join(stagingDir, "AGENT.md")
	if err := os.WriteFile(agentPath, []byte(banner+strings.TrimLeft(body, "\n")), 0o644); err != nil {
		return nil, err
	}

	// config.yaml: only known fields are emitted.
	cfg := agentConfigYAML(meta, a.Name, importedAt)
	if err := os.WriteFile(filepath.Join(stagingDir, "config.yaml"), []byte(cfg), 0o644); err != nil {
		return nil, err
	}
	return warns, nil
}

func splitFrontmatter(s string) (fm, body string, ok bool) {
	if !strings.HasPrefix(s, "---") {
		return "", s, false
	}
	parts := strings.SplitN(s, "---", 3)
	if len(parts) < 3 {
		return "", s, false
	}
	return strings.TrimSpace(parts[1]), parts[2], true
}

func unknownFrontmatterKeys(raw map[string]any) []string {
	var unknown []string
	for k := range raw {
		if !knownAgentFrontmatterKeys[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	return unknown
}

func agentConfigYAML(m agentFrontmatter, name, importedAt string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# imported from ~/.claude/agents/%s.md on %s\n", name, importedAt)
	if tools := normalizeTools(m.Tools); len(tools) > 0 {
		b.WriteString("tools:\n  allow:\n")
		for _, t := range tools {
			fmt.Fprintf(&b, "    - %s\n", t)
		}
	}
	if m.Model != "" {
		fmt.Fprintf(&b, "agent:\n  model: %s\n", m.Model)
	}
	return b.String()
}

func normalizeTools(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		var out []string
		for _, t := range strings.Split(x, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				out = append(out, t)
			}
		}
		return out
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				s = strings.TrimSpace(s)
				if s != "" {
					out = append(out, s)
				}
			}
		}
		return out
	}
	return nil
}
