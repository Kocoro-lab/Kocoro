package skills

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// SkillDetail is the API response type for GET /skills/{name}.
// Includes prompt body and source, unlike SkillMeta (metadata only)
// or Skill (which hides Source/Dir via json:"-" tags).
type SkillDetail struct {
	Name            string            `json:"name"`
	Description     string            `json:"description"`
	Prompt          string            `json:"prompt"`
	Source          string            `json:"source"`
	InstallSource   string            `json:"install_source"`
	MarketplaceSlug string            `json:"marketplace_slug,omitempty"`
	License         string            `json:"license,omitempty"`
	Compatibility   string            `json:"compatibility,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	AllowedTools    []string          `json:"allowed_tools,omitempty"`
}

// WriteGlobalSkill writes a skill to the global skills directory
// (~/.shannon/skills/<name>/SKILL.md). Same atomic write pattern
// as agents.WriteAgentSkill but different path root.
func WriteGlobalSkill(shannonDir string, skill *Skill) error {
	dir := filepath.Join(shannonDir, "skills", skill.Name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	fm := skillFrontmatter{
		Name:          skill.Name,
		Description:   skill.Description,
		License:       skill.License,
		Compatibility: skill.Compatibility,
		Metadata:      skill.Metadata,
	}
	if len(skill.AllowedTools) > 0 {
		fm.AllowedTools = strings.Join(skill.AllowedTools, " ")
	}

	fmBytes, err := yaml.Marshal(fm)
	if err != nil {
		return fmt.Errorf("marshal frontmatter: %w", err)
	}

	var buf strings.Builder
	buf.WriteString("---\n")
	buf.Write(fmBytes)
	buf.WriteString("---\n\n")
	buf.WriteString(skill.Prompt)
	if !strings.HasSuffix(skill.Prompt, "\n") {
		buf.WriteString("\n")
	}

	if err := atomicWrite(filepath.Join(dir, "SKILL.md"), []byte(buf.String())); err != nil {
		return err
	}
	return clearMarketplaceProvenance(dir)
}

// DeleteGlobalSkill removes a global skill directory.
func DeleteGlobalSkill(shannonDir, name string) error {
	if err := ValidateSkillName(name); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(shannonDir, "skills", name))
}

// DownloadableSkill describes a skill available for download from Anthropic's repo.
type DownloadableSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Installed   bool   `json:"installed"`
}

// DownloadableSkills is the registry of skills available for on-demand installation.
// Includes both formerly-bundled skills (copied from embedded binary) and
// proprietary skills (fetched from Anthropic's repo).
var DownloadableSkills = []struct {
	Name        string
	Description string
}{
	// Formerly bundled — installed from embedded binary
	{"algorithmic-art", "Create algorithmic art using p5.js with seeded randomness"},
	{"brand-guidelines", "Apply brand colors and typography to artifacts"},
	{"canvas-design", "Create visual art in PNG and PDF using design philosophy"},
	{"claude-api", "Build apps with the Claude API or Anthropic SDK"},
	{"doc-coauthoring", "Structured workflow for co-authoring documentation"},
	{"frontend-design", "Create production-grade frontend interfaces with high design quality"},
	{"internal-comms", "Write internal communications using company formats"},
	{"mcp-builder", "Create MCP servers for LLM-to-service integration"},
	{"skill-creator", "Create, modify, and measure skill performance"},
	{"slack-gif-creator", "Create animated GIFs optimized for Slack"},
	{"theme-factory", "Style artifacts with pre-set or custom themes"},
	{"web-artifacts-builder", "Create multi-component HTML artifacts with React and Tailwind"},
	{"webapp-testing", "Test local web applications using Playwright"},
	// Proprietary — installed from Anthropic's repo
	{"docx", "Document creation, editing, and analysis with tracked changes and comments"},
	{"pdf", "PDF extraction, creation, merging, splitting, and form filling"},
	{"pptx", "Presentation creation, editing, and analysis"},
	{"xlsx", "Spreadsheet creation, editing, analysis with formulas and formatting"},
}

// IsDownloadable returns true if the skill name is in the downloadable registry.
func IsDownloadable(name string) bool {
	for _, s := range DownloadableSkills {
		if s.Name == name {
			return true
		}
	}
	return false
}

// InstallSkill installs a downloadable skill to the global skills directory
// (~/.shannon/skills/<name>/). First checks if the skill is available in the
// embedded bundled directory (fast, no network). Falls back to fetching from
// Anthropic's skills repo via git sparse checkout.
func InstallSkill(shannonDir, name string) error {
	if err := ValidateSkillName(name); err != nil {
		return err
	}
	if !IsDownloadable(name) {
		return fmt.Errorf("skill %q is not available for download", name)
	}

	destDir := filepath.Join(shannonDir, "skills", name)
	if _, err := os.Stat(filepath.Join(destDir, "SKILL.md")); err == nil {
		return fmt.Errorf("skill %q is already installed", name)
	}

	// Try bundled source first (no network required)
	if err := installFromBundled(shannonDir, name, destDir); err == nil {
		return nil
	}

	// Fall back to Anthropic's repo
	return installFromRepo(shannonDir, name, destDir)
}

// installFromBundled copies a skill from the embedded bundled directory to global.
func installFromBundled(shannonDir, name, destDir string) error {
	bundledSrc, err := BundledSkillSource(shannonDir)
	if err != nil {
		return err
	}
	srcDir := filepath.Join(bundledSrc.Dir, name)
	skillMD := filepath.Join(srcDir, "SKILL.md")
	if _, err := os.Stat(skillMD); err != nil {
		return fmt.Errorf("skill %q not in bundled dir", name)
	}

	if err := os.MkdirAll(filepath.Dir(destDir), 0700); err != nil {
		return err
	}

	// Copy directory contents (bundled dir is read-only, can't rename)
	return copyDir(srcDir, destDir)
}

// installFromRepo downloads a skill from Anthropic's skills repo via git sparse checkout.
func installFromRepo(shannonDir, name, destDir string) error {
	tmpDir, err := os.MkdirTemp(shannonDir, "skill-install-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	if err := runGit(tmpDir, "clone", "--depth=1", "--filter=blob:none", "--sparse",
		"https://github.com/anthropics/skills.git", "."); err != nil {
		return fmt.Errorf("git clone: %w", err)
	}
	if err := runGit(tmpDir, "sparse-checkout", "set", "skills/"+name); err != nil {
		return fmt.Errorf("git sparse-checkout: %w", err)
	}

	srcDir := filepath.Join(tmpDir, "skills", name)
	if _, err := os.Stat(filepath.Join(srcDir, "SKILL.md")); err != nil {
		return fmt.Errorf("skill %q not found in Anthropic repo", name)
	}

	if err := os.MkdirAll(filepath.Dir(destDir), 0700); err != nil {
		return err
	}
	return os.Rename(srcDir, destDir)
}

// copyDir recursively copies a directory tree.
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		destPath := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(destPath, 0700)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(destPath, content, 0644)
	})
}

// InstallSkillFromRepo is a backwards-compatible alias for InstallSkill.
// Deprecated: use InstallSkill instead.
func InstallSkillFromRepo(shannonDir, name string) error {
	return InstallSkill(shannonDir, name)
}

func runGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// atomicWrite writes data to a temp file then renames to path.
func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
