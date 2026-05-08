package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

type ManifestFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Sha256 string `json:"sha256"`
}

type Manifest struct {
	BundleTs        string         `json:"bundle_ts"`
	BundleVersion   string         `json:"bundle_version"`
	SizeBytes       int64          `json:"size_bytes"`
	IntegritySha256 string         `json:"integrity_sha256"`
	Files           []ManifestFile `json:"files"`
}

// Puller drives the periodic bundle download cycle in cloud mode.
//   - cfg supplies BundleRoot, Endpoint, APIKey.
//   - sidecar may be nil; reload notification is wired in a later task.
//   - audit may be nil; events are dropped silently when so.
type Puller struct {
	cfg     Config
	sidecar *Sidecar
	audit   AuditLogger
	httpc   *http.Client
}

func NewPuller(cfg Config, sidecar *Sidecar, audit AuditLogger) *Puller {
	return &Puller{
		cfg:     cfg,
		sidecar: sidecar,
		audit:   audit,
		httpc:   &http.Client{Timeout: 60 * time.Second},
	}
}

// versionInRange enforces [0.4.0, 0.7.0). Producers guarantee additive minor
// bumps in this range; breaking schema changes must move to 0.7.0+ to trip
// this gate. Hand-rolled (no semver dep) because the constraint is fixed and
// trivially encodable as integer triplets.
func versionInRange(v string) bool {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) != 3 {
		return false
	}
	var maj, min, pat int
	if _, err := fmt.Sscanf(v, "%d.%d.%d", &maj, &min, &pat); err != nil {
		return false
	}
	if maj != 0 {
		return false
	}
	if min < 4 || min >= 7 {
		return false
	}
	return pat >= 0
}

// tick is one iteration of the bundle pull cycle. Steps 1-4 implemented here:
//  1. flock the bundle root (silent skip on contention)
//  2. tenant fingerprint check (wipe local bundles on switch)
//  3. fetch manifest + version range gate
//  4. compare bundle_ts against the current symlink target (no-op if same)
//
// Steps 5-8 perform sandboxed download, SHA256 verification, atomic install,
// reload, and retention.
func (p *Puller) tick(ctx context.Context) error {
	// Step 1: flock
	if err := os.MkdirAll(p.cfg.BundleRoot, 0o700); err != nil {
		return err
	}
	lockPath := filepath.Join(p.cfg.BundleRoot, "bundle.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// Contention: another caller is mid-pull; we'll get the next tick.
		return nil
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	// Step 2: tenant check (cloud-only — caller ensures provider==cloud
	// before invoking tick).
	switched, err := DetectTenantSwitch(p.cfg.BundleRoot, p.cfg.APIKey)
	if err != nil {
		return err
	}
	if switched {
		if err := os.RemoveAll(filepath.Join(p.cfg.BundleRoot, "bundles")); err != nil {
			return err
		}
		_ = os.Remove(filepath.Join(p.cfg.BundleRoot, "current"))
		if err := WriteFingerprint(p.cfg.BundleRoot, p.cfg.APIKey); err != nil {
			return err
		}
		if p.audit != nil {
			p.audit.Log("memory_tenant_switch", map[string]any{})
		}
	}

	// Step 3: fetch manifest + version gate
	mf, err := p.fetchManifest(ctx)
	if err != nil {
		return err
	}
	if !versionInRange(mf.BundleVersion) {
		return fmt.Errorf("bundle_version %q outside [0.4.0, 0.7.0)", mf.BundleVersion)
	}

	// Step 4: compare ts
	if cur := p.currentTs(); cur != "" && mf.BundleTs <= cur {
		return nil
	}

	return p.installBundle(ctx, mf)
}

func (p *Puller) fetchManifest(ctx context.Context) (*Manifest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.Endpoint+"/api/v1/memory/bundle/manifest", nil)
	if err != nil {
		return nil, err
	}
	if p.cfg.APIKey != "" {
		req.Header.Set("X-API-Key", p.cfg.APIKey)
	}
	resp, err := p.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("manifest status %d: %s", resp.StatusCode, body)
	}
	var mf Manifest
	if err := json.NewDecoder(resp.Body).Decode(&mf); err != nil {
		return nil, err
	}
	return &mf, nil
}

// currentTs reads the symlink target for <bundleRoot>/current and returns its
// basename (the bundle ts). Empty string if the symlink is absent or unreadable.
func (p *Puller) currentTs() string {
	target, err := os.Readlink(filepath.Join(p.cfg.BundleRoot, "current"))
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

func (p *Puller) installBundle(ctx context.Context, mf *Manifest) error {
	staging := filepath.Join(p.cfg.BundleRoot, "staging", mf.BundleTs)
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return err
	}
	cleanup := func() { _ = os.RemoveAll(staging) }

	for _, f := range mf.Files {
		if err := validateManifestPath(f.Path, staging); err != nil {
			sample := f.Path
			if len(sample) > 64 {
				sample = sample[:64]
			}
			if p.audit != nil {
				p.audit.Log("memory_bundle_unsafe_path", map[string]any{
					"path_sample": sample,
					"reason":      err.Error(),
				})
			}
			cleanup()
			return fmt.Errorf("unsafe manifest path %q: %w", sample, err)
		}
		if err := p.downloadFile(ctx, mf.BundleTs, f, staging); err != nil {
			cleanup()
			return err
		}
	}

	if err := p.atomicInstall(staging, mf.BundleTs); err != nil {
		cleanup()
		return err
	}
	if err := p.reloadSidecar(ctx); err != nil && p.audit != nil {
		p.audit.Log("memory_reload_failed", map[string]any{"reason": err.Error()})
	}
	p.retain(3)
	return nil
}

// validateManifestPath enforces the path-sandboxing rules from spec §4.2:
// reject empty, null bytes, absolute paths, parent traversal, and any path
// that escapes the staging dir after Clean+Join. This MUST run before any
// network I/O so a malicious manifest cannot trigger a download to an
// unauthorized location.
func validateManifestPath(rel string, stagingDir string) error {
	if rel == "" {
		return fmt.Errorf("empty path")
	}
	if strings.ContainsRune(rel, 0) {
		return fmt.Errorf("null byte in path")
	}
	if strings.HasPrefix(rel, "/") {
		return fmt.Errorf("absolute path")
	}
	cleaned := filepath.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, string(os.PathSeparator)+"..") {
		return fmt.Errorf("contains parent traversal")
	}
	abs := filepath.Join(stagingDir, cleaned)
	cleanedAbs := filepath.Clean(abs)
	prefix := filepath.Clean(stagingDir) + string(os.PathSeparator)
	if !strings.HasPrefix(cleanedAbs+string(os.PathSeparator), prefix) {
		return fmt.Errorf("escapes staging dir")
	}
	return nil
}

// downloadFile streams one manifest file into the (already-sandboxed) staging
// path while computing its SHA256. Mismatch → return error and let the caller
// clean staging.
func (p *Puller) downloadFile(ctx context.Context, ts string, f ManifestFile, staging string) error {
	target := filepath.Join(staging, filepath.Clean(f.Path))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	escapedTS := url.PathEscape(ts)
	escapedPath := escapedManifestPath(f.Path)
	fullURL := strings.TrimSuffix(p.cfg.Endpoint, "/") + "/api/v1/memory/bundle/" + escapedTS + "/" + escapedPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return err
	}
	if p.cfg.APIKey != "" {
		req.Header.Set("X-API-Key", p.cfg.APIKey)
	}
	resp, err := p.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("file %s status %d", f.Path, resp.StatusCode)
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), resp.Body); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != f.Sha256 {
		if p.audit != nil {
			p.audit.Log("memory_bundle_install_failed", map[string]any{
				"reason":      "sha256_mismatch",
				"path_sample": f.Path,
			})
		}
		return fmt.Errorf("sha256 mismatch on %s: got %s want %s", f.Path, got, f.Sha256)
	}
	return nil
}

func escapedManifestPath(raw string) string {
	parts := strings.Split(filepath.ToSlash(raw), "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		out = append(out, url.PathEscape(part))
	}
	return strings.Join(out, "/")
}

// atomicInstall renames the staging dir into bundles/<ts> and atomically
// swaps the `current` symlink. Both rename + symlink-swap are POSIX-atomic
// on the same filesystem.
func (p *Puller) atomicInstall(stagingDir, ts string) error {
	bundlesDir := filepath.Join(p.cfg.BundleRoot, "bundles")
	if err := os.MkdirAll(bundlesDir, 0o700); err != nil {
		return err
	}
	finalDir := filepath.Join(bundlesDir, ts)
	if err := os.Rename(stagingDir, finalDir); err != nil {
		return fmt.Errorf("rename staging→bundle: %w", err)
	}
	tmpLink := filepath.Join(p.cfg.BundleRoot, "current.tmp")
	_ = os.Remove(tmpLink)
	if err := os.Symlink(finalDir, tmpLink); err != nil {
		return fmt.Errorf("symlink current.tmp: %w", err)
	}
	if err := os.Rename(tmpLink, filepath.Join(p.cfg.BundleRoot, "current")); err != nil {
		return fmt.Errorf("swap current symlink: %w", err)
	}
	return nil
}

// reloadSidecar pings the sidecar's /bundle/reload endpoint via UDS so it
// picks up the new symlink target immediately. On 409 (reload_in_progress)
// retries once after 1s. Other failures are non-fatal — the sidecar's own
// poller will pick up the new bundle eventually.
func (p *Puller) reloadSidecar(ctx context.Context) error {
	if p.sidecar == nil {
		return nil
	}
	c := NewClient(p.cfg.SocketPath, 5*time.Second)
	_, err := c.Reload(ctx)
	if err != nil && strings.Contains(err.Error(), "reload_in_progress") {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
		_, err = c.Reload(ctx)
	}
	return err
}

// retain keeps the newest `keep` bundle dirs by ts plus the current symlink
// target (defensive). Best-effort — failures logged but not fatal.
func (p *Puller) retain(keep int) {
	bundlesDir := filepath.Join(p.cfg.BundleRoot, "bundles")
	entries, err := os.ReadDir(bundlesDir)
	if err != nil {
		return
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	if len(dirs) <= keep {
		return
	}
	currentTarget := p.currentTs()
	keepSet := map[string]bool{}
	for i, d := range dirs {
		if i < keep {
			keepSet[d] = true
		}
	}
	if currentTarget != "" {
		keepSet[currentTarget] = true
	}
	for _, d := range dirs {
		if !keepSet[d] {
			_ = os.RemoveAll(filepath.Join(bundlesDir, d))
		}
	}
}
