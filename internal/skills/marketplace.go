package skills

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/adrg/frontmatter"
	"gopkg.in/yaml.v3"
)

// marketplaceDownloadClient is the HTTP client used for zip-transport
// installs. Separate from the MarketplaceClient's registry client so
// downloads can tolerate slower upstream responses, but still has a
// 2-minute ceiling as a safety floor if the caller's context has no
// deadline.
var marketplaceDownloadClient = &http.Client{Timeout: 2 * time.Minute}

// runGitCtx is a context-aware variant of runGit from api.go. Lets
// InstallFromMarketplace cancel in-flight clones when the request
// context is canceled.
func runGitCtx(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RegistryIndex is the top-level JSON document served by the marketplace
// registry repo. Field names match the schema in
// docs/superpowers/specs/2026-04-06-skill-marketplace-design.md.
type RegistryIndex struct {
	Version   int                `json:"version"`
	UpdatedAt string             `json:"updated_at"`
	Skills    []MarketplaceEntry `json:"skills"`
}

// MarketplaceEntry is one skill listing in the registry.
//
// Transport: either Repo (git clone) or DownloadURL (HTTP zip) must be set.
// The git path is the primary transport for skills that have a public
// source repository. DownloadURL is the fallback for ClawHub skills that
// exist only as zip artifacts served by ClawHub's Convex backend — there
// is no GitHub repo to clone, so we fetch the zip and extract it
// directly into the skills directory.
type MarketplaceEntry struct {
	Slug        string       `json:"slug"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Author      string       `json:"author"`
	License     string       `json:"license,omitempty"`
	Repo        string       `json:"repo,omitempty"`
	RepoPath    string       `json:"repo_path,omitempty"`
	Ref         string       `json:"ref,omitempty"`
	DownloadURL string       `json:"download_url,omitempty"`
	Homepage    string       `json:"homepage,omitempty"`
	Downloads   int          `json:"downloads,omitempty"`
	Stars       int          `json:"stars,omitempty"`
	Version     string       `json:"version,omitempty"`
	Security    SecurityScan `json:"security,omitempty"`
	Tags        []string     `json:"tags,omitempty"`
}

// SecurityScan mirrors the scan results published by ClawHub.
// Empty strings mean "not scanned" and render as a neutral badge.
type SecurityScan struct {
	VirusTotal string `json:"virustotal,omitempty"`
	OpenClaw   string `json:"openclaw,omitempty"`
	ScannedAt  string `json:"scanned_at,omitempty"`
}

// IsMalicious returns true when any scanner flagged the entry as malicious.
// Used as a server-side gate in both the list and install endpoints.
func (e MarketplaceEntry) IsMalicious() bool {
	return e.Security.VirusTotal == "malicious" || e.Security.OpenClaw == "malicious"
}

// defaultStaleCooldown is the minimum gap between upstream refetch
// attempts once we've started serving stale. Without it, every Load
// during a registry outage would re-hit the remote and turn normal UI
// traffic into a retry storm.
const defaultStaleCooldown = 1 * time.Minute

// MarketplaceClient fetches and caches the registry index.
//
// Caching rules (see design doc §Registry Cache):
//   - First fetch populates the in-memory cache.
//   - Subsequent calls within TTL return the cached copy.
//   - After TTL expires, the next call refetches; on fetch failure the
//     previous cache is served as stale (IsStale() returns true) and a
//     retry cooldown is set so further Loads keep serving stale without
//     hammering the upstream.
//   - If no cache exists and fetch fails, Load returns the error.
type MarketplaceClient struct {
	url  string
	ttl  time.Duration
	http *http.Client

	// clawhubBase, when non-empty, switches Load() to fetch the catalog from
	// ClawHub's live HTTP API (paging /api/v1/skills) instead of the static
	// registry index at url. The fetched items are mapped into the same
	// RegistryIndex shape so handlers and install stay transport-agnostic.
	clawhubBase string

	// staleCooldown bounds how often we re-attempt an upstream fetch
	// while in stale mode. Exposed as a field (not a constructor arg)
	// so tests can set a short cooldown directly.
	staleCooldown time.Duration

	// maxAttempts / retryBase bound the in-client retry of transient upstream
	// failures on catalog GETs (fetch/getJSON/getText). Defaulted in the
	// constructors; overridable via SetRetryPolicy (server.go injects config
	// values). Fields (not constructor args) so tests set them directly.
	maxAttempts int
	retryBase   time.Duration

	mu         sync.Mutex
	cache      *RegistryIndex
	fetched    time.Time
	stale      bool
	retryAfter time.Time
}

// NewMarketplaceClient constructs a client with the given registry URL and
// cache TTL. A TTL of 0 forces every call to refetch (used by stale-on-error
// tests and by operators who explicitly disable caching).
func NewMarketplaceClient(url string, ttl time.Duration) *MarketplaceClient {
	return &MarketplaceClient{
		url:           url,
		ttl:           ttl,
		http:          &http.Client{Timeout: 15 * time.Second},
		staleCooldown: defaultStaleCooldown,
		maxAttempts:   defaultMarketplaceMaxAttempts,
		retryBase:     defaultMarketplaceRetryBase,
	}
}

// NewClawHubMarketplaceClient constructs a client that sources the catalog from
// ClawHub's live API at base (e.g. "https://clawhub.ai"). Same caching/stale
// semantics as the registry client.
func NewClawHubMarketplaceClient(base string, ttl time.Duration) *MarketplaceClient {
	return &MarketplaceClient{
		clawhubBase:   strings.TrimRight(base, "/"),
		ttl:           ttl,
		http:          &http.Client{Timeout: 15 * time.Second},
		staleCooldown: defaultStaleCooldown,
		maxAttempts:   defaultMarketplaceMaxAttempts,
		retryBase:     defaultMarketplaceRetryBase,
	}
}

// SetRetryPolicy overrides the transient-failure retry policy for catalog GETs
// (fetch/getJSON/getText). Called by the daemon with config-derived values;
// non-positive args are ignored so callers can pass 0 to keep the default.
func (c *MarketplaceClient) SetRetryPolicy(maxAttempts int, base time.Duration) {
	if maxAttempts > 0 {
		c.maxAttempts = maxAttempts
	}
	if base > 0 {
		c.retryBase = base
	}
}

// Load returns the current registry index, refetching when the cache is
// empty or past TTL. See the type doc for stale-on-error semantics.
func (c *MarketplaceClient) Load(ctx context.Context) (*RegistryIndex, error) {
	c.mu.Lock()
	// Fresh cache → return immediately.
	if c.cache != nil && time.Since(c.fetched) < c.ttl {
		c.stale = false
		cached := c.cache
		c.mu.Unlock()
		return cached, nil
	}
	// Stale-mode cooldown in effect → keep serving stale without
	// re-attempting the upstream fetch. Prevents retry storms during
	// registry outages.
	if c.cache != nil && !c.retryAfter.IsZero() && time.Now().Before(c.retryAfter) {
		c.stale = true
		cached := c.cache
		c.mu.Unlock()
		return cached, nil
	}
	c.mu.Unlock()

	// Fetch WITHOUT holding c.mu: the fetch now retries with backoff sleeps
	// (up to tens of seconds during an outage), and holding the lock across it
	// would serialize every concurrent Load()/IsStale() caller for that whole
	// window. The cost is that a cold-cache stampede may run a few fetches in
	// parallel; the staleCooldown set on the first failure quickly suppresses
	// further ones, and jittered backoff spreads the load.
	idx, err := c.fetch(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		if c.cache != nil {
			c.stale = true
			c.retryAfter = time.Now().Add(c.staleCooldown)
			return c.cache, nil
		}
		return nil, err
	}
	c.cache = idx
	c.fetched = time.Now()
	c.stale = false
	c.retryAfter = time.Time{}
	return c.cache, nil
}

// IsStale reports whether the most recent Load served a stale cache because
// the upstream fetch failed. Handlers use this to set an X-Cache-Stale header.
func (c *MarketplaceClient) IsStale() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stale
}

// Sentinel errors so daemon handlers can map to exact HTTP statuses without
// parsing message strings.
var (
	ErrSkillAlreadyInstalled      = errors.New("skill already installed")
	ErrMaliciousSkill             = errors.New("skill blocked by security scan")
	ErrInvalidSkillPayload        = errors.New("invalid skill payload")
	ErrMarketplaceUpstreamFailure = errors.New("marketplace upstream failure")
	// ErrSkillIsBuiltin is returned when a ZIP upload targets one of the
	// auto-installed builtins (IsBuiltinSkill). EnsureBuiltinSkills overwrites
	// these on every daemon restart, so user uploads would silently revert.
	ErrSkillIsBuiltin = errors.New("skill is an auto-installed builtin and cannot be replaced via upload")
	// ErrZipTooLarge is returned when the compressed payload exceeds
	// maxZipCompressedBytes or the sum of uncompressed entry sizes exceeds
	// maxZipUncompressedBytes (zip-bomb guard).
	ErrZipTooLarge = errors.New("zip payload too large")
)

// ConflictPromptPreviewBytes caps the prompt fields surfaced in a 409 conflict
// response. ZIP payloads can be up to 1 GiB and the extracted SKILL.md prompt
// may be much of that; returning the raw bytes inline produces an unmanageable
// JSON response. 8 KB is enough for a meaningful compare preview while
// keeping the response body bounded. Exported so the daemon's PUT /skills/{name}
// path can apply the same cap when surfacing the same 409 shape.
const ConflictPromptPreviewBytes = 8 * 1024

// SkillConflictError is returned by InstallFromZipData when a skill with the
// same slug already exists globally and force=false. The handler encodes all
// fields in the 409 body so the Desktop can render a side-by-side compare sheet.
// ExistingPrompt and NewPrompt are truncated to ConflictPromptPreviewBytes;
// callers needing the full body should fetch GET /skills/{slug}.
type SkillConflictError struct {
	ExistingName        string
	ExistingDescription string
	ExistingPrompt      string
	NewDescription      string
	NewPrompt           string
}

func (e *SkillConflictError) Error() string {
	return fmt.Sprintf("skill %q already installed", e.ExistingName)
}

// TruncatePromptPreview caps a prompt body at ConflictPromptPreviewBytes
// (total byte length including the marker), appending "[truncated]" so
// callers know the value is partial and can follow up with GET /skills/{slug}
// for the full body. Walks back to a rune boundary so multibyte content
// isn't cut mid-codepoint. Used by both /skills/upload (zip path) and
// PUT /skills/{name} (manual create/update path) so the 409 body shape and
// caps stay aligned across write endpoints.
func TruncatePromptPreview(s string) string {
	if len(s) <= ConflictPromptPreviewBytes {
		return s
	}
	const marker = "\n\n[truncated]"
	budget := ConflictPromptPreviewBytes - len(marker)
	if budget < 0 {
		budget = 0
	}
	truncated := s[:budget]
	// UTF-8 runes are at most 4 bytes; a cut lands at most 3 bytes into a
	// partial sequence. Walk back to the last valid rune boundary in O(1)
	// per step, bounded to 3 iterations. (The prior utf8.ValidString loop
	// rescanned the whole prefix each step — O(n²) on adversarial input.)
	for i := 0; i < 3 && len(truncated) > 0; i++ {
		r, size := utf8.DecodeLastRuneInString(truncated)
		if r != utf8.RuneError || size > 1 {
			break
		}
		truncated = truncated[:len(truncated)-1]
	}
	return truncated + marker
}

// InstallFromMarketplace runs the full install flow for a marketplace entry.
// Dispatches to the git transport (clone → stage) when entry.Repo is set,
// or the zip transport (HTTP GET → extract) when entry.DownloadURL is set.
// Both paths share the same validation rules, slug lock, sentinel errors,
// and cleanup guarantees.
//
// ctx is propagated into the transport layer: git clone runs under
// exec.CommandContext, zip downloads run under an http.Request with the
// same context. Cancellation aborts the in-flight operation and cleans
// up staging dirs on the way out.
//
// Steps common to both transports:
//  1. Validate slug.
//  2. Security gate (malicious → ErrMaliciousSkill).
//  3. Per-slug lock (serializes concurrent installs for the same slug).
//  4. Already-installed check (→ ErrSkillAlreadyInstalled).
//  5. Transport-specific payload acquisition into a stage directory.
//  6. Verify SKILL.md exists and parses; verify frontmatter name == slug.
//  7. Atomic rename stage → ~/.shannon/skills/<slug>/.
//
// All failures clean up temp directories. No partial installs ever remain.
func InstallFromMarketplace(ctx context.Context, shannonDir string, entry MarketplaceEntry, locks *SlugLocks) error {
	if err := ValidateSkillName(entry.Slug); err != nil {
		return err
	}
	if entry.IsMalicious() {
		return ErrMaliciousSkill
	}
	if entry.Repo == "" && entry.DownloadURL == "" {
		return fmt.Errorf("%w: entry has no transport (need repo or download_url)", ErrInvalidSkillPayload)
	}

	unlock := locks.Lock(entry.Slug)
	defer unlock()

	destDir := filepath.Join(shannonDir, "skills", entry.Slug)
	if _, err := os.Stat(filepath.Join(destDir, "SKILL.md")); err == nil {
		return ErrSkillAlreadyInstalled
	}

	tmpRoot := filepath.Join(shannonDir, "tmp")
	if err := os.MkdirAll(tmpRoot, 0700); err != nil {
		return fmt.Errorf("create tmp root: %w", err)
	}

	stageDir, err := os.MkdirTemp(tmpRoot, "skill-stage-"+entry.Slug+"-*")
	if err != nil {
		return fmt.Errorf("create stage dir: %w", err)
	}
	// stageDir is removed on failure; on success we rename it away so
	// the RemoveAll is a no-op.
	defer os.RemoveAll(stageDir)

	// Transport dispatch: git path clones via exec.CommandContext so
	// cancellation aborts in-flight clones; zip path passes ctx to the
	// http.Request so cancellation aborts in-flight downloads.
	if entry.Repo != "" {
		if err := installFromGit(ctx, entry, stageDir, tmpRoot); err != nil {
			return err
		}
	} else {
		// Remove the empty stageDir MkdirTemp created; extractZipToSkill
		// recreates it inside its own cleanup guarantees.
		os.RemoveAll(stageDir)
		if err := installFromZip(ctx, entry, stageDir); err != nil {
			return err
		}
	}

	// Verify SKILL.md exists and matches the declared slug. Same rules
	// apply regardless of transport.
	skillFile := filepath.Join(stageDir, "SKILL.md")
	if _, err := os.Stat(skillFile); err != nil {
		return fmt.Errorf("%w: SKILL.md missing at stage dir", ErrInvalidSkillPayload)
	}
	// loadSkillMD passes dirName=entry.Slug and enforces that the zip's
	// canonical identity (frontmatter `slug` when present, else `name`)
	// matches — so a separate `parsed.Name != entry.Slug` check here would
	// just duplicate that invariant.
	if _, err := loadSkillMD(skillFile, entry.Slug, "marketplace"); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSkillPayload, err)
	}
	if err := writeMarketplaceProvenance(stageDir, entry.Slug); err != nil {
		return fmt.Errorf("write marketplace provenance: %w", err)
	}

	// Atomic rename into place.
	if err := os.MkdirAll(filepath.Dir(destDir), 0700); err != nil {
		return fmt.Errorf("create skills dir: %w", err)
	}
	if err := os.Rename(stageDir, destDir); err != nil {
		return fmt.Errorf("install rename: %w", err)
	}

	return nil
}

// InstallFromZipData installs a skill from a ZIP payload uploaded directly
// by the user (POST /skills/upload). Reuses extractZipToSkill for extraction
// and security checks. Derives the slug from the frontmatter `name` field.
//
// If body is a GitHub-style ZIP (single top-level directory), the directory
// is automatically unwrapped so SKILL.md need not be at root level.
//
// Error mapping:
//   - ErrZipTooLarge          → 413 in handler
//   - ErrInvalidSkillPayload  → 422 in handler
//   - ErrSkillIsBuiltin       → 403 in handler
//   - *SkillConflictError     → 409 in handler
func InstallFromZipData(shannonDir string, body io.Reader, force bool, locks *SlugLocks) (*Skill, error) {
	tmpRoot := filepath.Join(shannonDir, "tmp")
	if err := os.MkdirAll(tmpRoot, 0700); err != nil {
		return nil, fmt.Errorf("create tmp root: %w", err)
	}
	stageDir, err := os.MkdirTemp(tmpRoot, "skill-upload-*")
	if err != nil {
		return nil, fmt.Errorf("create stage dir: %w", err)
	}
	// stageDir removed on failure; on success Rename moves it away (no-op RemoveAll)
	defer os.RemoveAll(stageDir)

	if err := extractZipToSkill(body, stageDir); err != nil {
		if errors.Is(err, ErrZipTooLarge) {
			return nil, ErrZipTooLarge
		}
		return nil, fmt.Errorf("%w: %v", ErrInvalidSkillPayload, err)
	}

	// Unwrap single-top-level-dir ZIPs (GitHub / Finder download format).
	// Ignore hidden entries (dot-prefixed) when counting real subdirs.
	skillRoot := stageDir
	if _, err := os.Stat(filepath.Join(skillRoot, "SKILL.md")); os.IsNotExist(err) {
		entries, readErr := os.ReadDir(skillRoot)
		if readErr == nil {
			var realDirs []os.DirEntry
			for _, e := range entries {
				if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && e.Name() != "__MACOSX" {
					realDirs = append(realDirs, e)
				}
			}
			if len(realDirs) == 1 {
				skillRoot = filepath.Join(skillRoot, realDirs[0].Name())
			}
		}
	}

	skillFile := filepath.Join(skillRoot, "SKILL.md")
	rawMD, err := os.ReadFile(skillFile)
	if err != nil {
		return nil, fmt.Errorf("%w: SKILL.md missing or unreadable", ErrInvalidSkillPayload)
	}

	// Light parse: extract slug and name, to derive the on-disk identifier
	// before full validation. frontmatter.name is a free-form display label
	// (may be CJK / uppercase / contain spaces — see validateFrontmatterName)
	// while frontmatter.slug is the URL-safe identifier; we prefer the slug
	// when present, falling back to name for skills that don't declare one.
	var lightFM struct {
		Name string `yaml:"name"`
		Slug string `yaml:"slug"`
	}
	if _, err := frontmatter.Parse(bytes.NewReader(rawMD), &lightFM, frontmatter.NewFormat("---", "---", yaml.Unmarshal)); err != nil {
		return nil, fmt.Errorf("%w: parse SKILL.md frontmatter: %v", ErrInvalidSkillPayload, err)
	}
	if lightFM.Name == "" {
		return nil, fmt.Errorf("%w: SKILL.md missing name field", ErrInvalidSkillPayload)
	}
	slug := lightFM.Slug
	if slug == "" {
		slug = lightFM.Name
	}
	if err := ValidateSkillName(slug); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSkillPayload, err)
	}

	// Serialize concurrent uploads for the same slug.
	unlock := locks.Lock(slug)
	defer unlock()

	// Builtin check: auto-installed builtins (see IsBuiltinSkill) are wiped
	// back to embed.FS contents on every daemon restart by EnsureBuiltinSkills,
	// so any user upload would silently revert. Reject up front regardless of
	// whether a global override exists or `force` is set.
	if IsBuiltinSkill(slug) {
		return nil, ErrSkillIsBuiltin
	}

	globalPath := filepath.Join(shannonDir, "skills", slug, "SKILL.md")
	// Conflict check: if a global skill with this slug exists and force=false, 409.
	// Parse both the existing and new skill so the frontend can show a compare sheet.
	if _, err := os.Stat(globalPath); err == nil && !force {
		existing, _ := loadSkillMD(globalPath, slug, SourceGlobal)
		uploaded, _ := loadSkillMD(skillFile, slug, SourceGlobal)
		conflict := &SkillConflictError{ExistingName: slug}
		if existing != nil {
			conflict.ExistingDescription = existing.Description
			conflict.ExistingPrompt = TruncatePromptPreview(existing.Prompt)
		}
		if uploaded != nil {
			conflict.NewDescription = uploaded.Description
			conflict.NewPrompt = TruncatePromptPreview(uploaded.Prompt)
		}
		return nil, conflict
	}

	// Full validation (name, description, frontmatter integrity).
	skill, err := loadSkillMD(skillFile, slug, SourceGlobal)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSkillPayload, err)
	}
	skill.InstallSource = InstallSourceLocal

	// Atomic rename into place. For force overwrite, move the existing
	// directory aside (rename-to-bak) before renaming the new one in, then
	// remove the backup on success. This narrows the crash window to a
	// single rename syscall — if the daemon dies between the two renames
	// the user is briefly left without a skill but the backup is intact
	// and recoverable, vs. the previous RemoveAll-then-Rename which would
	// destroy the prior install before the new one was in place.
	destDir := filepath.Join(shannonDir, "skills", slug)
	if err := os.MkdirAll(filepath.Dir(destDir), 0700); err != nil {
		return nil, fmt.Errorf("create skills dir: %w", err)
	}
	var backupDir string
	if force {
		if _, err := os.Stat(destDir); err == nil {
			backupDir = fmt.Sprintf("%s.bak.%d", destDir, time.Now().UnixNano())
			if err := os.Rename(destDir, backupDir); err != nil {
				return nil, fmt.Errorf("backup existing skill: %w", err)
			}
		}
	}
	if err := os.Rename(skillRoot, destDir); err != nil {
		// Restore the backup so the user isn't left without a skill on
		// a rename failure (full disk, permissions, cross-device, etc.).
		if backupDir != "" {
			_ = os.Rename(backupDir, destDir)
		}
		return nil, fmt.Errorf("install rename: %w", err)
	}
	if backupDir != "" {
		os.RemoveAll(backupDir)
	}
	return skill, nil
}

// installFromGit clones entry.Repo into a temp dir, selects the right
// subtree (entry.RepoPath or the clone root), and stages a clean copy
// into stageDir. Git subprocesses run under ctx so cancellation
// propagates. Payload-level validation errors (symlink, walk failure)
// are wrapped as ErrInvalidSkillPayload so the handler maps them to
// 422, matching the design doc's error matrix.
func installFromGit(ctx context.Context, entry MarketplaceEntry, stageDir, tmpRoot string) error {
	// Retry the git clone portion on transient failures. Matches the
	// downloadable-skills path (see installFromRepo): same flakes hit both
	// transports (Tokyo/CN networks intermittently lose reachability to
	// github.com / other hosts during clone). Staging/validation is local
	// and is NOT retried — only the network operations are. cloneDir is
	// recreated each attempt because `git clone` rejects a non-empty target.
	cloneDir, err := gitCloneWithRetry(ctx, entry, tmpRoot)
	if err != nil {
		return err
	}
	defer os.RemoveAll(cloneDir)

	srcDir := cloneDir
	if entry.RepoPath != "" {
		srcDir = filepath.Join(cloneDir, entry.RepoPath)
	}

	// Remove the empty stageDir MkdirTemp created before calling this
	// function; stageCleanPayload recreates it.
	os.RemoveAll(stageDir)
	if err := stageCleanPayload(srcDir, stageDir); err != nil {
		// Payload-level failures (symlinks, walk errors) are client-
		// visible invalid payloads, not upstream or internal errors.
		return fmt.Errorf("%w: %v", ErrInvalidSkillPayload, err)
	}
	return nil
}

// gitCloneWithRetry performs the marketplace git clone (+ optional sparse
// checkout) with retry/backoff identical to installFromRepo. Returns the
// populated cloneDir on success; cleans up the temp dir on every failed
// attempt so the next attempt starts from a fresh empty directory.
// Respects ctx cancellation during the inter-attempt sleep.
func gitCloneWithRetry(ctx context.Context, entry MarketplaceEntry, tmpRoot string) (string, error) {
	ref := entry.Ref
	if ref == "" {
		ref = "main"
	}
	var lastErr error
	for attempt := 0; attempt < installFromRepoMaxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * time.Second):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		cloneDir, err := os.MkdirTemp(tmpRoot, "skill-clone-"+entry.Slug+"-*")
		if err != nil {
			// Local MkdirTemp failure is not a network flake — fail fast.
			return "", fmt.Errorf("create clone dir: %w", err)
		}
		if entry.RepoPath == "" {
			err = runGitCtx(ctx, cloneDir, "clone", "--depth=1", "--branch", ref, entry.Repo, ".")
			if err != nil {
				os.RemoveAll(cloneDir)
				lastErr = fmt.Errorf("%w: git clone: %v", ErrMarketplaceUpstreamFailure, err)
				continue
			}
		} else {
			err = runGitCtx(ctx, cloneDir, "clone", "--depth=1", "--filter=blob:none", "--sparse", "--branch", ref, entry.Repo, ".")
			if err != nil {
				os.RemoveAll(cloneDir)
				lastErr = fmt.Errorf("%w: git clone: %v", ErrMarketplaceUpstreamFailure, err)
				continue
			}
			err = runGitCtx(ctx, cloneDir, "sparse-checkout", "set", entry.RepoPath)
			if err != nil {
				os.RemoveAll(cloneDir)
				lastErr = fmt.Errorf("%w: git sparse-checkout: %v", ErrMarketplaceUpstreamFailure, err)
				continue
			}
		}
		return cloneDir, nil
	}
	return "", lastErr
}

// installFromZip fetches entry.DownloadURL and extracts it into stageDir
// via extractZipToSkill. HTTP failures surface as
// ErrMarketplaceUpstreamFailure so the handler maps to 502. Uses the
// caller's ctx directly so client disconnect or daemon shutdown aborts
// the in-flight download. marketplaceDownloadClient provides a 2-minute
// safety ceiling when ctx has no deadline.
func installFromZip(ctx context.Context, entry MarketplaceEntry, stageDir string) error {
	// Single attempt (no doGETWithRetry): the zip download uses the 2-minute
	// marketplaceDownloadClient, so multiplying it by retries would let a
	// hanging upstream stall a user's Install for many minutes — and the
	// per-request retry policy isn't reachable here anyway. Transient
	// resilience already covers the catalog GETs (incl. install's slug
	// resolution); a failed download surfaces as ErrMarketplaceUpstreamFailure
	// (→502) and the user can reinstall.
	req, err := http.NewRequestWithContext(ctx, "GET", entry.DownloadURL, nil)
	if err != nil {
		return fmt.Errorf("%w: build download request: %v", ErrMarketplaceUpstreamFailure, err)
	}
	resp, err := marketplaceDownloadClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: download: %v", ErrMarketplaceUpstreamFailure, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%w: download status %d", ErrMarketplaceUpstreamFailure, resp.StatusCode)
	}

	if err := extractZipToSkill(resp.Body, stageDir); err != nil {
		// Payload-level failures (symlink, zip-slip, zip-bomb, bad
		// archive) are client-visible invalid payloads. Mapped to 422.
		return fmt.Errorf("%w: %v", ErrInvalidSkillPayload, err)
	}
	return nil
}

// Caps for zip-based skill installs. These are intentionally generous —
// effectively "no limit" for any realistic local skill — because the data
// lives on the user's own disk. They are NOT arbitrary size limits but
// memory/zip-bomb backstops: extractZipToSkill buffers the compressed
// payload (and each entry) in RAM via io.ReadAll, so the compressed cap
// bounds peak memory and the uncompressed cap guards against decompression
// bombs (a tiny archive expanding to fill disk/RAM). Variables (not consts)
// so tests can set a small cap to exercise the guard cheaply.
//
// Caveat (consciously accepted): these bound a SINGLE upload. The daemon does
// not gate aggregate in-flight uploads — slugLocks only serializes the same
// slug — so N concurrent uploads of distinct slugs can each pin ~1 GiB of RAM
// and OOM the daemon. Acceptable here because the server is localhost-only and
// the Kocoro Desktop client uploads serially (one modal, button disabled while
// busy); revisit with a global in-flight gate if a client ever fans these out.
var (
	maxZipCompressedBytes   int64 = 1 * 1024 * 1024 * 1024 // 1 GiB (RAM backstop)
	maxZipUncompressedBytes int64 = 1 * 1024 * 1024 * 1024 // 1 GiB (zip-bomb guard)
)

// extractZipToSkill reads a zip archive from body and extracts it into
// destDir, applying the same exclusion, symlink rejection, and mode
// preservation rules as stageCleanPayload. It is the zip-transport
// equivalent of (git clone + stageCleanPayload) collapsed into one
// step because a zip archive is already a self-contained payload.
//
// Rejections (all with destDir cleanup):
//   - Compressed body > maxZipCompressedBytes
//   - Sum of uncompressed entry sizes > maxZipUncompressedBytes (zip bomb guard)
//   - Any entry with a symlink mode bit
//   - Any entry whose cleaned path escapes destDir (zip-slip)
//   - Any entry whose first path segment is excluded git metadata
func extractZipToSkill(body io.Reader, destDir string) error {
	// Read the entire compressed payload into memory through a hard cap.
	// archive/zip requires a ReaderAt, so we buffer the body.
	limited := io.LimitReader(body, maxZipCompressedBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("read zip body: %w", err)
	}
	if int64(len(raw)) > maxZipCompressedBytes {
		return fmt.Errorf("%w: compressed payload exceeds %d bytes", ErrZipTooLarge, maxZipCompressedBytes)
	}

	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return fmt.Errorf("parse zip: %w", err)
	}

	excluded := map[string]bool{
		".git":           true,
		".github":        true,
		".gitignore":     true,
		".gitattributes": true,
		"__MACOSX":       true, // macOS Finder adds this to ZIPs
	}

	if err := os.MkdirAll(destDir, 0700); err != nil {
		return fmt.Errorf("create dest dir: %w", err)
	}

	// All work happens inside a closure so any failure triggers cleanup
	// via the single RemoveAll below.
	extractErr := func() error {
		absDest, err := filepath.Abs(destDir)
		if err != nil {
			return fmt.Errorf("resolve dest dir: %w", err)
		}

		// Zip-bomb guard: bound the TOTAL actual bytes decompressed
		// across all entries, using a LimitReader that counts real
		// bytes read — not the attacker-controlled UncompressedSize64
		// in the zip header. This prevents a malicious archive from
		// declaring 0-byte entries and then streaming gigabytes into
		// memory via ReadAll.
		remaining := maxZipUncompressedBytes

		for _, f := range zr.File {
			// Symlink rejection.
			if f.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("unsupported symlink in skill payload: %s", f.Name)
			}

			// Clean the path and verify it stays within destDir.
			// filepath.Clean normalizes ../ which would otherwise escape.
			cleanRel := filepath.Clean(f.Name)
			if cleanRel == "." || cleanRel == "" {
				continue
			}
			destPath := filepath.Join(absDest, cleanRel)
			absPath, err := filepath.Abs(destPath)
			if err != nil {
				return fmt.Errorf("resolve entry path %q: %w", f.Name, err)
			}
			if absPath != absDest && !strings.HasPrefix(absPath, absDest+string(filepath.Separator)) {
				return fmt.Errorf("zip entry %q escapes dest dir", f.Name)
			}

			// Exclusion check: any path segment matches.
			segments := strings.Split(cleanRel, string(filepath.Separator))
			skip := false
			for _, seg := range segments {
				if excluded[seg] {
					skip = true
					break
				}
			}
			if skip {
				continue
			}

			if f.FileInfo().IsDir() {
				if err := os.MkdirAll(destPath, 0700); err != nil {
					return fmt.Errorf("mkdir %q: %w", destPath, err)
				}
				continue
			}

			// Ensure parent exists (zip entries may list files before dirs).
			if err := os.MkdirAll(filepath.Dir(destPath), 0700); err != nil {
				return fmt.Errorf("mkdir parent of %q: %w", destPath, err)
			}

			// Read with a per-entry budget of (remaining+1) bytes.
			// If we can read even 1 byte past the budget, the archive
			// exceeds the uncompressed cap. This tracks ACTUAL bytes
			// decompressed, not declared size.
			rc, err := f.Open()
			if err != nil {
				return fmt.Errorf("open zip entry %q: %w", f.Name, err)
			}
			content, err := io.ReadAll(io.LimitReader(rc, remaining+1))
			rc.Close()
			if err != nil {
				return fmt.Errorf("read zip entry %q: %w", f.Name, err)
			}
			if int64(len(content)) > remaining {
				return fmt.Errorf("%w: uncompressed size exceeds %d bytes", ErrZipTooLarge, maxZipUncompressedBytes)
			}
			remaining -= int64(len(content))

			srcMode := f.Mode().Perm() & 0755
			if srcMode&0400 == 0 {
				srcMode |= 0400
			}
			if err := os.WriteFile(destPath, content, srcMode); err != nil {
				return fmt.Errorf("write %q: %w", destPath, err)
			}
			if err := os.Chmod(destPath, srcMode); err != nil {
				return fmt.Errorf("chmod %q: %w", destPath, err)
			}
		}
		return nil
	}()

	if extractErr != nil {
		os.RemoveAll(destDir)
		return extractErr
	}
	return nil
}

// stageCleanPayload walks src and copies every regular file into dst,
// excluding git metadata (.git/, .github/, .gitignore, .gitattributes) at
// any depth. Symlinks are rejected unconditionally: if the walk encounters
// one, the function removes dst (cleaning up any partial copy) and returns
// a 422-worthy error. See design doc §Install flow step 9.
//
// Exclusions match on the base name of any path segment, so nested .git dirs
// are also skipped.
//
// File modes are preserved from the source (masked to 0755 to strip any
// setuid/setgid/sticky bits), so shipped helper scripts keep their
// executable bit — this matters for community skills like
// self-improving-agent that ship scripts/activator.sh.
func stageCleanPayload(src, dst string) error {
	excluded := map[string]bool{
		".git":           true,
		".github":        true,
		".gitignore":     true,
		".gitattributes": true,
		"__MACOSX":       true,
	}

	if err := os.MkdirAll(dst, 0700); err != nil {
		return fmt.Errorf("create stage dir: %w", err)
	}

	walkErr := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == src {
			return nil
		}

		// Reject symlinks outright. WalkDir gives us the lstat'd entry via
		// d.Type(), which preserves the Symlink bit.
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsupported symlink in skill payload: %s", path)
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		// Exclude if any path segment matches.
		for _, seg := range strings.Split(rel, string(filepath.Separator)) {
			if excluded[seg] {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}

		destPath := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(destPath, 0700)
		}

		// Preserve source file mode so shipped helper scripts keep their
		// executable bit. Mask to 0755 so no file can become setuid/setgid/
		// sticky via install, and ensure owner-read is always set.
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		srcMode := info.Mode().Perm() & 0755
		if srcMode&0400 == 0 {
			srcMode |= 0400
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(destPath, content, srcMode); err != nil {
			return err
		}
		// os.WriteFile respects the umask on some platforms; chmod to
		// guarantee the requested mode lands on disk.
		return os.Chmod(destPath, srcMode)
	})

	if walkErr != nil {
		os.RemoveAll(dst)
		return walkErr
	}
	return nil
}

// SlugLocks is a map of per-slug mutexes. The outer mutex protects map access;
// each per-slug lock serializes install/uninstall/usage-check operations for
// that slug only. Different slugs never block each other.
//
// Usage:
//
//	unlock := locks.Lock("my-skill")
//	defer unlock()
type SlugLocks struct {
	outer   sync.Mutex
	perSlug map[string]*sync.Mutex
}

// NewSlugLocks creates an empty SlugLocks.
func NewSlugLocks() *SlugLocks {
	return &SlugLocks{perSlug: make(map[string]*sync.Mutex)}
}

// Lock acquires the per-slug mutex and returns a function that releases it.
// The returned function is safe to call exactly once (typically via defer).
func (l *SlugLocks) Lock(slug string) func() {
	l.outer.Lock()
	m, ok := l.perSlug[slug]
	if !ok {
		m = &sync.Mutex{}
		l.perSlug[slug] = m
	}
	l.outer.Unlock()

	m.Lock()
	return m.Unlock
}

// FilterSortPaginate applies the marketplace list pipeline to a raw index slice.
// Returns the page slice plus the total count of entries that matched the
// filter (used by the API response for client-side pagination controls).
//
// Pipeline:
//  1. Drop malicious entries (server-side security gate).
//  2. Apply case-insensitive substring search against name+description+author.
//  3. Sort by the requested key (downloads|stars|name); unknown keys fall
//     back to downloads desc.
//  4. Slice to the requested page. Out-of-range pages return an empty slice.
//
// Sort keys:
//   - "downloads" (default): descending by Downloads, ties broken by name asc
//   - "stars":               descending by Stars, ties broken by name asc
//   - "name":                ascending by Name
func FilterSortPaginate(entries []MarketplaceEntry, query, sortKey string, page, size int) ([]MarketplaceEntry, int) {
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 20
	}
	if size > 100 {
		size = 100
	}

	// Step 1+2: filter.
	q := strings.ToLower(strings.TrimSpace(query))
	filtered := make([]MarketplaceEntry, 0, len(entries))
	for _, e := range entries {
		if e.IsMalicious() {
			continue
		}
		if q != "" {
			hay := strings.ToLower(e.Name + " " + e.Description + " " + e.Author)
			if !strings.Contains(hay, q) {
				continue
			}
		}
		filtered = append(filtered, e)
	}

	// Step 3: sort.
	switch sortKey {
	case "name":
		sort.SliceStable(filtered, func(i, j int) bool {
			return filtered[i].Name < filtered[j].Name
		})
	case "stars":
		sort.SliceStable(filtered, func(i, j int) bool {
			if filtered[i].Stars != filtered[j].Stars {
				return filtered[i].Stars > filtered[j].Stars
			}
			return filtered[i].Name < filtered[j].Name
		})
	default: // "downloads" and unknown
		sort.SliceStable(filtered, func(i, j int) bool {
			if filtered[i].Downloads != filtered[j].Downloads {
				return filtered[i].Downloads > filtered[j].Downloads
			}
			return filtered[i].Name < filtered[j].Name
		})
	}

	total := len(filtered)

	// Step 4: paginate.
	start := (page - 1) * size
	if start >= total {
		return []MarketplaceEntry{}, total
	}
	end := start + size
	if end > total {
		end = total
	}
	return filtered[start:end], total
}

func (c *MarketplaceClient) fetch(ctx context.Context) (*RegistryIndex, error) {
	resp, err := doGETWithRetry(ctx, c.http, c.url, c.maxAttempts, c.retryBase, marketplaceCatalogRetryBudget)
	if err != nil {
		return nil, fmt.Errorf("fetch registry: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch registry: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024)) // 10 MB cap
	if err != nil {
		return nil, fmt.Errorf("read registry body: %w", err)
	}
	var idx RegistryIndex
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil, fmt.Errorf("parse registry: %w", err)
	}
	return &idx, nil
}

// ---- ClawHub live API source -------------------------------------------

// ClawHub /api/v1/skills response shapes. Only the fields we map are declared.
type clawhubListResp struct {
	Items      []clawhubItem `json:"items"`
	NextCursor *string       `json:"nextCursor"`
}

type clawhubItem struct {
	Slug          string            `json:"slug"`
	DisplayName   string            `json:"displayName"`
	Summary       string            `json:"summary"`
	Topics        []string          `json:"topics"`
	Tags          map[string]string `json:"tags"`
	Stats         clawhubStats      `json:"stats"`
	LatestVersion clawhubVersion    `json:"latestVersion"`
}

type clawhubStats struct {
	Downloads       int `json:"downloads"`
	InstallsAllTime int `json:"installsAllTime"`
	Stars           int `json:"stars"`
}

type clawhubVersion struct {
	Version string  `json:"version"`
	License *string `json:"license"`
}

// clawhubSort maps the desktop sort keys to ClawHub's supported /api/v1/skills
// sort values. Unknown keys fall back to ClawHub's default ordering.
func clawhubSort(sort string) string {
	switch sort {
	case "recommended", "":
		return "recommended"
	case "downloads":
		return "downloads"
	case "stars":
		return "stars"
	case "trending":
		return "trending"
	case "newest", "createdAt":
		return "createdAt"
	case "updated":
		return "updated"
	default:
		return ""
	}
}

// clawhubSearchResp / clawhubSearchResult model the dedicated full-text search
// endpoint (/api/v1/search), whose shape differs from the list endpoint and
// which actually honors the query (the list endpoint's ?q= is ignored upstream).
type clawhubSearchResp struct {
	Results []clawhubSearchResult `json:"results"`
}

type clawhubSearchResult struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"displayName"`
	Summary     string `json:"summary"`
	Version     string `json:"version"`
	Downloads   int    `json:"downloads"`
	UpdatedAt   int64  `json:"updatedAt"`
	Owner       struct {
		Handle string `json:"handle"`
	} `json:"owner"`
}

// clawhubSearchPool is how many relevance-ranked search hits to fetch so that
// client-side re-sorting (by downloads / recency) is meaningful, not just a
// reorder of the top few.
const clawhubSearchPool = 100

// FetchClawHubPage returns one page of the ClawHub catalog. The full catalog is
// ~12k skills, so we never cache it whole — each request is proxied straight to
// ClawHub.
//
//   - With a query: hit /api/v1/search (relevance-ranked; the list endpoint's
//     ?q= is ignored by ClawHub). Search is not cursor-paginated, so next is "".
//   - Without a query: hit /api/v1/skills with sort + cursor (Load-more paging).
func (c *MarketplaceClient) FetchClawHubPage(ctx context.Context, q, sortKey, cursor string, limit int) ([]MarketplaceEntry, string, error) {
	if c.clawhubBase == "" {
		return nil, "", errors.New("not a clawhub client")
	}
	// <=0 → default page size; oversize → clamp to the ceiling (rather than
	// silently snapping back to the default, which made size=201 yield 20).
	if limit <= 0 {
		limit = 20
	} else if limit > 200 {
		limit = 200
	}

	if q != "" {
		// ClawHub search is relevance-ranked and ignores sort. Fetch a larger
		// pool and re-order it ourselves so a chosen sort still applies within a
		// query/category. Only downloads and recency are available on search
		// results; other sort keys keep the relevance order.
		pool := limit
		if pool < clawhubSearchPool {
			pool = clawhubSearchPool
		}
		u := fmt.Sprintf("%s/api/v1/search?limit=%d&q=%s", c.clawhubBase, pool, url.QueryEscape(q))
		var sr clawhubSearchResp
		if err := c.getJSON(ctx, u, &sr); err != nil {
			return nil, "", err
		}
		switch sortKey {
		case "downloads":
			sort.SliceStable(sr.Results, func(i, j int) bool {
				return sr.Results[i].Downloads > sr.Results[j].Downloads
			})
		case "newest", "createdAt", "updated":
			sort.SliceStable(sr.Results, func(i, j int) bool {
				return sr.Results[i].UpdatedAt > sr.Results[j].UpdatedAt
			})
		}
		// Search has no cursor pagination: this pool IS the complete result set
		// for the query, so we return all of it (next cursor "") rather than
		// capping to limit — capping would hide most of a category with no way
		// to load more.
		entries := make([]MarketplaceEntry, 0, len(sr.Results))
		for _, r := range sr.Results {
			entries = append(entries, c.clawhubSearchToEntry(r))
		}
		return entries, "", nil
	}

	// nonSuspiciousOnly lets ClawHub drop flagged skills server-side — the
	// closest equivalent to the registry path's IsMalicious() gate, since
	// ClawHub list items carry no scan data for us to filter on locally.
	u := fmt.Sprintf("%s/api/v1/skills?limit=%d&nonSuspiciousOnly=true", c.clawhubBase, limit)
	if s := clawhubSort(sortKey); s != "" {
		u += "&sort=" + url.QueryEscape(s)
	}
	if cursor != "" {
		u += "&cursor=" + url.QueryEscape(cursor)
	}
	var lr clawhubListResp
	if err := c.getJSON(ctx, u, &lr); err != nil {
		return nil, "", err
	}
	entries := make([]MarketplaceEntry, 0, len(lr.Items))
	for _, it := range lr.Items {
		entries = append(entries, c.clawhubItemToEntry(it))
	}
	next := ""
	if lr.NextCursor != nil {
		next = *lr.NextCursor
	}
	return entries, next, nil
}

func (c *MarketplaceClient) clawhubSearchToEntry(r clawhubSearchResult) MarketplaceEntry {
	name := r.DisplayName
	if name == "" {
		name = r.Slug
	}
	e := MarketplaceEntry{
		Slug:        r.Slug,
		Name:        name,
		Description: r.Summary,
		Author:      r.Owner.Handle,
		DownloadURL: c.ClawHubDownloadURL(r.Slug, r.Owner.Handle),
		Downloads:   r.Downloads,
		Version:     r.Version,
	}
	if r.Owner.Handle != "" {
		e.Homepage = fmt.Sprintf("%s/%s/%s", c.clawhubBase, r.Owner.Handle, r.Slug)
	}
	return e
}

// ClawHubDownloadURL is the deterministic zip artifact URL for a slug. Lets the
// install/detail handlers build an entry without a full catalog lookup. owner
// disambiguates slugs shared by multiple publishers (ClawHub returns 409 for a
// bare ambiguous slug); pass "" when unknown.
func (c *MarketplaceClient) ClawHubDownloadURL(slug, owner string) string {
	u := fmt.Sprintf("%s/api/v1/download?slug=%s", c.clawhubBase, url.QueryEscape(slug))
	if owner != "" {
		u += "&owner=" + url.QueryEscape(owner)
	}
	return u
}

func (c *MarketplaceClient) clawhubItemToEntry(it clawhubItem) MarketplaceEntry {
	name := it.DisplayName
	if name == "" {
		name = it.Slug
	}
	version := it.Tags["latest"]
	if version == "" {
		version = it.LatestVersion.Version
	}
	license := ""
	if it.LatestVersion.License != nil {
		license = *it.LatestVersion.License
	}
	return MarketplaceEntry{
		Slug:        it.Slug,
		Name:        name,
		Description: it.Summary,
		License:     license,
		// ClawHub serves skills as zip artifacts; install uses this download URL.
		// Browse items carry no owner handle, so the slug is left bare here.
		DownloadURL: c.ClawHubDownloadURL(it.Slug, ""),
		Downloads:   it.Stats.Downloads,
		Stars:       it.Stats.Stars,
		Version:     version,
		Tags:        it.Topics,
	}
}

// ClawHubDetail is a fully-built marketplace entry for one slug plus the SKILL.md
// body usable as a pre-install preview. Built entirely from ClawHub's detail
// endpoint, so the detail handler needs no cached catalog.
type ClawHubDetail struct {
	Entry   MarketplaceEntry
	Preview string
}

// FetchClawHubDetail loads a single skill's detail from ClawHub and assembles a
// MarketplaceEntry (including owner handle → author + homepage, and the full
// SKILL.md as preview). Only valid on a ClawHub-sourced client.
func (c *MarketplaceClient) FetchClawHubDetail(ctx context.Context, slug, owner string) (*ClawHubDetail, error) {
	if c.clawhubBase == "" {
		return nil, errors.New("not a clawhub client")
	}
	u := fmt.Sprintf("%s/api/v1/skills/%s", c.clawhubBase, url.PathEscape(slug))
	if owner != "" {
		u += "?owner=" + url.QueryEscape(owner)
	}
	var dr struct {
		Skill struct {
			Slug        string            `json:"slug"`
			DisplayName string            `json:"displayName"`
			Summary     string            `json:"summary"`
			Description string            `json:"description"`
			Topics      []string          `json:"topics"`
			Tags        map[string]string `json:"tags"`
			Stats       clawhubStats      `json:"stats"`
		} `json:"skill"`
		Owner struct {
			Handle string `json:"handle"`
		} `json:"owner"`
		LatestVersion clawhubVersion `json:"latestVersion"`
	}
	if err := c.getJSON(ctx, u, &dr); err != nil {
		return nil, err
	}
	name := dr.Skill.DisplayName
	if name == "" {
		name = slug
	}
	version := dr.Skill.Tags["latest"]
	if version == "" {
		version = dr.LatestVersion.Version
	}
	license := ""
	if dr.LatestVersion.License != nil {
		license = *dr.LatestVersion.License
	}
	// Use the canonical slug ClawHub resolved the request to (the requested
	// path may be a non-canonical alias), falling back to the request slug.
	resolvedSlug := dr.Skill.Slug
	if resolvedSlug == "" {
		resolvedSlug = slug
	}
	entry := MarketplaceEntry{
		Slug:        resolvedSlug,
		Name:        name,
		Description: dr.Skill.Summary,
		Author:      dr.Owner.Handle,
		License:     license,
		DownloadURL: c.ClawHubDownloadURL(resolvedSlug, dr.Owner.Handle),
		Downloads:   dr.Skill.Stats.Downloads,
		Stars:       dr.Skill.Stats.Stars,
		Version:     version,
		Tags:        dr.Skill.Topics,
	}
	if dr.Owner.Handle != "" {
		entry.Homepage = fmt.Sprintf("%s/%s/%s", c.clawhubBase, dr.Owner.Handle, resolvedSlug)
	}
	return &ClawHubDetail{Entry: entry, Preview: dr.Skill.Description}, nil
}

// ClawHubFile is one file entry in a skill version's manifest.
type ClawHubFile struct {
	Path        string `json:"path"`
	Size        int    `json:"size"`
	ContentType string `json:"content_type"`
}

// resolveClawHubVersion returns version if non-empty, else the skill's latest
// version (via the detail endpoint). owner disambiguates shared slugs.
func (c *MarketplaceClient) resolveClawHubVersion(ctx context.Context, slug, version, owner string) (string, error) {
	if version != "" {
		return version, nil
	}
	d, err := c.FetchClawHubDetail(ctx, slug, owner)
	if err != nil {
		return "", err
	}
	return d.Entry.Version, nil
}

// FetchClawHubFiles returns the file manifest for a skill version (the resolved
// version is returned too, useful when the caller passed "" for latest).
func (c *MarketplaceClient) FetchClawHubFiles(ctx context.Context, slug, version, owner string) (string, []ClawHubFile, error) {
	if c.clawhubBase == "" {
		return "", nil, errors.New("not a clawhub client")
	}
	ver, err := c.resolveClawHubVersion(ctx, slug, version, owner)
	if err != nil {
		return "", nil, err
	}
	u := fmt.Sprintf("%s/api/v1/skills/%s/versions/%s", c.clawhubBase, url.PathEscape(slug), url.PathEscape(ver))
	if owner != "" {
		u += "?owner=" + url.QueryEscape(owner)
	}
	var vr struct {
		Version struct {
			Files []struct {
				Path        string `json:"path"`
				Size        int    `json:"size"`
				ContentType string `json:"contentType"`
			} `json:"files"`
		} `json:"version"`
	}
	if err := c.getJSON(ctx, u, &vr); err != nil {
		return "", nil, err
	}
	files := make([]ClawHubFile, 0, len(vr.Version.Files))
	for _, f := range vr.Version.Files {
		files = append(files, ClawHubFile{Path: f.Path, Size: f.Size, ContentType: f.ContentType})
	}
	return ver, files, nil
}

// FetchClawHubFile returns the raw text content of one file in a skill version.
func (c *MarketplaceClient) FetchClawHubFile(ctx context.Context, slug, version, path, owner string) (string, error) {
	if c.clawhubBase == "" {
		return "", errors.New("not a clawhub client")
	}
	ver, err := c.resolveClawHubVersion(ctx, slug, version, owner)
	if err != nil {
		return "", err
	}
	u := fmt.Sprintf("%s/api/v1/skills/%s/file?path=%s&version=%s",
		c.clawhubBase, url.PathEscape(slug), url.QueryEscape(path), url.QueryEscape(ver))
	if owner != "" {
		u += "&owner=" + url.QueryEscape(owner)
	}
	return c.getText(ctx, u)
}

// getText performs a GET and returns the raw response body as a string. Used for
// file content (ClawHub caps single files at 200KB); 1 MB ceiling as a guard.
func (c *MarketplaceClient) getText(ctx context.Context, rawURL string) (string, error) {
	resp, err := doGETWithRetry(ctx, c.http, rawURL, c.maxAttempts, c.retryBase, marketplaceCatalogRetryBudget)
	if err != nil {
		return "", fmt.Errorf("fetch clawhub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("fetch clawhub: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
	if err != nil {
		return "", fmt.Errorf("read clawhub body: %w", err)
	}
	return string(body), nil
}

// getJSON performs a GET and decodes the JSON body into v, sharing the HTTP
// client, body cap, and error wrapping with the registry fetch path.
func (c *MarketplaceClient) getJSON(ctx context.Context, rawURL string, v any) error {
	resp, err := doGETWithRetry(ctx, c.http, rawURL, c.maxAttempts, c.retryBase, marketplaceCatalogRetryBudget)
	if err != nil {
		return fmt.Errorf("fetch clawhub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("fetch clawhub: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024)) // 10 MB cap
	if err != nil {
		return fmt.Errorf("read clawhub body: %w", err)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("parse clawhub: %w", err)
	}
	return nil
}
