package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/skills/bundled"
	"gopkg.in/yaml.v3"
)

// SkillDetail is the API response type for GET /skills/{name}.
// Includes prompt body and source, unlike SkillMeta (metadata only)
// or Skill (which hides Source/Dir via json:"-" tags).
type SkillDetail struct {
	Name              string         `json:"name"`
	Slug              string         `json:"slug"`
	Description       string         `json:"description"`
	Prompt            string         `json:"prompt"`
	Source            string         `json:"source"`
	InstallSource     string         `json:"install_source"`
	MarketplaceSlug   string         `json:"marketplace_slug,omitempty"`
	License           string         `json:"license,omitempty"`
	Compatibility     string         `json:"compatibility,omitempty"`
	Metadata          map[string]any `json:"metadata,omitempty"`
	AllowedTools      []string       `json:"allowed_tools,omitempty"`
	StickyInstructions bool          `json:"sticky_instructions,omitempty"`
	Hidden            bool           `json:"hidden,omitempty"`
	StickySnippet     string         `json:"sticky_snippet,omitempty"`
	RequiredSecrets   []SecretSpec   `json:"required_secrets,omitempty"`
	ConfiguredSecrets []string       `json:"configured_secrets,omitempty"`
}

// WriteGlobalSkill writes a skill to the global skills directory
// (~/.shannon/skills/<slug>/SKILL.md). Same atomic write pattern
// as agents.WriteAgentSkill but different path root.
//
// Directory is keyed by Slug (the URL/on-disk identifier); Name is the
// frontmatter display label and may contain uppercase / CJK / spaces,
// neither of which is safe for a filesystem path. Falls back to Name
// for skills created before the Name/Slug split where Slug is unset.
func WriteGlobalSkill(shannonDir string, skill *Skill) error {
	dirKey := skill.Slug
	if dirKey == "" {
		dirKey = skill.Name
	}
	dir := filepath.Join(shannonDir, "skills", dirKey)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	fm := skillFrontmatter{
		Name:               skill.Name,
		Description:        skill.Description,
		License:            skill.License,
		Compatibility:      skill.Compatibility,
		Metadata:           skill.Metadata,
		StickyInstructions: skill.StickyInstructions,
		Hidden:             skill.Hidden,
	}
	if len(skill.AllowedTools) > 0 {
		fm.AllowedTools = strings.Join(skill.AllowedTools, " ")
	}
	// Only marshal the sticky-snippet when the author explicitly pinned one
	// (via StickySnippetOverride). The resolved StickySnippet may come from
	// the heuristic extractor; serializing that would freeze a heuristic
	// choice into the file and, on the next reload, skip Pass-1 entirely.
	if override := strings.TrimSpace(skill.StickySnippetOverride); override != "" {
		fm.StickySnippet = override
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
	{"pdf-reader", "Analyze PDF files using file_read's built-in PDF rendering and vision"},
	{"algorithmic-art", "Create algorithmic art using p5.js with seeded randomness"},
	{"brand-guidelines", "Apply brand colors and typography to artifacts"},
	{"canvas-design", "Create visual art in PNG and PDF using design philosophy"},
	{"claude-api", "Build apps with the Claude API or Anthropic SDK"},
	{"doc-coauthoring", "Structured workflow for co-authoring documentation"},
	{"frontend-design", "Create production-grade frontend interfaces with high design quality"},
	{"heatmap-analyze", "End-to-end Ptengine heatmap analysis with AI-powered CRO insights"},
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

// builtinSkills are skills that are auto-installed on startup.
// Unlike other bundled skills (which require manual installation),
// these are always available without user action.
var builtinSkills = []string{"kocoro", "kocoro-generative-ui"}

// IsBuiltinSkill reports whether slug is one of the auto-installed builtins
// that EnsureBuiltinSkills syncs into the global skills dir on every startup.
// Uploads / overrides targeting these slugs are rejected because the next
// daemon restart would wipe the user's version back to embed.FS contents.
func IsBuiltinSkill(slug string) bool {
	for _, name := range builtinSkills {
		if name == slug {
			return true
		}
	}
	return false
}

// EnsureBuiltinSkills syncs every builtin skill in the global skills directory
// against the binary's embed.FS. For each builtin: hash the embed.FS tree and
// the on-disk tree; if they differ (including disk dir missing), wipe and
// rewrite from embed.FS atomically. If they match, leave the directory alone.
//
// Content-addressed by design — there is no version sidecar to drift, no disk
// cache layer (`bundled-skills/`) to go stale on dev builds, and no edge case
// where the binary upgraded but the on-disk SKILL.md didn't. Two consequences
// the previous version-sidecar design tolerated and this design rejects:
//
//   - User edits to builtin skills are wiped on next startup. Builtins are
//     daemon-managed; users who want to customize should fork under a
//     different skill name.
//   - Every startup pays a sha256 walk over the on-disk subtree (~15 small
//     markdown files). The embed-side hashes are memoized per-process so
//     repeat callers (daemon + TUI in the same binary) only pay the disk walk.
//
// Concurrent callers (daemon and TUI cold-starting at the same time) are
// serialized through `~/.shannon/skills/.builtin.lock` — without it, both
// would race on `RemoveAll(destDir)` followed by per-file `.tmp` renames and
// could leave a partial tree until the next startup re-ran the overlay.
//
// The benefit: deleting `~/.shannon/skills/kocoro` self-heals, regardless of
// build-time version metadata. Mirrors agents.EnsureBuiltins's intent without
// inheriting its dev-build fragility.
//
// Called at daemon/TUI/CLI startup alongside agents.EnsureBuiltins.
func EnsureBuiltinSkills(shannonDir string) error {
	globalSkills := filepath.Join(shannonDir, "skills")
	if err := os.MkdirAll(globalSkills, 0700); err != nil {
		return err
	}

	lockPath := filepath.Join(globalSkills, ".builtin.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open builtin lock: %w", err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock builtin: %w", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

	// Best-effort cleanup of the legacy version sidecar from the previous
	// design. Safe to ignore errors — it is purely informational and an
	// existing one no longer affects behavior.
	_ = os.Remove(filepath.Join(globalSkills, "_builtin.version"))

	for _, name := range builtinSkills {
		destDir := filepath.Join(globalSkills, name)
		match, err := builtinMatchesEmbed(name, destDir)
		if err != nil {
			return fmt.Errorf("compare builtin skill %s: %w", name, err)
		}
		if match {
			continue
		}
		if err := overlayBuiltinFromEmbed(name, destDir); err != nil {
			return fmt.Errorf("install builtin skill %s: %w", name, err)
		}
	}
	return nil
}

// builtinMatchesEmbed returns true when destDir is byte-for-byte identical to
// the embed.FS subtree at skills/<name>/. A missing destDir counts as a
// mismatch (triggers install). Hashes file relative paths and contents so a
// reference file that exists only on disk (e.g. an orphan from a previous
// bundled version) also counts as a mismatch and gets wiped on overlay.
func builtinMatchesEmbed(name, destDir string) (bool, error) {
	embedHash, err := hashEmbedBuiltin(name)
	if err != nil {
		return false, fmt.Errorf("hash embed: %w", err)
	}
	diskHash, err := hashDirIfPresent(destDir)
	if err != nil {
		return false, fmt.Errorf("hash disk: %w", err)
	}
	if diskHash == "" {
		return false, nil
	}
	return embedHash == diskHash, nil
}

// hashEmbedBuiltin returns the sha256 of the embed.FS subtree at
// skills/<name>/, memoized per name for the lifetime of the process. The
// embed.FS contents are baked into the binary, so the hash is invariant —
// recomputing it on every EnsureBuiltinSkills call would be wasted work
// when daemon and TUI are linked into the same binary or when the function
// is called multiple times in tests.
func hashEmbedBuiltin(name string) (string, error) {
	hashOnceMu.Lock()
	fn, ok := hashOnce[name]
	if !ok {
		fn = sync.OnceValues(func() (string, error) { return computeEmbedBuiltinHash(name) })
		hashOnce[name] = fn
	}
	hashOnceMu.Unlock()
	return fn()
}

var (
	hashOnceMu sync.Mutex
	hashOnce   = make(map[string]func() (string, error))
)

// computeEmbedBuiltinHash walks bundled.FS at skills/<name>/ and returns a
// sha256 over (relative path, content length, content) for every file. Path
// and length framing prevents prefix collisions and rename ambiguity.
func computeEmbedBuiltinHash(name string) (string, error) {
	root := "skills/" + name
	h := sha256.New()
	err := fs.WalkDir(bundled.FS, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := bundled.FS.ReadFile(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", filepath.ToSlash(rel), len(data))
		h.Write(data)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashDirIfPresent walks dir on disk with the same framing as hashEmbedBuiltin.
// Returns ("", nil) when dir does not exist so the caller can distinguish
// "missing" from "present but empty".
func hashDirIfPresent(dir string) (string, error) {
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	h := sha256.New()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", filepath.ToSlash(rel), len(data))
		h.Write(data)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// overlayBuiltinFromEmbed replaces destDir with the contents of bundled.FS at
// skills/<name>/. destDir is wiped first so orphan files from a prior bundled
// version (e.g. a reference file that was renamed or removed) don't linger.
// Per-file atomic writes (temp + rename) bound the partial-state window to a
// single file; the next startup re-hashes and self-heals if interrupted.
func overlayBuiltinFromEmbed(name, destDir string) error {
	if err := os.RemoveAll(destDir); err != nil {
		return err
	}
	if err := os.MkdirAll(destDir, 0700); err != nil {
		return err
	}
	root := "skills/" + name
	return fs.WalkDir(bundled.FS, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destDir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		data, err := bundled.FS.ReadFile(path)
		if err != nil {
			return err
		}
		return atomicWrite(target, data)
	})
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

// installFromRepoMaxAttempts is the number of times installFromRepo retries
// the git operations on transient failures. Tokyo/CN corporate networks see
// intermittent github.com reachability flakes; observed user workaround was
// to click Install again from the Desktop UI. Backoff: 0s, 1s, 2s → ≤3s of
// added wait worst-case, on top of actual clone time.
// Override: not currently exposed; recompile if 3 attempts proves wrong.
const installFromRepoMaxAttempts = 3

// ErrSkillNotInRepo is returned by tryInstallFromRepo when `git clone` and
// `sparse-checkout` succeed but the requested skill's directory is absent
// from the upstream tree — a deterministic 404 against
// github.com/anthropics/skills, not a transient flake. installFromRepo
// uses errors.Is to short-circuit retry on this sentinel.
var ErrSkillNotInRepo = errors.New("skill not found in Anthropic repo")

// installFromRepo downloads a skill from Anthropic's skills repo via git sparse checkout.
// Retries the git operations on transient failures (network flake, github.com
// reachability). Does NOT retry ErrSkillNotInRepo — that's a real 404 against
// the upstream tree, not a flake.
func installFromRepo(shannonDir, name, destDir string) error {
	var lastErr error
	for attempt := 0; attempt < installFromRepoMaxAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
		err := tryInstallFromRepo(shannonDir, name, destDir)
		if err == nil {
			return nil
		}
		// Don't retry a genuine "skill not in upstream" — that's deterministic.
		if errors.Is(err, ErrSkillNotInRepo) {
			return err
		}
		lastErr = err
	}
	return lastErr
}

// tryInstallFromRepo is a single attempt of installFromRepo. Each attempt
// uses a fresh tmpDir so a partially-written clone from a previous failure
// does not contaminate the next try.
func tryInstallFromRepo(shannonDir, name, destDir string) error {
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
		return fmt.Errorf("%w: %q", ErrSkillNotInRepo, name)
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

// runGit is a package-level indirection so retry tests in api_test.go can
// inject failures without spawning a real `git` binary. Production callers
// see no behavioral difference from a regular function — restore by writing
// runGit = runGitReal in a t.Cleanup.
var runGit = runGitReal

func runGitReal(dir string, args ...string) error {
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
