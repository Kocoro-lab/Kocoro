package skills

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/fslock"
	"github.com/Kocoro-lab/ShanClaw/internal/skills/bundled"
	"gopkg.in/yaml.v3"
)

// SkillDetail is the API response type for GET /skills/{name}.
// Includes prompt body and source, unlike SkillMeta (metadata only)
// or Skill (which hides Source/Dir via json:"-" tags).
type SkillDetail struct {
	Name               string         `json:"name"`
	Slug               string         `json:"slug"`
	Description        string         `json:"description"`
	Prompt             string         `json:"prompt"`
	Source             string         `json:"source"`
	InstallSource      string         `json:"install_source"`
	MarketplaceSlug    string         `json:"marketplace_slug,omitempty"`
	License            string         `json:"license,omitempty"`
	Compatibility      string         `json:"compatibility,omitempty"`
	Metadata           map[string]any `json:"metadata,omitempty"`
	AllowedTools       []string       `json:"allowed_tools,omitempty"`
	StickyInstructions bool           `json:"sticky_instructions,omitempty"`
	Hidden             bool           `json:"hidden,omitempty"`
	StickySnippet      string         `json:"sticky_snippet,omitempty"`
	RequiredSecrets    []SecretSpec   `json:"required_secrets,omitempty"`
	ConfiguredSecrets  []string       `json:"configured_secrets,omitempty"`
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
		// stringOrList.MarshalYAML re-joins these into the scalar string form,
		// so the on-disk SKILL.md is byte-identical to the previous behavior.
		fm.AllowedTools = stringOrList(skill.AllowedTools)
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

// BuiltinSkillNames returns the slugs of the always-available builtin skills
// that EnsureBuiltinSkills auto-installs into the global skills dir. Returned as
// a fresh slice so callers cannot mutate the package-level source of truth.
func BuiltinSkillNames() []string {
	out := make([]string, len(builtinSkills))
	copy(out, builtinSkills)
	return out
}

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
	if err := fslock.Lock(lockFile.Fd()); err != nil {
		return fmt.Errorf("lock builtin: %w", err)
	}
	defer fslock.Unlock(lockFile.Fd())

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

// ErrPreviewUnavailable is returned by PreviewSkill when a downloadable skill
// has no locally-available SKILL.md — it is neither installed nor present in the
// embedded bundle. The four proprietary skills (docx/pdf/pptx/xlsx) ship only as
// a git-fetched install and have no bundled copy, so there is nothing to preview
// before installing.
var ErrPreviewUnavailable = errors.New("no local preview available for skill")

// PreviewSkill returns the raw SKILL.md content for a downloadable skill WITHOUT
// installing it, so the UI can show a full preview before the user commits. The
// content is served entirely from the daemon (never the network):
//  1. Already installed on disk → read the on-disk SKILL.md.
//  2. Bundled (embedded in the binary) → read from the extracted bundled dir.
//
// If neither exists (a proprietary skill not yet installed), returns
// ErrPreviewUnavailable so the caller can fall back to the short description.
func PreviewSkill(shannonDir, name string) (string, error) {
	if err := ValidateSkillName(name); err != nil {
		return "", err
	}
	if !IsDownloadable(name) {
		return "", fmt.Errorf("skill %q is not available for download", name)
	}

	// 1. Installed copy on disk (also reflects any local edits).
	if data, err := os.ReadFile(filepath.Join(shannonDir, "skills", name, "SKILL.md")); err == nil {
		return string(data), nil
	}

	// 2. Bundled (embedded) source — offline, no network.
	if bundledSrc, err := BundledSkillSource(shannonDir); err == nil {
		if data, err := os.ReadFile(filepath.Join(bundledSrc.Dir, name, "SKILL.md")); err == nil {
			return string(data), nil
		}
	}

	return "", ErrPreviewUnavailable
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
// the tarball download on transient failures. Tokyo/CN corporate networks see
// intermittent github.com reachability flakes; observed user workaround was
// to click Install again from the Desktop UI. Backoff: 0s, 1s, 2s → ≤3s of
// added wait worst-case, on top of actual download time.
// Override: not currently exposed; recompile if 3 attempts proves wrong.
const installFromRepoMaxAttempts = 3

// ErrSkillNotInRepo is returned by tryInstallFromRepo when the tarball
// downloads and extracts fine but the requested skill's directory is absent
// from the upstream tree — a deterministic 404 against
// github.com/anthropics/skills, not a transient flake. installFromRepo
// uses errors.Is to short-circuit retry on this sentinel.
var ErrSkillNotInRepo = errors.New("skill not found in Anthropic repo")

// installFromRepo downloads a skill from Anthropic's skills repo over HTTP
// (the GitHub codeload tarball — no `git` binary required, so it works on a
// stock Windows/macOS/Linux machine that has never installed git). Retries the
// download on transient failures (network flake, github.com reachability).
// Does NOT retry ErrSkillNotInRepo — that's a real 404 against the upstream
// tree, not a flake.
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

// tryInstallFromRepo is a single attempt of installFromRepo. It downloads the
// anthropics/skills repo as a gzip tarball, extracts only skills/<name>/** into
// a fresh per-attempt staging dir, then copies that into destDir. A fresh
// tmpDir per attempt guarantees a partially-written download from a previous
// failure cannot contaminate the next try.
func tryInstallFromRepo(shannonDir, name, destDir string) error {
	tmpDir, err := os.MkdirTemp(shannonDir, "skill-install-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	rc, err := openRepoTarball(context.Background())
	if err != nil {
		return fmt.Errorf("download skills tarball: %w", err)
	}
	defer rc.Close()

	// Extract into a staging dir first; only touch destDir once the whole
	// extraction succeeds so a mid-download failure never leaves a half-written
	// skill in ~/.shannon/skills.
	stageDir := filepath.Join(tmpDir, "stage")
	if err := extractSkillFromTarball(rc, name, stageDir); err != nil {
		return err // ErrSkillNotInRepo when the skill is absent upstream
	}

	if err := os.MkdirAll(filepath.Dir(destDir), 0700); err != nil {
		return err
	}
	// Clear any leftover/partial destDir before copying in. Use copyDir rather
	// than os.Rename: on Windows MoveFile refuses an existing target directory,
	// and rename would also fail with EXDEV if TMPDIR/shannonDir ever land on
	// different volumes. copyDir (MkdirAll + per-file WriteFile) is safe on all.
	if err := os.RemoveAll(destDir); err != nil {
		return err
	}
	return copyDir(stageDir, destDir)
}

// anthropicSkillsTarballURL is the GitHub codeload endpoint that serves the
// whole anthropics/skills repo as a gzip tarball for the main branch. codeload
// is the same backend `git` archive/clone hits, but over plain HTTP — no local
// git binary, no partial-clone/sparse-checkout git-version requirements.
const anthropicSkillsTarballURL = "https://codeload.github.com/anthropics/skills/tar.gz/refs/heads/main"

// Extraction backstops for the downloaded tarball. Generous but bounded: the
// payload is attacker-influenced (a compromised upstream / MITM), so cap total
// decompressed bytes (zip-bomb guard), per-file size, and file count. Vars (not
// consts) so tests can shrink them to exercise the guards cheaply.
var (
	maxSkillTarballBytes int64 = 200 << 20 // 200 MiB total decompressed
	maxSkillFileBytes    int64 = 25 << 20  // 25 MiB per file
	maxSkillFiles              = 10000
)

// skillsHTTPClient downloads the skills tarball. The 2-minute ceiling bounds a
// stalled download so an install can never hang the HTTP handler forever (the
// old git path had no such deadline).
var skillsHTTPClient = &http.Client{Timeout: 2 * time.Minute}

// openRepoTarball is the single injectable seam (it replaces the former
// `runGit` seam) so retry/not-found tests in api_test.go can feed a synthetic
// tarball or a transient error without any network access.
var openRepoTarball = openRepoTarballReal

func openRepoTarballReal(ctx context.Context) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, anthropicSkillsTarballURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := skillsHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("codeload returned %s", resp.Status)
	}
	return resp.Body, nil
}

// extractSkillFromTarball reads a gzip tar stream of the anthropics/skills repo
// and extracts only entries under <root>/skills/<name>/, stripping that prefix
// into destDir. codeload wraps the repo in a single top-level directory whose
// name embeds the ref (e.g. "skills-main/"); that name is not hardcoded — the
// first path segment is treated as the wrapper. Returns ErrSkillNotInRepo when
// no matching entry exists (deterministic upstream 404).
//
// Security: tar paths are always '/'-separated, so cleaning uses path.Clean
// (never filepath) to avoid the Windows backslash-as-separator asymmetry.
// Symlinks/hardlinks/devices are skipped (they can be a path-escape vector and
// never belong in a skill), and every target is re-checked to stay within
// destDir as defense-in-depth against tar-slip.
func extractSkillFromTarball(r io.Reader, name, destDir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(&countingReader{r: gz, limit: maxSkillTarballBytes})
	if err := os.MkdirAll(destDir, 0700); err != nil {
		return err
	}

	matched, files := 0, 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		rel, ok := stripSkillPrefix(path.Clean(hdr.Name), name)
		if !ok {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if rel == "" {
				continue
			}
			if err := os.MkdirAll(filepath.Join(destDir, filepath.FromSlash(rel)), 0700); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			files++
			if files > maxSkillFiles {
				return fmt.Errorf("skill %q exceeds %d files", name, maxSkillFiles)
			}
			target := filepath.Join(destDir, filepath.FromSlash(rel))
			if !withinDir(destDir, target) {
				return fmt.Errorf("unsafe tar path %q", hdr.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return err
			}
			if err := writeTarFile(target, tr); err != nil {
				return err
			}
			matched++
		default:
			// Skip symlinks/hardlinks/char/block/fifo — not expected in a skill
			// payload and a potential path-escape vector.
			continue
		}
	}
	if matched == 0 {
		return fmt.Errorf("%w: %q", ErrSkillNotInRepo, name)
	}
	return nil
}

// stripSkillPrefix matches "<root>/skills/<name>/<rest>" (or the bare
// "<root>/skills/<name>" directory entry) and returns <rest> ("" for the dir
// entry itself). ok is false for any path that isn't under the target skill.
func stripSkillPrefix(clean, name string) (rest string, ok bool) {
	// [root, "skills", name, rest] — SplitN keeps the remainder intact in [3].
	parts := strings.SplitN(clean, "/", 4)
	if len(parts) < 3 || parts[1] != "skills" || parts[2] != name {
		return "", false
	}
	if len(parts) == 3 {
		return "", true
	}
	return parts[3], true
}

// withinDir reports whether target resolves inside root (tar-slip guard).
func withinDir(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// writeTarFile writes one tar entry to target with a fixed 0644 mode. It caps
// actual bytes copied (guarding against a lying hdr.Size) and deliberately does
// NOT preserve the tar mode bits — matching copyDir/installFromBundled and
// sidestepping Windows, where os.Chmod cannot set an executable bit anyway.
func writeTarFile(target string, r io.Reader) error {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, maxSkillFileBytes+1))
	if err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if n > maxSkillFileBytes {
		return fmt.Errorf("tar file %q exceeds %d bytes", target, maxSkillFileBytes)
	}
	return nil
}

// countingReader caps the total number of bytes read from the underlying
// (decompressed) stream, so a zip-bomb tarball can't exhaust memory/disk during
// extraction regardless of how small the compressed payload is.
type countingReader struct {
	r     io.Reader
	n     int64
	limit int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.n > c.limit {
		return 0, fmt.Errorf("skills tarball exceeds %d bytes (decompression guard)", c.limit)
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
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

// atomicWrite writes data to a temp file then renames to dest.
func atomicWrite(dest string, data []byte) error {
	tmp := dest + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}
