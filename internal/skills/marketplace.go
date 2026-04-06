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
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
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

	// staleCooldown bounds how often we re-attempt an upstream fetch
	// while in stale mode. Exposed as a field (not a constructor arg)
	// so tests can set a short cooldown directly.
	staleCooldown time.Duration

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
	}
}

// Load returns the current registry index, refetching when the cache is
// empty or past TTL. See the type doc for stale-on-error semantics.
func (c *MarketplaceClient) Load(ctx context.Context) (*RegistryIndex, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Fresh cache → return immediately.
	if c.cache != nil && time.Since(c.fetched) < c.ttl {
		c.stale = false
		return c.cache, nil
	}

	// Stale-mode cooldown in effect → keep serving stale without
	// re-attempting the upstream fetch. Prevents retry storms during
	// registry outages.
	if c.cache != nil && !c.retryAfter.IsZero() && time.Now().Before(c.retryAfter) {
		c.stale = true
		return c.cache, nil
	}

	idx, err := c.fetch(ctx)
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
)

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
	parsed, err := loadSkillMD(skillFile, entry.Slug, "marketplace")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSkillPayload, err)
	}
	if parsed.Name != entry.Slug {
		return fmt.Errorf("%w: frontmatter name %q != slug %q", ErrInvalidSkillPayload, parsed.Name, entry.Slug)
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

// installFromGit clones entry.Repo into a temp dir, selects the right
// subtree (entry.RepoPath or the clone root), and stages a clean copy
// into stageDir. Git subprocesses run under ctx so cancellation
// propagates. Payload-level validation errors (symlink, walk failure)
// are wrapped as ErrInvalidSkillPayload so the handler maps them to
// 422, matching the design doc's error matrix.
func installFromGit(ctx context.Context, entry MarketplaceEntry, stageDir, tmpRoot string) error {
	cloneDir, err := os.MkdirTemp(tmpRoot, "skill-clone-"+entry.Slug+"-*")
	if err != nil {
		return fmt.Errorf("create clone dir: %w", err)
	}
	defer os.RemoveAll(cloneDir)

	ref := entry.Ref
	if ref == "" {
		ref = "main"
	}
	if entry.RepoPath == "" {
		if err := runGitCtx(ctx, cloneDir, "clone", "--depth=1", "--branch", ref, entry.Repo, "."); err != nil {
			return fmt.Errorf("%w: git clone: %v", ErrMarketplaceUpstreamFailure, err)
		}
	} else {
		if err := runGitCtx(ctx, cloneDir, "clone", "--depth=1", "--filter=blob:none", "--sparse", "--branch", ref, entry.Repo, "."); err != nil {
			return fmt.Errorf("%w: git clone: %v", ErrMarketplaceUpstreamFailure, err)
		}
		if err := runGitCtx(ctx, cloneDir, "sparse-checkout", "set", entry.RepoPath); err != nil {
			return fmt.Errorf("%w: git sparse-checkout: %v", ErrMarketplaceUpstreamFailure, err)
		}
	}

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

// installFromZip fetches entry.DownloadURL and extracts it into stageDir
// via extractZipToSkill. HTTP failures surface as
// ErrMarketplaceUpstreamFailure so the handler maps to 502. Uses the
// caller's ctx directly so client disconnect or daemon shutdown aborts
// the in-flight download. marketplaceDownloadClient provides a 2-minute
// safety ceiling when ctx has no deadline.
func installFromZip(ctx context.Context, entry MarketplaceEntry, stageDir string) error {
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

// Caps for zip-based skill installs. 50 MB is more than generous for
// any realistic skill (ontology was 12 KB); 200 MB uncompressed guards
// against zip bombs. Variables (not consts) so tests can set a small
// cap to exercise the guard without allocating 200 MB of in-memory
// content.
var (
	maxZipCompressedBytes   int64 = 50 * 1024 * 1024
	maxZipUncompressedBytes int64 = 200 * 1024 * 1024
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
		return fmt.Errorf("zip payload exceeds %d bytes", maxZipCompressedBytes)
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
				return fmt.Errorf("zip uncompressed size exceeds %d bytes", maxZipUncompressedBytes)
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
	req, err := http.NewRequestWithContext(ctx, "GET", c.url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := c.http.Do(req)
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
